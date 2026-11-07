package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"maps"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dhowden/tag"
	"github.com/egoist/mygo/audio/mp4"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const (
	// indexVersion changes when what's kept of a file does, for all of them to
	// be read again.
	indexVersion = 2
	// scanBatch is how many newly read files make a scan show what it has.
	scanBatch = 300
	// indexEvery is how often a long scan saves what it has, for quitting not to
	// lose it all.
	indexEvery = 5 * time.Second
)

type indexFile struct {
	Version int     `json:"version"`
	Tracks  []entry `json:"tracks"`
}

func (s *Source) indexPath() string { return filepath.Join(s.dir, "index.json") }
func (s *Source) coverDir() string  { return filepath.Join(s.dir, "covers") }

// loadIndex returns what the last scan found in the folders.
func (s *Source) loadIndex(folders []string) []entry {
	b, err := os.ReadFile(s.indexPath())
	if err != nil {
		return nil
	}
	var index indexFile
	if err := json.Unmarshal(b, &index); err != nil || index.Version != indexVersion {
		return nil
	}
	return slices.DeleteFunc(index.Tracks, func(e entry) bool {
		return !slices.ContainsFunc(folders, func(f string) bool { return within(e.Path, f) })
	})
}

func within(path, folder string) bool {
	rel, err := filepath.Rel(folder, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Source) saveIndex(entries map[string]entry) {
	tracks := slices.Collect(maps.Values(entries))
	slices.SortFunc(tracks, func(a, b entry) int { return strings.Compare(a.Path, b.Path) })
	b, err := json.Marshal(indexFile{Version: indexVersion, Tracks: tracks})
	if err == nil {
		err = writeAtomic(s.indexPath(), b)
	}
	if err != nil {
		log.Printf("local: saving index: %v", err)
	}
}

// Rescan starts a scan on its own goroutine, in place of one under way.
func (s *Source) Rescan() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.mu.Lock()
	if s.cancelScan != nil {
		s.cancelScan()
	}
	prev := s.scanDone
	s.cancelScan, s.scanDone = cancel, done
	s.scanGen++
	gen := s.scanGen
	s.scanning = true
	folders := slices.Clone(s.lib.Folders)
	s.mu.Unlock()

	go func() {
		defer close(done)
		defer cancel()
		// One at a time: the last one in charge of index.json is the latest.
		if prev != nil {
			<-prev
		}
		s.scan(ctx, gen, folders)
	}()
}

// found is a file a walk came across.
type found struct {
	path    string
	size    int64
	mod     int64
	sidecar string
}

func (s *Source) scan(ctx context.Context, gen int64, folders []string) {
	files := walk(ctx, folders)
	if ctx.Err() != nil {
		return
	}

	// What hasn't changed needs no reading. What has stays as it was until read,
	// for it not to blink out of what's listed.
	prev := s.cat.Load()
	current := make(map[string]entry, len(files))
	var fresh []found
	sidecarMoved := false
	covers := map[string]bool{}
	for _, f := range files {
		old := prev.byPath[f.path]
		if old != nil {
			current[f.path] = old.entry
		}
		if old == nil || old.Size != f.size || old.Mod != f.mod || !coverKept(old.Cover, covers) {
			fresh = append(fresh, f)
			continue
		}
		if old.Sidecar != f.sidecar {
			e := old.entry
			e.Sidecar = f.sidecar
			current[f.path] = e
			sidecarMoved = true
		}
	}
	gone := len(current) < len(prev.items)
	if gone || sidecarMoved {
		s.publish(gen, current, false)
	}

	lastSave := time.Now()
	read := 0
	for e := range s.readFiles(ctx, fresh) {
		current[e.Path] = e
		read++
		if read%scanBatch != 0 || ctx.Err() != nil {
			continue
		}
		s.publish(gen, current, false)
		if time.Since(lastSave) >= indexEvery {
			s.saveIndex(current)
			lastSave = time.Now()
		}
	}
	if ctx.Err() != nil {
		return
	}
	if gone || sidecarMoved || len(fresh) > 0 {
		s.saveIndex(current)
	}
	s.publish(gen, current, true)
}

