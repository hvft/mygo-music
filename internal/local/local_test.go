package local

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mygo-music/internal/music"
)

var (
	jpegData = append([]byte{0xff, 0xd8, 0xff, 0xe0, 0, 0x10, 'J', 'F', 'I', 'F', 0}, bytes.Repeat([]byte{7}, 64)...)
	pngData  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{9}, 64)...)
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "player", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// id3 builds an ID3v2.3 tag of the frames: text ones by name, a TXXX as
// "TXXX:name", USLT, and APIC as a JPEG or "APIC:type".
func id3(frames ...[2]string) []byte {
	var body bytes.Buffer
	frame := func(id string, data []byte) {
		body.WriteString(id)
		binary.Write(&body, binary.BigEndian, uint32(len(data)))
		body.Write([]byte{0, 0})
		body.Write(data)
	}
	for _, f := range frames {
		id, value := f[0], f[1]
		switch {
		case strings.HasPrefix(id, "TXXX:"):
			frame("TXXX", []byte("\x03"+id[5:]+"\x00"+value))
		case id == "USLT":
			frame(id, []byte("\x03eng\x00"+value))
		case id == "APIC":
			frame(id, []byte("\x00image/jpeg\x00\x03\x00"+value))
		case strings.HasPrefix(id, "APIC:"):
			frame("APIC", []byte("\x00"+id[5:]+"\x00\x03\x00"+value))
		default:
			frame(id, []byte("\x03"+value))
		}
	}
	n := body.Len()
	head := []byte{'I', 'D', '3', 3, 0, 0, byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	return append(head, body.Bytes()...)
}

// flacWith adds blocks of comments and, if any, a picture to seek.flac, after
// its STREAMINFO.
func flacWith(t *testing.T, picture []byte, comments ...string) []byte {
	t.Helper()
	src := fixture(t, "seek.flac")
	block := func(kind byte, data []byte) []byte {
		n := len(data)
		return append([]byte{kind, byte(n >> 16), byte(n >> 8), byte(n)}, data...)
	}
	var c bytes.Buffer
	binary.Write(&c, binary.LittleEndian, uint32(4))
	c.WriteString("test")
	binary.Write(&c, binary.LittleEndian, uint32(len(comments)))
	for _, s := range comments {
		binary.Write(&c, binary.LittleEndian, uint32(len(s)))
		c.WriteString(s)
	}
	const afterInfo = 4 + 4 + 34
	out := slices.Clone(src[:afterInfo])
	out = append(out, block(4, c.Bytes())...)
	if picture != nil {
		var p bytes.Buffer
		w := func(v uint32) { binary.Write(&p, binary.BigEndian, v) }
		w(3)
		w(9)
		p.WriteString("image/png")
		w(0)
		w(1)
		w(1)
		w(32)
		w(0)
		w(uint32(len(picture)))
		p.Write(picture)
		out = append(out, block(6, p.Bytes())...)
	}
	return append(out, src[afterInfo:]...)
}

// mp3With prepends an ID3 tag of the frames to a little of seek.mp3, which is
// all that reading its tags and length takes.
func mp3With(t *testing.T, frames ...[2]string) []byte {
	t.Helper()
	return append(id3(frames...), fixture(t, "seek.mp3")[:4096]...)
}

func write(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// wait returns once the source's scans are over.
func wait(t *testing.T, s *Source) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for s.Scanning() {
		if time.Now().After(deadline) {
			t.Fatal("still scanning")
		}
		time.Sleep(time.Millisecond)
	}
}

func rescan(t *testing.T, s *Source) {
	t.Helper()
	s.Rescan()
	wait(t, s)
}

// scanned returns a source with the folder, after scanning it.
func scanned(t *testing.T, folder string) *Source {
	t.Helper()
	s := New(t.TempDir())
	s.AddFolders(folder)
	wait(t, s)
	return s
}

func songs(t *testing.T, s *Source) []music.Track {
	t.Helper()
	b, err := s.Browse("songs")
	if err != nil {
		t.Fatal(err)
	}
	return b.Sections[0].Tracks
}

func songNamed(t *testing.T, s *Source, name string) music.Track {
	t.Helper()
	for _, tr := range songs(t, s) {
		if tr.Name == name {
			return tr
		}
	}
	t.Fatalf("no song %q in %v", name, names(songs(t, s)))
	return music.Track{}
}

func names(tracks []music.Track) []string {
	out := make([]string, len(tracks))
	for i, tr := range tracks {
		out[i] = tr.Name
	}
	return out
}

func ids(tracks []music.Track) []string {
	out := make([]string, len(tracks))
	for i, tr := range tracks {
		out[i] = tr.ID
	}
	return out
}

func albumNamed(t *testing.T, s *Source, name string) []music.Album {
	t.Helper()
	b, err := s.Browse("albums")
	if err != nil {
		t.Fatal(err)
	}
	var out []music.Album
	for _, al := range b.Sections[0].Albums {
		if al.Name == name {
			out = append(out, al)
		}
	}
	return out
}

// The tags of both kinds of file end up on the track.
func TestScanReadsTags(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.mp3"), mp3With(t,
		[2]string{"TIT2", "夜曲"}, [2]string{"TPE1", "周杰伦 / Someone feat. Other"},
		[2]string{"TALB", "十一月的萧邦"}, [2]string{"TPE2", "周杰伦"}, [2]string{"TRCK", "3/12"},
		[2]string{"TPOS", "2/2"}, [2]string{"TYER", "2005"},
		[2]string{"TXXX:REPLAYGAIN_TRACK_GAIN", "-6.5 dB"}, [2]string{"TXXX:REPLAYGAIN_TRACK_PEAK", "0.9"},
	))
	write(t, filepath.Join(dir, "b.flac"), flacWith(t, nil,
		"TITLE=Lossless", "ARTIST=Earth, Wind & Fire", "ALBUM=Gold", "DATE=1978-11-01", "TRACKNUMBER=2",
		"REPLAYGAIN_TRACK_GAIN=+1.25 dB", "REPLAYGAIN_TRACK_PEAK=0.5",
	))
	s := scanned(t, dir)

	mp3 := songNamed(t, s, "夜曲")
	if mp3.Source != ID || mp3.Album.Name != "十一月的萧邦" || mp3.Album.Released != "2005" {
		t.Errorf("mp3 track: %+v", mp3)
	}
	if got := mp3.ArtistNames(); got != "周杰伦, Someone, Other" {
		t.Errorf("artists: %q", got)
	}
	for _, a := range mp3.Artists {
		if a.ID == "" {
			t.Errorf("artist without an ID: %+v", a)
		}
	}
	st, err := s.Stream(mp3.ID, "lossless")
	if err != nil || st.Type != "mp3" || st.Quality != "high" || st.Gain != -6.5 || st.Peak != 0.9 {
		t.Errorf("mp3 stream: %+v, %v", st, err)
	}

	flac := songNamed(t, s, "Lossless")
	if len(flac.Artists) != 1 || flac.Artists[0].Name != "Earth, Wind & Fire" || flac.Album.Released != "1978" {
		t.Errorf("flac track: %+v", flac)
	}
	st, err = s.Stream(flac.ID, "standard")
	if err != nil || st.Type != "flac" || st.Quality != "lossless" || st.Gain != 1.25 || st.Peak != 0.5 {
		t.Errorf("flac stream: %+v, %v", st, err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "b.flac")); st.Size != info.Size() {
		t.Errorf("flac size: %d", st.Size)
	}
	if path, ok := music.PathOf(st.URL); !ok || path != filepath.Join(dir, "b.flac") {
		t.Errorf("flac URL: %q", st.URL)
	}
}