// publish makes the entries what's listed, and says so, unless a newer scan has
// taken over. The last one of a scan ends it.
func (s *Source) publish(gen int64, entries map[string]entry, last bool) {
	cat := buildCatalog(slices.Collect(maps.Values(entries)))
	s.mu.Lock()
	ok := gen == s.scanGen
	if ok {
		s.cat.Store(cat)
		if last {
			s.scanning = false
		}
	}
	s.mu.Unlock()
	if ok {
		s.notify()
	}
}

// coverKept reports whether the kept picture of a file is still there, asking
// the disk once for each.
func coverKept(path string, seen map[string]bool) bool {
	if path == "" {
		return true
	}
	kept, ok := seen[path]
	if !ok {
		_, err := os.Stat(path)
		kept = err == nil
		seen[path] = kept
	}
	return kept
}

// walk finds the music in the folders, and the picture beside it if any.
// Hidden files and folders are left out, as are links to folders, which could
// lead round in circles; a folder chosen may be a link or hidden itself.
func walk(ctx context.Context, folders []string) []found {
	var files []found
	seen := map[string]bool{}
	sidecars := map[string]string{}
	ranks := map[string]int{}
	for _, root := range folders {
		start := root
		if !strings.HasSuffix(start, string(filepath.Separator)) {
			start += string(filepath.Separator) // WalkDir follows it, if it's a link
		}
		filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if err != nil {
				return nil // what can't be read is as good as gone
			}
			name := d.Name()
			if d.IsDir() {
				if path != start && strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(name, ".") {
				return nil // such as macOS's ._song.mp3
			}
			if rank := sidecarRank(name); rank >= 0 {
				dir := filepath.Dir(path)
				if have, ok := ranks[dir]; !ok || rank < have {
					ranks[dir] = rank
					sidecars[dir] = path
				}
				return nil
			}
			switch strings.ToLower(filepath.Ext(name)) {
			case ".mp3", ".flac", ".ogg", ".opus", ".wav", ".m4a", ".aac":
			default:
				return nil
			}
			if seen[path] {
				return nil // a folder within another also chosen
			}
			seen[path] = true
			info, err := os.Stat(path) // through a link, if it's one
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return nil
			}
			files = append(files, found{path: path, size: info.Size(), mod: info.ModTime().UnixNano()})
			return nil
		})
	}
	for i := range files {
		files[i].sidecar = sidecars[filepath.Dir(files[i].path)]
	}
	return files
}

// sidecarRank says how much a file's name makes it a folder's cover, lowest
// first, or -1 if it doesn't.
func sidecarRank(name string) int {
	name = strings.ToLower(name)
	ext := filepath.Ext(name)
	e := slices.Index([]string{".jpg", ".jpeg", ".png"}, ext)
	if e < 0 {
		return -1
	}
	n := slices.Index([]string{"cover", "folder", "front", "album"}, strings.TrimSuffix(name, ext))
	if n < 0 {
		return -1
	}
	return n*3 + e
}

// readFiles reads the files on a few goroutines, answering each as it's done.
// The channel closes once all are read, or soon after ctx ends.
func (s *Source) readFiles(ctx context.Context, files []found) <-chan entry {
	jobs := make(chan found)
	out := make(chan entry)
	go func() {
		defer close(jobs)
		for _, f := range files {
			select {
			case jobs <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	var workers sync.WaitGroup
	for range min(runtime.NumCPU(), 4) {
		workers.Go(func() {
			for f := range jobs {
				if e, ok := s.readFile(f); ok {
					out <- e
				}
			}
		})
	}
	go func() {
		workers.Wait()
		close(out)
	}()
	return out
}

// readFile reads what's known of a file, as far as there is any.
func (s *Source) readFile(file found) (entry, bool) {
	f, err := os.Open(file.path)
	if err != nil {
		return entry{}, false
	}
	defer f.Close()

	typ := fileType(f, file.path)
	var track *mp4.Track
	switch typ {
	case "m4a":
		var ok bool
		if track, ok = m4aTrack(f, file.size); !ok {
			return entry{}, false // HE-AAC, say, which the player doesn't decode
		}
	case "aac":
		if !adtsPlayable(f) {
			return entry{}, false
		}
	case "opus":
		if h := opusHead(f); h == nil || h[9] > 2 {
			return entry{}, false // not Opus after all, or more channels than the player decodes
		}
	}
	t := readTags(f, typ, file.size)
	e := entry{
		Path: file.path, Size: file.size, Mod: file.mod,
		Title: t.title, Artist: t.artist, Album: t.album, AlbumArtist: t.albumArtist,
		Year: t.year, Track: t.track, Disc: t.disc,
		Sidecar: file.sidecar,
		Gain:    t.gain, Peak: t.peak,
	}
	if e.Title == "" {
		title, artist := nameOfFile(file.path)
		e.Title = title
		if e.Artist == "" {
			e.Artist = artist
		}
	}
	switch typ {
	case "flac":
		e.Duration = flacDuration(f)
	case "ogg":
		e.Duration = oggDuration(f, file.size)
	case "opus":
		e.Duration = opusDuration(f, file.size)
	case "wav":
		e.Duration = wavDuration(f, file.size)
	case "m4a":
		e.Duration = track.Duration.Milliseconds()
	case "aac":
		e.Duration = adtsDuration(f, file.size)
	default:
		e.Duration = mp3Duration(f, file.size)
	}
	if t.picture != nil {
		e.Cover = s.keepCover(t.picture)
	}
	return e, true
}

// nameOfFile makes a title of a file's name, and an artist if it reads
// "Artist - Title". "01 - Title" names no artist.
func nameOfFile(path string) (title, artist string) {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	before, after, ok := strings.Cut(name, " - ")
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)
	if !ok || before == "" || after == "" {
		return name, ""
	}
	if strings.TrimFunc(before, func(r rune) bool { return unicode.IsDigit(r) || r == '.' }) == "" {
		return name, ""
	}
	return after, before
}

// tags is what a file's tags say.
type tags struct {
	title, artist, album, albumArtist string
	year, track, disc                 int
	gain, peak                        float64
	lyrics                            string
	picture                           *tag.Picture
}

// readTags reads a file's tags. The library panics on some malformed files, and
// answers nothing if it can't make them out: either way, what's found so far
// stands.
func readTags(f *os.File, typ string, size int64) (t tags) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("local: reading tags of %s: %v", f.Name(), r)
		}
	}()
	var src io.ReadSeeker = f
	// The library takes a FLAC behind an ID3 tag for an MP3.
	start := int64(0)
	switch typ {
	case "flac":
		start = id3v2Size(f)
	case "wav":
		// It reads no WAV: its tags are an ID3 tag in a chunk, or else
		// RIFF's own.
		id3 := wavID3(f, size)
		if id3 == nil {
			return wavInfo(f, size)
		}
		src = id3
	}
	if _, err := src.Seek(start, io.SeekStart); err != nil {
		return t
	}
	m, err := tag.ReadFrom(src)
	if err != nil {
		return t
	}
	t.title = cleanTag(m.Title())
	t.artist = cleanTag(m.Artist())
	t.album = cleanTag(m.Album())
	t.albumArtist = cleanTag(m.AlbumArtist())
	t.track, _ = m.Track()
	t.disc, _ = m.Disc()
	raw := m.Raw()
	t.year = yearOf(m.Year(), raw)
	t.gain, t.peak = replayGain(raw)
	t.lyrics = m.Lyrics()
	if t.lyrics == "" {
		t.lyrics, _ = raw["unsyncedlyrics"].(string)
	}
	t.lyrics = decodeLegacy(t.lyrics)
	t.picture = m.Picture()
	return t
}

func cleanTag(s string) string {
	return strings.Trim(decodeLegacy(s), " \t\r\n\x00")
}

// decodeLegacy undoes text the library read in the wrong encoding. ID3 has no
// way to say GBK, so Chinese taggers wrote it where ISO-8859-1 belongs, and the
// library hands it back a rune a byte; ID3v1 comes back as the bytes
// themselves. Some taggers wrote UTF-8 there too. A byte that's ASCII after a
// high one is taken for Latin-1, for "Motörhead" not to turn Chinese: GB2312,
// which nearly every such tag keeps to, has both bytes high.
func decodeLegacy(s string) string {
	var b []byte
	if utf8.ValidString(s) {
		high := false
		for _, r := range s {
			if r > 0xff {
				return s
			}
			high = high || r >= 0x80
		}
		if !high {
			return s
		}
		b = make([]byte, 0, len(s))
		for _, r := range s {
			b = append(b, byte(r))
		}
	} else {
		b = []byte(s)
	}
	if utf8.Valid(b) {
		return string(b)
	}
	if gbkPairs(b) {
		if out, err := simplifiedchinese.GBK.NewDecoder().Bytes(b); err == nil && utf8.Valid(out) && !bytes.ContainsRune(out, utf8.RuneError) {
			return string(out)
		}
	}
	// Latin-1 after all.
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

// gbkPairs tells whether every high byte of b is in a pair of two.
func gbkPairs(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] < 0x80 {
			continue
		}
		if b[i] == 0x80 || b[i] == 0xff || i+1 == len(b) || b[i+1] < 0x80 || b[i+1] == 0xff {
			return false
		}
		i++
	}
	return true
}