// A FLAC behind an ID3 tag still reads as the FLAC it is.
func TestFLACBehindID3(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "x.flac"), append(id3([2]string{"TIT2", "from id3"}), flacWith(t, nil, "TITLE=from flac")...))
	s := scanned(t, dir)
	tr := songNamed(t, s, "from flac")
	if tr.Duration != 5000 {
		t.Errorf("duration: %d", tr.Duration)
	}
}

// A file without a title is named from its file name, which may give the artist.
func TestNameFromFile(t *testing.T) {
	dir := t.TempDir()
	audio := fixture(t, "seek.mp3")[:4096]
	write(t, filepath.Join(dir, "The Band - The Song - Live.mp3"), audio)
	write(t, filepath.Join(dir, "01 - Intro.MP3"), audio)
	write(t, filepath.Join(dir, "Plain.mp3"), audio)
	write(t, filepath.Join(dir, "Tagged - Name.mp3"), mp3With(t, [2]string{"TPE1", "From Tag"}))
	s := scanned(t, dir)

	for name, artist := range map[string]string{
		"The Song - Live": "The Band",
		"01 - Intro":      "",
		"Plain":           "",
		"Name":            "From Tag",
	} {
		tr := songNamed(t, s, name)
		if got := tr.ArtistNames(); got != artist {
			t.Errorf("%q: artist %q, want %q", name, got, artist)
		}
		if tr.Album.ID != "" || tr.Album.Name != "" {
			t.Errorf("%q: album %+v", name, tr.Album)
		}
	}
}

// A file whose tags can't be made out is listed all the same, and doesn't stop the scan.
func TestMalformedTags(t *testing.T) {
	dir := t.TempDir()
	// A frame says it's longer than what's left of the file, another has no text.
	broken := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0x7f, 0x7f, 'T', 'I', 'T', '2', 0xff, 0xff, 0xff, 0xff, 0, 0, 1}
	write(t, filepath.Join(dir, "broken.mp3"), broken)
	write(t, filepath.Join(dir, "noise.flac"), []byte("fLaC not really"))
	write(t, filepath.Join(dir, "short.mp3"), []byte{1})
	write(t, filepath.Join(dir, "fine.mp3"), mp3With(t, [2]string{"TIT2", "Fine"}))
	s := scanned(t, dir)
	if got := s.TrackCount(); got != 4 {
		t.Fatalf("%d tracks: %v", got, names(songs(t, s)))
	}
	songNamed(t, s, "Fine")
	songNamed(t, s, "broken")
	songNamed(t, s, "noise")
}

// Durations come from the headers, without decoding.
func TestDurations(t *testing.T) {
	mp3 := fixture(t, "seek.mp3")
	flac := fixture(t, "seek.flac")
	frame, ok := parseFrameHeader(mp3)
	if !ok {
		t.Fatal("no frame at the start of seek.mp3")
	}
	audio := mp3[frame.length:] // the same with no Info frame: nothing says how many there are
	vbri := slices.Clone(mp3)
	copy(vbri[36:], "VBRI\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x64") // 100 frames

	tests := []struct {
		name     string
		data     []byte
		flac     bool
		from, to int64
	}{
		{name: "flac", data: flac, flac: true, from: 5000, to: 5000},
		{name: "flac behind ID3", data: append(id3([2]string{"TIT2", "x"}), flac...), flac: true, from: 5000, to: 5000},
		{name: "not flac", data: []byte("nothing of the kind, not at all"), flac: true},
		{name: "Info frame", data: mp3, from: 5000, to: 5060},
		{name: "ID3 first", data: append(id3([2]string{"TIT2", "x"}), mp3...), from: 5000, to: 5060},
		{name: "padding after ID3", data: append(append(id3([2]string{"TIT2", "x"}), make([]byte, 1000)...), mp3...), from: 5000, to: 5060},
		{name: "VBRI", data: vbri, from: 2610, to: 2613},
		{name: "constant bit rate", data: audio, from: 5000, to: 5060},
		{name: "ID3v1 not counted", data: append(slices.Clone(audio), append([]byte("TAG"), make([]byte, 125)...)...), from: 5000, to: 5060},
		{name: "not mp3", data: bytes.Repeat([]byte("noise"), 100)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got int64
			r := bytes.NewReader(tt.data)
			if tt.flac {
				got = flacDuration(r)
			} else {
				got = mp3Duration(r, int64(len(tt.data)))
			}
			if got < tt.from || got > tt.to {
				t.Errorf("%d ms, want %d to %d", got, tt.from, tt.to)
			}
		})
	}
}

// A chance 0xff before the audio isn't taken for its first frame.
func TestFalseSyncBeforeAudio(t *testing.T) {
	mp3 := fixture(t, "seek.mp3")
	data := append([]byte{0xff, 0xfb, 0x10, 0x00, 0, 0, 0, 0}, mp3...)
	if got := mp3Duration(bytes.NewReader(data), int64(len(data))); got < 5000 || got > 5060 {
		t.Errorf("%d ms", got)
	}
}

// Artists split on what separates them in a tag, and not on commas.
func TestSplitArtists(t *testing.T) {
	for tag, want := range map[string][]string{
		"A / B":             {"A", "B"},
		"A;B; C":            {"A", "B", "C"},
		"甲、乙":               {"甲", "乙"},
		"A FEAT. B":         {"A", "B"},
		"A Ft. B feat. C":   {"A", "B", "C"},
		"AC/DC":             {"AC/DC"},
		"Earth, Wind, Fire": {"Earth, Wind, Fire"},
		"A / a":             {"A"},
		" ; ":               nil,
		"Daft Punk":         {"Daft Punk"},
		"Left feat Right":   {"Left feat Right"},
	} {
		if got := splitArtists(tag); !slices.Equal(got, want) {
			t.Errorf("%q: %q, want %q", tag, got, want)
		}
	}
}

// Albums group by album artist wherever the files are, and by folder without
// one, in order of disc and track, and credit who they should.
func TestAlbumsAndArtists(t *testing.T) {
	dir := t.TempDir()
	tag := func(title, artist, album, albumArtist, track, disc string) []byte {
		frames := [][2]string{{"TIT2", title}, {"TRCK", track}, {"TPOS", disc}}
		if artist != "" {
			frames = append(frames, [2]string{"TPE1", artist})
		}
		if album != "" {
			frames = append(frames, [2]string{"TALB", album})
		}
		if albumArtist != "" {
			frames = append(frames, [2]string{"TPE2", albumArtist})
		}
		return mp3With(t, frames...)
	}
	// By album artist, over two folders, with its name in two spellings.
	write(t, filepath.Join(dir, "cd1", "b.mp3"), tag("Second", "Guest", "Spread", "Main", "1", "2"))
	write(t, filepath.Join(dir, "cd2", "a.mp3"), tag("First", "Main", "SPREAD", "MAIN", "2", "1"))
	write(t, filepath.Join(dir, "cd2", "c.mp3"), tag("Very First", "Main", "Spread", "Main", "1", "1"))
	// The same name in two folders, without one: two albums.
	write(t, filepath.Join(dir, "x", "1.mp3"), tag("X1", "Solo", "Hits", "", "1", "1"))
	write(t, filepath.Join(dir, "y", "1.mp3"), tag("Y1", "Other", "Hits", "", "1", "1"))
	// Different artists in a folder, without one: various.
	write(t, filepath.Join(dir, "z", "1.mp3"), tag("Z1", "One", "Mix", "", "1", "1"))
	write(t, filepath.Join(dir, "z", "2.mp3"), tag("Z2", "Two", "Mix", "", "2", "1"))
	// No album at all.
	write(t, filepath.Join(dir, "single.mp3"), tag("Single", "One", "", "", "1", "1"))
	s := scanned(t, dir)

	spread := albumNamed(t, s, "Spread")
	if len(spread) == 0 {
		spread = albumNamed(t, s, "SPREAD")
	}
	if len(spread) != 1 {
		t.Fatalf("Spread: %+v", spread)
	}
	page, err := s.Album(spread[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(page.Tracks); !slices.Equal(got, []string{"Very First", "First", "Second"}) {
		t.Errorf("Spread's tracks: %q", got)
	}
	if len(page.Album.Artists) != 1 || !strings.EqualFold(page.Album.Artists[0].Name, "Main") || page.Album.Artists[0].ID == "" {
		t.Errorf("Spread's artists: %+v", page.Album.Artists)
	}

	if hits := albumNamed(t, s, "Hits"); len(hits) != 2 {
		t.Errorf("Hits: %+v", hits)
	} else if hits[0].Artists[0].Name == hits[1].Artists[0].Name {
		t.Errorf("Hits' artists: %+v", hits)
	}

	mix := albumNamed(t, s, "Mix")
	if len(mix) != 1 || len(mix[0].Artists) != 1 || mix[0].Artists[0].Name != "Various Artists" || mix[0].Artists[0].ID != "" {
		t.Errorf("Mix: %+v", mix)
	}
	if tr := songNamed(t, s, "Z1"); tr.Album.Artists[0].Name != "Various Artists" {
		t.Errorf("Z1's album: %+v", tr.Album)
	}

	if tr := songNamed(t, s, "Single"); tr.Album.ID != "" {
		t.Errorf("Single's album: %+v", tr.Album)
	}

	// One's on an album of many and on no album: the album first.
	one, err := s.Artist(songNamed(t, s, "Single").Artists[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(one.Tracks); !slices.Equal(got, []string{"Z1", "Single"}) {
		t.Errorf("One's tracks: %q", got)
	}
	if len(one.Albums) != 1 || one.Albums[0].Name != "Mix" {
		t.Errorf("One's albums: %+v", one.Albums)
	}

	// Guest is only on a track of it, and is credited with the album all the same.
	guest, err := s.Artist(songNamed(t, s, "Second").Artists[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if guest.Artist.Name != "Guest" || len(guest.Albums) != 1 || len(guest.Tracks) != 1 {
		t.Errorf("Guest: %+v", guest)
	}

	if _, err := s.Album("nope"); err == nil {
		t.Error("unknown album: no error")
	}
	if _, err := s.Artist("nope"); err == nil {
		t.Error("unknown artist: no error")
	}
}

// IDs don't change from one scan to the next, or from one source to another of
// the same files: playlists and likes hang on them.
func TestStableIDs(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.mp3"), mp3With(t, [2]string{"TIT2", "A"}, [2]string{"TPE1", "Art"}, [2]string{"TALB", "Alb"}))
	one, two := scanned(t, dir), scanned(t, dir)
	a, b := songNamed(t, one, "A"), songNamed(t, two, "A")
	if a.ID != b.ID || a.Album.ID != b.Album.ID || a.Artists[0].ID != b.Artists[0].ID {
		t.Errorf("%+v and %+v", a, b)
	}
	if a.ID != makeID("track", filepath.Join(dir, "a.mp3")) || len(a.ID) != 16 {
		t.Errorf("track ID %q", a.ID)
	}
	if a.Artists[0].ID != makeID("artist", "art") {
		t.Errorf("artist ID %q", a.Artists[0].ID)
	}
}

// A file of the same size and time isn't read again, whatever's in it now,
// even by a new source; touching it is what makes it read.
func TestRescanReusesIndex(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	path := write(t, filepath.Join(dir, "a.mp3"), mp3With(t, [2]string{"TIT2", "Old"}))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(data)
	s.AddFolders(dir)
	wait(t, s)
	songNamed(t, s, "Old")

	if err := os.WriteFile(path, mp3With(t, [2]string{"TIT2", "New"}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	rescan(t, s)
	songNamed(t, s, "Old")

	again := New(data)
	if got := again.TrackCount(); got != 1 {
		t.Fatalf("listed before scanning: %d", got)
	}
	rescan(t, again)
	songNamed(t, again, "Old")

	later := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	rescan(t, again)
	songNamed(t, again, "New")
}

// Only music outside hidden folders counts, whatever the case of its extension.
func TestWalkFinds(t *testing.T) {
	dir := t.TempDir()
	audio := fixture(t, "seek.mp3")[:4096]
	write(t, filepath.Join(dir, "deep", "er", "Upper.MP3"), audio)
	write(t, filepath.Join(dir, "lower.flac"), fixture(t, "seek.flac"))
	write(t, filepath.Join(dir, ".hidden", "no.mp3"), audio)
	write(t, filepath.Join(dir, "._lower.mp3"), audio)
	write(t, filepath.Join(dir, "no.wma"), audio)
	write(t, filepath.Join(dir, "empty.mp3"), nil)
	s := scanned(t, dir)
	got := names(songs(t, s))
	if !slices.Equal(got, []string{"lower", "Upper"}) {
		t.Errorf("found %q", got)
	}
}

// A folder that's a link is walked, and tracks keep the path through it.
func TestFolderIsALink(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "real", "song.mp3"), fixture(t, "seek.mp3")[:4096])
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "real"), link); err != nil {
		t.Skip(err)
	}
	s := scanned(t, link)
	st, err := s.Stream(songNamed(t, s, "song").ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if path, _ := music.PathOf(st.URL); path != filepath.Join(link, "song.mp3") {
		t.Errorf("path %q", path)
	}
}

// Folders are kept cleaned and once each, and a folder taken away takes its
// tracks with it, while playlists and likes keep them for its return.
func TestFoldersComeAndGo(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	one := filepath.Join(root, "one")
	two := filepath.Join(root, "two")
	write(t, filepath.Join(one, "a.mp3"), mp3With(t, [2]string{"TIT2", "A"}))
	write(t, filepath.Join(two, "b.mp3"), mp3With(t, [2]string{"TIT2", "B"}))

	s := New(data)
	s.AddFolders(one, one+string(filepath.Separator), two, filepath.Join(two, "x", ".."))
	wait(t, s)
	if got := s.Folders(); !slices.Equal(got, []string{one, two}) {
		t.Fatalf("folders: %q", got)
	}
	a, b := songNamed(t, s, "A"), songNamed(t, s, "B")
	list, err := s.CreatePlaylist("Mix")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddToPlaylist(list.ID, []string{a.ID, b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.Like(b.ID, true); err != nil {
		t.Fatal(err)
	}

	s.RemoveFolder(two)
	wait(t, s)
	if got := s.TrackCount(); got != 1 {
		t.Fatalf("%d tracks with the folder gone", got)
	}
	p, err := s.Playlist(list.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p.Tracks); !slices.Equal(got, []string{"A"}) || p.TrackCount != 1 {
		t.Errorf("playlist with B away: %q, count %d", got, p.TrackCount)
	}
	if liked, _ := s.Liked(0); len(liked.Tracks) != 0 {
		t.Errorf("liked with B away: %q", names(liked.Tracks))
	}
	if on, _ := s.IsLiked([]string{b.ID}); !on[b.ID] {
		t.Error("B's like is gone")
	}

	// Not read from the index either, next time.
	if got := New(data).TrackCount(); got != 1 {
		t.Errorf("%d tracks in the index", got)
	}

	s.AddFolders(two)
	wait(t, s)
	p, err = s.Playlist(list.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(p.Tracks); !slices.Equal(got, []string{"A", "B"}) {
		t.Errorf("playlist with B back: %q", got)
	}
	if liked, _ := s.Liked(0); len(liked.Tracks) != 1 {
		t.Errorf("liked with B back: %q", names(liked.Tracks))
	}
}

// A deleted file leaves on the next scan, and returns with the file.
func TestFileGoesAndComesBack(t *testing.T) {
	dir := t.TempDir()
	content := mp3With(t, [2]string{"TIT2", "Away"})
	path := write(t, filepath.Join(dir, "a.mp3"), content)
	write(t, filepath.Join(dir, "b.mp3"), mp3With(t, [2]string{"TIT2", "Here"}))
	s := scanned(t, dir)
	away := songNamed(t, s, "Away")
	list, _ := s.CreatePlaylist("P")
	s.AddToPlaylist(list.ID, []string{away.ID})
	s.Played(away)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rescan(t, s)
	if got := s.TrackCount(); got != 1 {
		t.Fatalf("%d tracks", got)
	}
	if recent, _ := s.Recent(); len(recent) != 0 {
		t.Errorf("recent: %q", names(recent))
	}
	if ps, _ := s.Playlists(); ps[0].TrackCount != 0 {
		t.Errorf("playlist count: %d", ps[0].TrackCount)
	}

	write(t, path, content)
	rescan(t, s)
	p, err := s.Playlist(list.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p.Tracks); !slices.Equal(got, []string{away.ID}) {
		t.Errorf("playlist: %q", got)
	}
	if recent, _ := s.Recent(); len(recent) != 1 {
		t.Errorf("recent: %q", names(recent))
	}
}

// A move goes by what the playlist lists, the tracks away keeping their places
// among the others: the app knows only those.
func TestMoveAroundATrackAway(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.mp3"), mp3With(t, [2]string{"TIT2", "A"}))
	path := write(t, filepath.Join(dir, "b.mp3"), mp3With(t, [2]string{"TIT2", "Away"}))
	write(t, filepath.Join(dir, "c.mp3"), mp3With(t, [2]string{"TIT2", "C"}))
	s := scanned(t, dir)
	a, away, c := songNamed(t, s, "A"), songNamed(t, s, "Away"), songNamed(t, s, "C")
	list, _ := s.CreatePlaylist("P")
	s.AddToPlaylist(list.ID, []string{a.ID, away.ID, c.ID})
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	rescan(t, s)
	if err := s.MovePlaylistTrack(list.ID, 0, 1); err != nil {
		t.Fatal(err)
	}
	p, _ := s.Playlist(list.ID)
	if got := ids(p.Tracks); !slices.Equal(got, []string{c.ID, a.ID}) {
		t.Errorf("listed %q, want C, A", got)
	}
	if err := s.MovePlaylistTrack(list.ID, 0, 2); err == nil {
		t.Error("moved past the end of what it lists")
	}
}

// Every word must be found, in any of a track's fields, whatever the case.
func TestSearch(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.mp3"), mp3With(t, [2]string{"TIT2", "Blue Moon"}, [2]string{"TPE1", "Frank"}, [2]string{"TALB", "Night"}))
	write(t, filepath.Join(dir, "b.mp3"), mp3With(t, [2]string{"TIT2", "Blue Sky"}, [2]string{"TPE1", "Ella"}, [2]string{"TALB", "Day"}))
	s := scanned(t, dir)

	for query, want := range map[string][]string{
		"blue":         {"Blue Moon", "Blue Sky"},
		"BLUE  frank":  {"Blue Moon"},
		"ella day sky": {"Blue Sky"},
		"night ella":   nil,
		"":             {"Blue Moon", "Blue Sky"},
		"   ":          {"Blue Moon", "Blue Sky"},
	} {
		res, err := s.Search(query, music.SearchTracks, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(res.Tracks); !slices.Equal(got, want) && len(got)+len(want) > 0 {
			t.Errorf("%q: %q, want %q", query, got, want)
		}
	}

	if res, _ := s.Search("NIG", music.SearchAlbums, 0); len(res.Albums) != 1 || res.Albums[0].Name != "Night" {
		t.Errorf("albums: %+v", res.Albums)
	}
	if res, _ := s.Search("frank", music.SearchArtists, 0); len(res.Artists) != 1 || res.Artists[0].Name != "Frank" {
		t.Errorf("artists: %+v", res.Artists)
	}
	s.CreatePlaylist("Road Trip")
	s.CreatePlaylist("Gym")
	if res, _ := s.Search("road", music.SearchPlaylists, 0); len(res.Playlists) != 1 || res.Playlists[0].Name != "Road Trip" {
		t.Errorf("playlists: %+v", res.Playlists)
	}
	if res, err := s.Search("x", music.SearchShows, 0); err != nil || res.Len() != 0 {
		t.Errorf("shows: %+v, %v", res, err)
	}
}

// Results and likes come a page at a time, saying whether more follow.
func TestSearchPages(t *testing.T) {
	dir := t.TempDir()
	audio := fixture(t, "seek.mp3")[:256]
	for i := range pageSize + 5 {
		write(t, filepath.Join(dir, fmt.Sprintf("Song %03d.mp3", i)), audio)
	}
	s := scanned(t, dir)

	first, err := s.Search("song", music.SearchTracks, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tracks) != pageSize || !first.More || first.Tracks[0].Name != "Song 000" {
		t.Errorf("first page: %d, more %v", len(first.Tracks), first.More)
	}
	second, _ := s.Search("song", music.SearchTracks, pageSize)
	if len(second.Tracks) != 5 || second.More || second.Tracks[4].Name != "Song 204" {
		t.Errorf("second page: %d, more %v", len(second.Tracks), second.More)
	}
	if past, _ := s.Search("song", music.SearchTracks, 10000); len(past.Tracks) != 0 || past.More {
		t.Errorf("past the end: %+v", past)
	}

	// Likes page the same way, with the newest first.
	for _, tr := range songs(t, s) {
		s.Like(tr.ID, true)
	}
	liked, err := s.Liked(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(liked.Tracks) != pageSize || !liked.More || liked.Tracks[0].Name != "Song 204" {
		t.Errorf("liked: %d, more %v, first %q", len(liked.Tracks), liked.More, liked.Tracks[0].Name)
	}
	if liked, _ := s.Liked(pageSize); len(liked.Tracks) != 5 || liked.More || liked.Tracks[4].Name != "Song 000" {
		t.Errorf("liked's last page: %d", len(liked.Tracks))
	}
}

// Playlists and likes outlive the source, and edits land where they should.
func TestLibraryPersists(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	for _, name := range []string{"A", "B", "C"} {
		write(t, filepath.Join(dir, name+".mp3"), mp3With(t, [2]string{"TIT2", name}))
	}
	s := New(data)
	s.AddFolders(dir)
	wait(t, s)
	a, b, c := songNamed(t, s, "A").ID, songNamed(t, s, "B").ID, songNamed(t, s, "C").ID

	list, err := s.CreatePlaylist("First")
	if err != nil || !list.Mine || len(list.ID) != 16 {
		t.Fatalf("created %+v, %v", list, err)
	}
	other, _ := s.CreatePlaylist("Other")
	if err := s.AddToPlaylist(list.ID, []string{a, b, c, a}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddToPlaylist(list.ID, []string{b}); err != nil {
		t.Errorf("adding what's there: %v", err)
	}
	if err := s.MovePlaylistTrack(list.ID, 2, 0); err != nil { // C A B
		t.Fatal(err)
	}
	if err := s.MovePlaylistTrack(list.ID, 0, 1); err != nil { // A C B
		t.Fatal(err)
	}
	for _, bad := range [][2]int{{-1, 0}, {0, 3}, {3, 0}} {
		if err := s.MovePlaylistTrack(list.ID, bad[0], bad[1]); err == nil {
			t.Errorf("moving %v: no error", bad)
		}
	}
	if err := s.RenamePlaylist(list.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePlaylist(other.ID); err != nil {
		t.Fatal(err)
	}
	s.Like(a, true)
	s.Like(b, true)
	s.Like(a, true) // no change: it stays where it was
	s.Like(c, true)
	s.Like(b, false)

	again := New(data)
	if got := again.Folders(); !slices.Equal(got, []string{dir}) {
		t.Errorf("folders: %q", got)
	}
	lists, err := again.Playlists()
	if err != nil || len(lists) != 1 {
		t.Fatalf("playlists: %+v, %v", lists, err)
	}
	if l := lists[0]; l.Name != "Renamed" || !l.Mine || l.TrackCount != 3 || l.Tracks != nil {
		t.Errorf("playlist: %+v", l)
	}
	p, err := again.Playlist(list.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p.Tracks); !slices.Equal(got, []string{a, c, b}) {
		t.Errorf("order: %q, want %q", got, []string{a, c, b})
	}
	if err := again.RemoveFromPlaylist(list.ID, []string{c}); err != nil {
		t.Fatal(err)
	}
	if p, _ := New(data).Playlist(list.ID); !slices.Equal(ids(p.Tracks), []string{a, b}) {
		t.Errorf("after removing: %q", ids(p.Tracks))
	}

	liked, err := again.Liked(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(liked.Tracks); !slices.Equal(got, []string{c, a}) {
		t.Errorf("liked: %q, want %q", got, []string{c, a})
	}
	on, _ := again.IsLiked([]string{a, b})
	if !on[a] || on[b] {
		t.Errorf("isLiked: %v", on)
	}

	for name, err := range map[string]error{
		"playlist": func() error { _, err := again.Playlist("nope"); return err }(),
		"rename":   again.RenamePlaylist("nope", "x"),
		"delete":   again.DeletePlaylist("nope"),
		"add":      again.AddToPlaylist("nope", []string{a}),
		"remove":   again.RemoveFromPlaylist("nope", []string{a}),
		"move":     again.MovePlaylistTrack("nope", 0, 0),
	} {
		if err == nil {
			t.Errorf("%s of an unknown playlist: no error", name)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(data, "*.tmp")); len(left) != 0 {
		t.Errorf("left behind: %q", left)
	}
}

// Lyrics come from beside the file, then from its tags if they're timed.
func TestLyrics(t *testing.T) {
	dir := t.TempDir()
	const timed = "[00:01.00]line one"
	write(t, filepath.Join(dir, "side.mp3"), mp3With(t, [2]string{"TIT2", "Side"}, [2]string{"USLT", "[00:09.00]tag loses"}))
	write(t, filepath.Join(dir, "side.lrc"), []byte("\xef\xbb\xbf"+timed))
	write(t, filepath.Join(dir, "tag.mp3"), mp3With(t, [2]string{"TIT2", "Tag"}, [2]string{"USLT", timed}))
	write(t, filepath.Join(dir, "plain.mp3"), mp3With(t, [2]string{"TIT2", "Plain"}, [2]string{"USLT", "just words"}))
	write(t, filepath.Join(dir, "vorbis.flac"), flacWith(t, nil, "TITLE=Vorbis", "LYRICS="+timed))
	write(t, filepath.Join(dir, "none.mp3"), mp3With(t, [2]string{"TIT2", "None"}))
	s := scanned(t, dir)

	for name, want := range map[string]string{"Side": timed, "Tag": timed, "Vorbis": timed, "Plain": "", "None": ""} {
		l, err := s.Lyrics(songNamed(t, s, name).ID)
		if err != nil || l == nil || l.LRC != want {
			t.Errorf("%s: %+v, %v", name, l, err)
		}
	}
	if _, err := s.Lyrics("nope"); !errors.Is(err, music.ErrUnavailable) {
		t.Errorf("unknown track: %v", err)
	}
}

// Pictures in files are kept once, and those beside files are found, whichever
// way they're spelled; albums, artists and playlists take their tracks' pictures.
func TestCovers(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	withPicture := func(title string) []byte {
		return mp3With(t, [2]string{"TIT2", title}, [2]string{"TPE1", "Art"}, [2]string{"TALB", "Pics"}, [2]string{"TPE2", "Art"}, [2]string{"APIC", string(jpegData)})
	}
	// The first of the album has no picture: the album takes the second's.
	write(t, filepath.Join(dir, "one", "0.mp3"), mp3With(t, [2]string{"TIT2", "Bare"}, [2]string{"TALB", "Pics"}, [2]string{"TPE2", "Art"}, [2]string{"TRCK", "1"}))
	write(t, filepath.Join(dir, "one", "1.mp3"), append(withPicture("Pic1"), 0))
	write(t, filepath.Join(dir, "one", "2.mp3"), withPicture("Pic2"))
	write(t, filepath.Join(dir, "flac", "f.flac"), flacWith(t, pngData, "TITLE=FlacPic"))
	sidecar := write(t, filepath.Join(dir, "two", "FOLDER.PNG"), pngData)
	write(t, filepath.Join(dir, "two", "album.jpg"), jpegData) // a lesser name
	write(t, filepath.Join(dir, "two", "s.mp3"), mp3With(t, [2]string{"TIT2", "Side"}))
	write(t, filepath.Join(dir, "three", "n.mp3"), mp3With(t, [2]string{"TIT2", "NoPic"}))

	s := New(data)
	s.AddFolders(dir)
	wait(t, s)

	kept, _ := filepath.Glob(filepath.Join(data, "covers", "*"))
	if len(kept) != 2 {
		t.Errorf("covers kept: %q", kept)
	}
	check := func(url string, want []byte) {
		t.Helper()
		got, err := music.Fetch(url)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%q: %d bytes, %v", url, len(got), err)
		}
	}
	pic := songNamed(t, s, "Pic2")
	check(pic.Album.ImageURL, jpegData)
	if path, _ := music.PathOf(pic.Album.ImageURL); !strings.HasSuffix(path, ".jpg") || filepath.Dir(path) != filepath.Join(data, "covers") {
		t.Errorf("cover path: %q", path)
	}
	flac := songNamed(t, s, "FlacPic").Album.ImageURL
	check(flac, pngData)
	if !strings.HasSuffix(flac, ".png") {
		t.Errorf("flac cover: %q", flac)
	}
	if got := songNamed(t, s, "Side").Album.ImageURL; got != music.FileURL(sidecar) {
		t.Errorf("sidecar: %q", got)
	}
	if got := songNamed(t, s, "NoPic").Album.ImageURL; got != "" {
		t.Errorf("no picture: %q", got)
	}

	albums := albumNamed(t, s, "Pics")
	if len(albums) != 1 || albums[0].ImageURL != pic.Album.ImageURL {
		t.Errorf("album: %+v", albums)
	}
	if got := songNamed(t, s, "Bare").Album.ImageURL; got != pic.Album.ImageURL {
		t.Errorf("track without its own picture: %q", got)
	}
	if a, _ := s.Artist(pic.Artists[0].ID); a == nil || a.Artist.ImageURL != pic.Album.ImageURL {
		t.Errorf("artist: %+v", a)
	}

	list, _ := s.CreatePlaylist("P")
	s.AddToPlaylist(list.ID, []string{songNamed(t, s, "NoPic").ID, songNamed(t, s, "Side").ID})
	if ps, _ := s.Playlists(); ps[0].ImageURL != music.FileURL(sidecar) {
		t.Errorf("playlist: %+v", ps[0])
	}

	// A cover added later is seen without the file changing, and one kept that
	// has gone is written again.
	added := write(t, filepath.Join(dir, "three", "Cover.jpeg"), jpegData)
	for _, path := range kept {
		os.Remove(path)
	}
	rescan(t, s)
	if got := songNamed(t, s, "NoPic").Album.ImageURL; got != music.FileURL(added) {
		t.Errorf("added sidecar: %q", got)
	}
	check(songNamed(t, s, "Pic2").Album.ImageURL, jpegData)
}

// A picture goes by what it holds over what it says it is, and one of no kind
// the app can show is no cover, for a sidecar to stand in.
func TestPictureKinds(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a", "a.mp3"), mp3With(t, [2]string{"TIT2", "Odd"}, [2]string{"APIC:image/tiff", "not a kind we show"}))
	side := write(t, filepath.Join(dir, "a", "cover.jpg"), jpegData)
	write(t, filepath.Join(dir, "b", "b.mp3"), mp3With(t, [2]string{"TIT2", "Mislabelled"}, [2]string{"APIC:image/jpeg", string(pngData)}))
	s := scanned(t, dir)
	if got := songNamed(t, s, "Odd").Album.ImageURL; got != music.FileURL(side) {
		t.Errorf("odd picture: %q", got)
	}
	if got := songNamed(t, s, "Mislabelled").Album.ImageURL; !strings.HasSuffix(got, ".png") {
		t.Errorf("mislabelled picture: %q", got)
	}
}

// A file gone since the scan can't be played, as a track never known can't.
func TestStreamMissing(t *testing.T) {
	dir := t.TempDir()
	path := write(t, filepath.Join(dir, "a.mp3"), mp3With(t, [2]string{"TIT2", "A"}))
	s := scanned(t, dir)
	id := songNamed(t, s, "A").ID
	if _, err := s.Stream(id, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stream(id, ""); !errors.Is(err, music.ErrUnavailable) {
		t.Errorf("gone: %v", err)
	}
	if _, err := s.Stream("nope", ""); !errors.Is(err, music.ErrUnavailable) {
		t.Errorf("unknown: %v", err)
	}
}

// A file named wrongly plays as what it is.
func TestStreamTypeByContent(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "really-flac.mp3"), fixture(t, "seek.flac"))
	write(t, filepath.Join(dir, "really-mp3.flac"), fixture(t, "seek.mp3"))
	s := scanned(t, dir)
	for name, want := range map[string]string{"really-flac": "flac", "really-mp3": "mp3"} {
		tr := songNamed(t, s, name)
		st, err := s.Stream(tr.ID, "")
		if err != nil || st.Type != want {
			t.Errorf("%s: %+v, %v", name, st, err)
		}
		if tr.Duration < 5000 || tr.Duration > 5060 {
			t.Errorf("%s: %d ms", name, tr.Duration)
		}
	}
}

func sectionTitles(b *music.Browse) []string {
	titles := make([]string, len(b.Sections))
	for i, sec := range b.Sections {
		titles[i] = sec.Title
	}
	return titles
}

// Home has nothing without music, for the app to show its own empty state, and
// offers new songs only until something has been played.
func TestHome(t *testing.T) {
	s := New(t.TempDir())
	if h, err := s.Home(); err != nil || len(h.Sections) != 0 {
		t.Errorf("empty: %+v, %v", h, err)
	}

	list, _ := s.CreatePlaylist("Only a playlist")
	if h, _ := s.Home(); !slices.Equal(sectionTitles(h), []string{"Playlists"}) {
		t.Errorf("playlist only: %q", sectionTitles(h))
	}
	s.DeletePlaylist(list.ID)

	dir := t.TempDir()
	old := time.Now().Add(-time.Hour)
	for i := range homeRows + 2 {
		path := write(t, filepath.Join(dir, fmt.Sprintf("s%02d.mp3", i)), mp3With(t,
			[2]string{"TIT2", fmt.Sprintf("Song %02d", i)}, [2]string{"TALB", fmt.Sprintf("Album %02d", i)}))
		when := old.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	s.AddFolders(dir)
	wait(t, s)

	h, err := s.Home()
	if err != nil {
		t.Fatal(err)
	}
	if got := sectionTitles(h); !slices.Equal(got, []string{"Albums", "Songs"}) {
		t.Fatalf("not played yet: %q", got)
	}
	if sec := h.Sections[0]; len(sec.Albums) != homeRows || sec.MorePage != "albums" {
		t.Errorf("albums: %d, %q", len(sec.Albums), sec.MorePage)
	}
	sec := h.Sections[1]
	if len(sec.Tracks) != homeRows || sec.MorePage != "songs" || sec.Tracks[0].Name != fmt.Sprintf("Song %02d", homeRows+1) {
		t.Errorf("songs: %q, %q", names(sec.Tracks), sec.MorePage)
	}

	s.Played(songNamed(t, s, "Song 03"))
	s.CreatePlaylist("Now")
	h, _ = s.Home()
	if got := sectionTitles(h); !slices.Equal(got, []string{"Recently Played", "Albums", "Playlists"}) {
		t.Fatalf("played: %q", got)
	}
	if got := names(h.Sections[0].Tracks); !slices.Equal(got, []string{"Song 03"}) {
		t.Errorf("recent: %q", got)
	}
}

// The pages hold everything, in order of name whatever the case, and tell what
// they are; a page that doesn't exist is an error.
func TestBrowse(t *testing.T) {
	dir := t.TempDir()
	for i, title := range []string{"beta", "Alpha", "Gamma"} {
		write(t, filepath.Join(dir, fmt.Sprintf("%d.mp3", i)), mp3With(t,
			[2]string{"TIT2", title}, [2]string{"TALB", title + " album"}, [2]string{"TPE1", title + " artist"}))
	}
	s := scanned(t, dir)

	b, err := s.Browse("songs")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(b.Sections[0].Tracks); !slices.Equal(got, []string{"Alpha", "beta", "Gamma"}) || b.Title != "Songs" || b.Subtitle != "3 songs" {
		t.Errorf("songs: %q, %+v", got, b)
	}

	b, err = s.Browse("albums")
	if err != nil {
		t.Fatal(err)
	}
	if al := b.Sections[0].Albums; len(al) != 3 || al[0].Name != "Alpha album" || al[1].Name != "beta album" {
		t.Errorf("albums: %+v", al)
	}

	b, err = s.Browse("artists")
	if err != nil {
		t.Fatal(err)
	}
	if ar := b.Sections[0].Artists; len(ar) != 3 || ar[0].Name != "Alpha artist" || ar[1].Name != "beta artist" {
		t.Errorf("artists: %+v", ar)
	}

	if _, err := s.Browse("nope"); err == nil {
		t.Error("unknown page: no error")
	}
}

// Playing moves a track to the front; what's kept and what's shown are capped.
func TestRecent(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	for _, name := range []string{"A", "B", "C"} {
		write(t, filepath.Join(dir, name+".mp3"), mp3With(t, [2]string{"TIT2", name}))
	}
	s := New(data)
	s.AddFolders(dir)
	wait(t, s)
	for _, name := range []string{"A", "B", "C", "A"} {
		s.Played(songNamed(t, s, name))
	}
	s.Played(music.Track{}) // nothing to go by

	recent, err := New(data).Recent()
	if err != nil {
		t.Fatal(err)
	}
	if got := names(recent); !slices.Equal(got, []string{"A", "C", "B"}) {
		t.Errorf("recent: %q", got)
	}

	for i := range keepRecent + 10 {
		s.Played(music.Track{ID: fmt.Sprintf("away%d", i)})
	}
	s.mu.RLock()
	kept := len(s.lib.Recent)
	s.mu.RUnlock()
	if kept != keepRecent {
		t.Errorf("kept %d", kept)
	}
}

// Recent answers at most its limit, of the tracks on disk.
func TestRecentLimit(t *testing.T) {
	dir := t.TempDir()
	audio := fixture(t, "seek.mp3")[:256]
	for i := range showRecent + 5 {
		write(t, filepath.Join(dir, fmt.Sprintf("Song %03d.mp3", i)), audio)
	}
	s := scanned(t, dir)
	for _, tr := range songs(t, s) {
		s.Played(tr)
	}
	recent, _ := s.Recent()
	if len(recent) != showRecent || recent[0].Name != fmt.Sprintf("Song %03d", showRecent+4) {
		t.Errorf("%d, first %q", len(recent), recent[0].Name)
	}
}

// Listeners hear of a scan's progress, its end and any change to the library,
// with the scan shown as over by the time they hear of its end.
func TestOnChange(t *testing.T) {
	dir := t.TempDir()
	audio := fixture(t, "seek.mp3")[:256]
	for i := range scanBatch + 10 {
		write(t, filepath.Join(dir, fmt.Sprintf("s%d.mp3", i)), audio)
	}
	s := New(t.TempDir())
	changes := make(chan int, 16)
	s.OnChange(func() { changes <- s.TrackCount() })

	s.AddFolders(dir)
	var seen []int
	for len(seen) == 0 || seen[len(seen)-1] != scanBatch+10 {
		select {
		case n := <-changes:
			seen = append(seen, n)
		case <-time.After(20 * time.Second):
			t.Fatalf("heard %v", seen)
		}
	}
	if !slices.Contains(seen, scanBatch) {
		t.Errorf("no progress in %v", seen)
	}
	wait(t, s)
	for len(changes) > 0 {
		<-changes
	}

	hears := func(what string, fn func()) {
		t.Helper()
		fn()
		select {
		case <-changes:
		default:
			t.Errorf("%s: not heard", what)
		}
	}
	id := songs(t, s)[0].ID
	hears("played", func() { s.Played(music.Track{ID: id}) })
	hears("liked", func() { s.Like(id, true) })
	hears("playlist", func() { s.CreatePlaylist("x") })
}

// A new scan takes over from one under way, and what's left is the latest's,
// with nothing else in the meantime answering wrongly or racing.
func TestScanSuperseded(t *testing.T) {
	root := t.TempDir()
	audio := fixture(t, "seek.mp3")[:256]
	one := filepath.Join(root, "one")
	two := filepath.Join(root, "two")
	for i := range 400 {
		write(t, filepath.Join(one, fmt.Sprintf("a%d.mp3", i)), audio)
		if i < 3 {
			write(t, filepath.Join(two, fmt.Sprintf("b%d.mp3", i)), audio)
		}
	}
	s := New(t.TempDir())
	s.OnChange(func() {
		s.TrackCount()
		s.Folders()
	})
	s.AddFolders(one)
	for range 5 {
		s.Rescan()
		s.Info()
		s.Scanning()
	}
	s.AddFolders(two)
	s.RemoveFolder(one)
	wait(t, s)

	if got := s.TrackCount(); got != 3 {
		t.Errorf("%d tracks", got)
	}
	if s.Scanning() {
		t.Error("still scanning")
	}
}

// An Ogg Vorbis file is found like the others, with its tags and length, and
// plays as what it is, whatever it's named.
func TestScanFindsOgg(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.ogg"), fixture(t, "seek.ogg"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := scanned(t, dir)
	r, err := s.Search("Ogg Title", music.SearchTracks, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tracks) != 1 {
		t.Fatalf("found %d tracks", len(r.Tracks))
	}
	tr := r.Tracks[0]
	if tr.Name != "Ogg Title" || tr.ArtistNames() != "Ogg Artist" || tr.Album.Name != "Ogg Album" {
		t.Errorf("tags: %+v", tr)
	}
	if tr.Duration < 4900 || tr.Duration > 5100 {
		t.Errorf("duration: %d", tr.Duration)
	}
	st, err := s.Stream(tr.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if st.Type != "ogg" {
		t.Errorf("type: %s", st.Type)
	}
}

// An Ogg Opus file is found like the others, with its tags and length, and
// plays as what it is, whatever it's named.
func TestScanFindsOpus(t *testing.T) {
	dir := t.TempDir()
	b := fixture(t, "seek.opus")
	for _, name := range []string{"a.opus", "b.ogg"} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := scanned(t, dir)
	r, err := s.Search("Opus Title", music.SearchTracks, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tracks) != 2 {
		t.Fatalf("found %d tracks", len(r.Tracks))
	}
	for _, tr := range r.Tracks {
		if tr.Name != "Opus Title" || tr.ArtistNames() != "Opus Artist" || tr.Album.Name != "Opus Album" {
			t.Errorf("tags: %+v", tr)
		}
		if tr.Duration < 4900 || tr.Duration > 5100 {
			t.Errorf("duration: %d", tr.Duration)
		}
		st, err := s.Stream(tr.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if st.Type != "opus" {
			t.Errorf("type: %s", st.Type)
		}
	}
}

// Tags read in the wrong encoding come back as what was written: GBK and UTF-8
// that the library took for Latin-1 or left as bytes, while real Latin-1 stays.
func TestDecodeLegacy(t *testing.T) {
	latin1 := func(b string) string {
		r := make([]rune, len(b))
		for i := range len(b) {
			r[i] = rune(b[i])
		}
		return string(r)
	}
	for _, c := range []struct{ in, want string }{
		{latin1("\xb4\xf8\xc0\xe1\xb5\xc4\xd3\xe3"), "带泪的鱼"},
		{"\xd5\xd4\xc0\xd7", "赵雷"}, // ID3v1, as bytes
		{latin1("\xe8\xb5\xb5\xe9\x9b\xb7"), "赵雷"},
		{latin1("Mot\xf6rhead"), "Motörhead"},
		{"Caf\xe9", "Café"},
		{"Hebe 田馥甄", "Hebe 田馥甄"},
	} {
		if got := decodeLegacy(c.in); got != c.want {
			t.Errorf("decodeLegacy(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// wavWith makes a second of 16-bit stereo WAV, with RIFF's INFO tags, as
// field and value pairs.
func wavWith(info ...[2]string) []byte {
	le32 := func(b []byte, v int) []byte { return binary.LittleEndian.AppendUint32(b, uint32(v)) }
	var list []byte
	list = append(list, "INFO"...)
	for _, f := range info {
		v := f[1] + "\x00"
		list = le32(append(list, f[0]...), len(v))
		list = append(list, v...)
		if len(v)%2 == 1 {
			list = append(list, 0)
		}
	}
	var b []byte
	b = le32(append(b, "RIFF"...), 0)
	b = append(b, "WAVE"...)
	b = le32(append(b, "fmt "...), 16)
	b = append(b, 1, 0, 2, 0)
	b = le32(b, 44100)
	b = le32(b, 44100*4)
	b = append(b, 4, 0, 16, 0)
	b = le32(append(b, "LIST"...), len(list))
	b = append(b, list...)
	b = le32(append(b, "data"...), 44100*4)
	b = append(b, make([]byte, 44100*4)...)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	return b
}

// WAV, ALAC and AAC files are found with their tags and lengths, and ALAC and
// WAV play as lossless.
func TestScanM4AAndWAV(t *testing.T) {
	dir, data := t.TempDir(), t.TempDir()
	write(t, filepath.Join(dir, "alac.m4a"), fixture(t, "seek.m4a"))
	write(t, filepath.Join(dir, "aac.M4A"), fixture(t, "seek-aac.m4a"))
	write(t, filepath.Join(dir, "Someone - Raw.aac"), fixture(t, "seek.aac"))
	write(t, filepath.Join(dir, "w.wav"), wavWith([2]string{"INAM", "Waves"}, [2]string{"IART", "The Sea"}, [2]string{"IPRD", "Shore"}, [2]string{"ITRK", "4"}))
	s := New(data)
	s.AddFolders(dir)
	wait(t, s)
	if got := s.TrackCount(); got != 4 {
		t.Fatalf("%d tracks: %v", got, names(songs(t, s)))
	}

	alac := songNamed(t, s, "Apple Lossless")
	if alac.ArtistNames() != "The Encoders" || alac.Album.Name != "Containers" || alac.Album.Released != "2019" || alac.Duration != 2000 {
		t.Errorf("alac track: %+v", alac)
	}
	if alac.Album.ImageURL == "" {
		t.Error("alac track: no cover")
	}
	aac := songNamed(t, s, "Advanced Audio")
	if aac.Duration != 3000 {
		t.Errorf("aac track: %+v", aac)
	}
	raw := songNamed(t, s, "Raw")
	if raw.ArtistNames() != "Someone" || raw.Duration < 3000 || raw.Duration > 3200 {
		t.Errorf("adts track: %+v", raw)
	}
	wav := songNamed(t, s, "Waves")
	if wav.ArtistNames() != "The Sea" || wav.Album.Name != "Shore" || wav.Duration != 1000 {
		t.Errorf("wav track: %+v", wav)
	}

	for _, tt := range []struct {
		tr           music.Track
		typ, quality string
	}{
		{alac, "m4a", "lossless"},
		{aac, "m4a", "high"},
		{raw, "aac", "high"},
		{wav, "wav", "lossless"},
	} {
		st, err := s.Stream(tt.tr.ID, "standard")
		if err != nil || st.Type != tt.typ || st.Quality != tt.quality {
			t.Errorf("%s: %+v, %v", tt.tr.Name, st, err)
		}
	}
}

// A WAV's tags in an ID3 chunk are read, before its INFO ones.
func TestWAVWithID3(t *testing.T) {
	dir := t.TempDir()
	b := wavWith([2]string{"INAM", "From INFO"})
	tag := id3([2]string{"TIT2", "From ID3"}, [2]string{"TPE1", "Tagger"})
	chunk := binary.LittleEndian.AppendUint32([]byte("id3 "), uint32(len(tag)))
	chunk = append(chunk, tag...)
	if len(tag)%2 == 1 {
		chunk = append(chunk, 0)
	}
	b = append(b, chunk...)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	write(t, filepath.Join(dir, "w.wav"), b)
	s := scanned(t, dir)
	if tr := songNamed(t, s, "From ID3"); tr.ArtistNames() != "Tagger" || tr.Duration != 1000 {
		t.Errorf("track: %+v", tr)
	}
}