// yearOf is the year the library found, or else from a date it can't parse, of
// which it may say the year 1.
func yearOf(year int, raw map[string]any) int {
	if year >= 1000 {
		return year
	}
	for _, key := range []string{"date", "year", "TDRC", "TYER", "TYE"} {
		if s, ok := raw[key].(string); ok && len(s) >= 4 {
			if y, err := strconv.Atoi(s[:4]); err == nil && y >= 1000 {
				return y
			}
		}
	}
	return 0
}

// replayGain finds the track's replay gain, in decibels, and peak: under their
// own names in Vorbis comments, and in a TXXX frame of ID3.
func replayGain(raw map[string]any) (gain, peak float64) {
	for key, v := range raw {
		var text string
		switch v := v.(type) {
		case string:
			text = v
		case *tag.Comm:
			key, text = v.Description, v.Text
		default:
			continue
		}
		switch strings.ToLower(key) {
		case "replaygain_track_gain":
			gain = parseGain(text)
		case "replaygain_track_peak":
			peak = parseGain(text)
		}
	}
	return gain, peak
}

// parseGain reads the number of "-6.5 dB".
func parseGain(text string) float64 {
	text = strings.TrimRightFunc(strings.TrimSpace(text), func(r rune) bool { return unicode.IsLetter(r) || unicode.IsSpace(r) })
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// keepCover writes a picture to where covers are, under what's in it, for a
// whole album's copies to be one file. It returns the path, or "" if it's not
// a picture the app can show.
func (s *Source) keepCover(pic *tag.Picture) string {
	ext := coverExt(pic)
	if ext == "" {
		return ""
	}
	sum := sha256.Sum256(pic.Data)
	path := filepath.Join(s.coverDir(), hex.EncodeToString(sum[:8])+"."+ext)
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if err := writeAtomic(path, pic.Data); err != nil {
		log.Printf("local: keeping cover: %v", err)
		return ""
	}
	return path
}

// coverExt goes by what the picture holds, since taggers often say wrong, and
// else by what it says it is.
func coverExt(pic *tag.Picture) string {
	if len(pic.Data) == 0 {
		return ""
	}
	mime := http.DetectContentType(pic.Data)
	if !strings.HasPrefix(mime, "image/") {
		mime = pic.MIMEType
	}
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	}
	return ""
}

// fileType says whether a file is an mp3, a flac, an ogg, an opus, a wav, an
// m4a or an aac: by what it holds if that tells, and else by its name.
func fileType(f io.ReaderAt, path string) string {
	var head [12]byte
	if _, err := f.ReadAt(head[:], 0); err == nil {
		if string(head[4:8]) == "ftyp" {
			return "m4a"
		}
		if (string(head[:4]) == "RIFF" || string(head[:4]) == "RF64") && string(head[8:12]) == "WAVE" {
			return "wav"
		}
	}
	clear(head[:])
	if _, err := f.ReadAt(head[:4], id3v2Size(f)); err == nil && string(head[:4]) == "fLaC" {
		return "flac"
	}
	if string(head[:4]) == "OggS" {
		if opusHead(f) != nil {
			return "opus"
		}
		return "ogg"
	}
	if head[0] == 0xff && head[1]&0xf6 == 0xf0 {
		return "aac" // ADTS: MP3's layer bits are never 0
	}
	if _, ok := parseFrameHeader(head[:4]); ok {
		return "mp3"
	}
	switch ext := strings.ToLower(filepath.Ext(path)); ext {
	case ".flac", ".ogg", ".opus", ".wav", ".m4a", ".aac":
		return ext[1:]
	}
	return "mp3"
}
