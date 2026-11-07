// Package local is the built-in source of music files on disk: the folders the
// listener chose, scanned for .mp3, .flac, .ogg, .opus, .wav, .m4a and .aac.
// What it lists is held in memory and swapped whole as a scan goes on, so
// asking never waits for one. What it keeps is in its data directory:
// library.json is the listener's own (folders, likes, playlists, what was
// played), index.json only saves reading the tags of files again, and covers/
// holds the pictures found inside files.
package local

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"mygo-music/internal/i18n"
	"mygo-music/internal/music"
)

// ID is the source's ID, on every track it answers.
const ID = "local"

const (
	pageSize = 200
	// keepRecent is how many plays are kept, showRecent how many Recent answers:
	// more are kept for those that went away with their folder to make room.
	keepRecent = 200
	showRecent = 100
	// homeRows is the most Home shows of what has a page of its own.
	homeRows = 12
)

// can is what Can says yes to, by the name of the plugin function.
var can = map[string]bool{
	"search": true, "playlists": true, "playlist": true, "album": true, "artist": true,
	"liked": true, "recent": true, "isLiked": true, "like": true,
	"createPlaylist": true, "renamePlaylist": true, "deletePlaylist": true,
	"addToPlaylist": true, "removeFromPlaylist": true, "movePlaylistTrack": true,
	"lyrics": true, "stream": true, "home": true, "browse": true,
}

// Source is a music.Source of the files in the listener's folders. It's safe
// to use from any goroutine.
type Source struct {
	dir string

	// cat is what's listed now: never changed, only replaced. Reading it takes
	// no lock.
	cat atomic.Pointer[catalog]

	// mu is only held for moments: nothing under it touches the disk.
	mu         sync.RWMutex
	lib        library
	onChange   func()
	scanning   bool
	scanGen    int64 // the scan under way; an older one publishes nothing
	cancelScan func()
	scanDone   chan struct{} // closes when the latest scan has stopped
	saves      int64         // counts changes to lib, for writing the latest

	// saveMu keeps writes of library.json in order, without holding up mu.
	saveMu sync.Mutex
	saved  int64 // which change library.json holds
}

// New returns the source keeping its data in dataDir, listing what the last
// scan found. It doesn't scan: see Rescan.
func New(dataDir string) *Source {
	s := &Source{dir: dataDir}
	if b, err := os.ReadFile(s.libraryPath()); err == nil {
		if err := json.Unmarshal(b, &s.lib); err != nil {
			log.Printf("local: reading library: %v", err)
		}
	}
	s.cat.Store(buildCatalog(s.loadIndex(s.lib.Folders)))
	return s
}

// OnChange sets what's called, on any goroutine, after what the source lists
// changed: a scan's batch and its end, folders, playlists, likes and plays.
// Set it once, before the first Rescan.
func (s *Source) OnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

func (s *Source) notify() {
	s.mu.RLock()
	fn := s.onChange
	s.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Folders returns the folders scanned.
func (s *Source) Folders() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.lib.Folders)
}

// AddFolders keeps the folders and starts a scan.
func (s *Source) AddFolders(paths ...string) {
	s.changeFolders(func(folders []string) []string {
		for _, p := range paths {
			if p == "" {
				continue
			}
			if abs := cleanPath(p); !slices.Contains(folders, abs) {
				folders = append(folders, abs)
			}
		}
		return folders
	})
}

// RemoveFolder lets a folder go and starts a scan, which takes its tracks away.
func (s *Source) RemoveFolder(path string) {
	abs := cleanPath(path)
	s.changeFolders(func(folders []string) []string {
		return slices.DeleteFunc(folders, func(f string) bool { return f == abs })
	})
}

func (s *Source) changeFolders(fn func([]string) []string) {
	err := s.change(func() error {
		s.lib.Folders = fn(s.lib.Folders)
		return nil
	}, false)
	if err != nil {
		log.Printf("local: saving library: %v", err)
	}
	s.Rescan()
	s.notify()
}

func cleanPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// Scanning reports whether a scan is under way.
func (s *Source) Scanning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scanning
}

// TrackCount is how many tracks are listed now.
func (s *Source) TrackCount() int { return len(s.cat.Load().items) }

func (s *Source) Info() music.Info {
	return music.Info{
		ID:       ID,
		Name:     i18n.T("Local Files"),
		Playback: music.PlaybackStream,
		Navigation: []music.NavItem{
			{Builtin: "home"},
			{Title: i18n.T("Songs"), Page: "songs", Icon: "note"},
			{Title: i18n.T("Albums"), Page: "albums", Icon: "album"},
			{Title: i18n.T("Artists"), Page: "artists", Icon: "artist"},
			{Builtin: "liked"},
		},
	}
}

func (s *Source) Can(what string) bool { return can[what] }

func (s *Source) Setting(key string) string          { return "" }
func (s *Source) SetSetting(key, value string) error { return nil }

func (s *Source) SignIn() (*music.Account, error)  { return nil, music.ErrUnsupported }
func (s *Source) SignOut() error                   { return music.ErrUnsupported }
func (s *Source) Account() (*music.Account, error) { return nil, nil }

// tokens are what a query's words must each match, in lower case.
func tokens(query string) []string { return strings.Fields(strings.ToLower(query)) }

func matches(text string, words []string) bool {
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

// pageOf cuts a page out of all, and says whether more follows it.
func pageOf[T any](all []T, offset int) ([]T, bool) {
	offset = min(max(offset, 0), len(all))
	end := min(offset+pageSize, len(all))
	return all[offset:end], end < len(all)
}

func (s *Source) Search(query string, kind music.SearchKind, offset int) (*music.SearchResult, error) {
	words := tokens(query)
	cat := s.cat.Load()
	res := &music.SearchResult{}
	switch kind {
	case music.SearchTracks:
		var found []*item
		for _, it := range cat.items {
			if matches(it.search, words) {
				found = append(found, it)
			}
		}
		page, more := pageOf(found, offset)
		res.Tracks, res.More = publicTracks(page), more
	case music.SearchAlbums:
		var found []*album
		for _, al := range cat.albums {
			if matches(strings.ToLower(al.Name), words) {
				found = append(found, al)
			}
		}
		page, more := pageOf(found, offset)
		res.Albums, res.More = publicAlbums(page), more
	case music.SearchArtists:
		var found []music.Artist
		for _, a := range cat.artists {
			if matches(strings.ToLower(a.Name), words) {
				found = append(found, a.Artist)
			}
		}
		res.Artists, res.More = pageOf(found, offset)
	case music.SearchPlaylists:
		var found []music.Playlist
		lists, _ := s.Playlists()
		for _, p := range lists {
			if matches(strings.ToLower(p.Name), words) {
				found = append(found, p)
			}
		}
		res.Playlists, res.More = pageOf(found, offset)
	}
	return res, nil
}

func (s *Source) Suggest(query string) ([]string, error) { return nil, music.ErrUnsupported }

// publicPlaylist is a playlist as it's answered, of the tracks still on disk.
func publicPlaylist(p *playlist, cat *catalog) music.Playlist {
	out := music.Playlist{ID: p.ID, Name: p.Name, Mine: true}
	for _, id := range p.Tracks {
		it, ok := cat.byID[id]
		if !ok {
			continue
		}
		out.TrackCount++
		if out.ImageURL == "" {
			out.ImageURL = it.imageURL
		}
	}
	return out
}

func (s *Source) Playlists() ([]music.Playlist, error) {
	cat := s.cat.Load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	lists := make([]music.Playlist, len(s.lib.Playlists))
	for i := range s.lib.Playlists {
		lists[i] = publicPlaylist(&s.lib.Playlists[i], cat)
	}
	return lists, nil
}

func (s *Source) Playlist(id string) (*music.Playlist, error) {
	cat := s.cat.Load()
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.lib.playlist(id)
	if p == nil {
		return nil, noPlaylist(id)
	}
	out := publicPlaylist(p, cat)
	out.Tracks = cat.resolve(p.Tracks)
	return &out, nil
}

func (s *Source) Album(id string) (*music.AlbumPage, error) {
	al, ok := s.cat.Load().albumByID[id]
	if !ok {
		return nil, fmt.Errorf("no album %q", id)
	}
	return &music.AlbumPage{Album: al.public(), Tracks: publicTracks(al.items)}, nil
}

func (s *Source) Artist(id string) (*music.ArtistPage, error) {
	a, ok := s.cat.Load().artistByID[id]
	if !ok {
		return nil, fmt.Errorf("no artist %q", id)
	}
	return &music.ArtistPage{Artist: a.Artist, Tracks: publicTracks(a.items), Albums: publicAlbums(a.albums)}, nil
}

func (s *Source) Liked(offset int) (*music.Page, error) {
	s.mu.RLock()
	likes := slices.Clone(s.lib.Likes)
	s.mu.RUnlock()
	tracks, more := pageOf(s.cat.Load().resolve(likes), offset)
	return &music.Page{Tracks: tracks, More: more}, nil
}

func (s *Source) IsLiked(ids []string) (map[string]bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	likes := make(map[string]bool, len(s.lib.Likes))
	for _, id := range s.lib.Likes {
		likes[id] = true
	}
	res := make(map[string]bool, len(ids))
	for _, id := range ids {
		res[id] = likes[id]
	}
	return res, nil
}

func (s *Source) Like(id string, on bool) error {
	return s.change(func() error {
		at := slices.Index(s.lib.Likes, id)
		switch {
		case on && at < 0:
			s.lib.Likes = slices.Insert(s.lib.Likes, 0, id)
		case !on && at >= 0:
			s.lib.Likes = slices.Delete(s.lib.Likes, at, at+1)
		}
		return nil
	}, true)
}

// LikeEpisode: files on disk are no podcasts.
func (s *Source) LikeEpisode(string, bool) error { return music.ErrUnsupported }

// Recent answers the tracks played last, those no longer on disk left out.
func (s *Source) Recent() ([]music.Track, error) {
	s.mu.RLock()
	ids := slices.Clone(s.lib.Recent)
	s.mu.RUnlock()
	tracks := s.cat.Load().resolve(ids)
	return tracks[:min(len(tracks), showRecent)], nil
}

// Played puts the track first in Recent.
func (s *Source) Played(track music.Track) {
	if track.ID == "" {
		return
	}
	err := s.change(func() error {
		recent := slices.DeleteFunc(s.lib.Recent, func(id string) bool { return id == track.ID })
		recent = slices.Insert(recent, 0, track.ID)
		s.lib.Recent = recent[:min(len(recent), keepRecent)]
		return nil
	}, true)
	if err != nil {
		log.Printf("local: saving library: %v", err)
	}
}

func (s *Source) Home() (*music.Browse, error) {
	cat := s.cat.Load()
	playlists, _ := s.Playlists()
	if len(cat.items) == 0 && len(playlists) == 0 {
		return &music.Browse{}, nil
	}
	recent, _ := s.Recent()
	recent = recent[:min(len(recent), homeRows)]

	var sections []music.Section
	if len(recent) > 0 {
		sections = append(sections, music.Section{Title: i18n.T("Recently Played"), Tracks: recent})
	}
	if len(cat.albums) > 0 {
		albums := cat.albums[:min(len(cat.albums), homeRows)]
		sections = append(sections, music.Section{Title: i18n.T("Albums"), Albums: publicAlbums(albums), MorePage: "albums"})
	}
	if len(playlists) > 0 {
		sections = append(sections, music.Section{Title: i18n.T("Playlists"), Playlists: playlists})
	}
	if len(recent) == 0 && len(cat.items) > 0 {
		added := slices.Clone(cat.items)
		slices.SortStableFunc(added, func(a, b *item) int { return cmp.Compare(b.Mod, a.Mod) })
		added = added[:min(len(added), homeRows)]
		sections = append(sections, music.Section{Title: i18n.T("Songs"), Tracks: publicTracks(added), MorePage: "songs"})
	}
	return &music.Browse{Sections: sections}, nil
}

func (s *Source) Browse(page string) (*music.Browse, error) {
	cat := s.cat.Load()
	switch page {
	case "songs":
		return &music.Browse{
			Title:    i18n.T("Songs"),
			Subtitle: i18n.Songs(len(cat.items)),
			Sections: []music.Section{{Tracks: publicTracks(cat.items)}},
		}, nil
	case "albums":
		return &music.Browse{
			Title:    i18n.T("Albums"),
			Sections: []music.Section{{Albums: publicAlbums(cat.albums)}},
		}, nil
	case "artists":
		artists := make([]music.Artist, len(cat.artists))
		for i, a := range cat.artists {
			artists[i] = a.Artist
		}
		return &music.Browse{
			Title:    i18n.T("Artists"),
			Sections: []music.Section{{Artists: artists}},
		}, nil
	}
	return nil, fmt.Errorf("no page %q", page)
}

func (s *Source) CreatePlaylist(name string) (*music.Playlist, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(b[:])
	err := s.change(func() error {
		s.lib.Playlists = append(s.lib.Playlists, playlist{ID: id, Name: name})
		return nil
	}, true)
	if err != nil {
		return nil, err
	}
	return &music.Playlist{ID: id, Name: name, Mine: true}, nil
}

func (s *Source) RenamePlaylist(id, name string) error {
	return s.changePlaylist(id, func(p *playlist) error {
		p.Name = name
		return nil
	})
}

func (s *Source) DeletePlaylist(id string) error {
	return s.change(func() error {
		at := slices.IndexFunc(s.lib.Playlists, func(p playlist) bool { return p.ID == id })
		if at < 0 {
			return noPlaylist(id)
		}
		s.lib.Playlists = slices.Delete(s.lib.Playlists, at, at+1)
		return nil
	}, true)
}

func (s *Source) AddToPlaylist(id string, trackIDs []string) error {
	return s.changePlaylist(id, func(p *playlist) error {
		for _, t := range trackIDs {
			if !slices.Contains(p.Tracks, t) {
				p.Tracks = append(p.Tracks, t)
			}
		}
		return nil
	})
}

func (s *Source) RemoveFromPlaylist(id string, trackIDs []string) error {
	return s.changePlaylist(id, func(p *playlist) error {
		p.Tracks = slices.DeleteFunc(p.Tracks, func(t string) bool { return slices.Contains(trackIDs, t) })
		return nil
	})
}

// MovePlaylistTrack moves a track within the order stored, which may hold more
// than Playlist answers, while some are away.
// MovePlaylistTrack moves a track of those Playlist lists, which leaves out any
// not on disk now: they keep their places among the others.
func (s *Source) MovePlaylistTrack(id string, from, to int) error {
	cat := s.cat.Load()
	return s.changePlaylist(id, func(p *playlist) error {
		var listed []int
		for i, t := range p.Tracks {
			if _, ok := cat.byID[t]; ok {
				listed = append(listed, i)
			}
		}
		n := len(listed)
		if from < 0 || from >= n || to < 0 || to >= n {
			return fmt.Errorf("no track at %d or %d of %d", from, to, n)
		}
		from, to = listed[from], listed[to]
		t := p.Tracks[from]
		p.Tracks = slices.Insert(slices.Delete(p.Tracks, from, from+1), to, t)
		return nil
	})
}

func (s *Source) SavePlaylist(id string, on bool) error { return music.ErrUnsupported }
func (s *Source) FollowArtist(id string, on bool) error { return music.ErrUnsupported }
func (s *Source) SaveAlbum(id string, on bool) error    { return music.ErrUnsupported }
func (s *Source) SavedAlbums(offset int) (*music.AlbumList, error) {
	return nil, music.ErrUnsupported
}

func (s *Source) Shows() ([]music.Show, error) { return nil, music.ErrUnsupported }
func (s *Source) Show(id string, offset int) (*music.ShowPage, error) {
	return nil, music.ErrUnsupported
}
func (s *Source) SubscribeShow(id string, on bool) error { return music.ErrUnsupported }

func (s *Source) Stations() ([]music.Station, error)           { return nil, music.ErrUnsupported }
func (s *Source) StationNext(id string) ([]music.Track, error) { return nil, music.ErrUnsupported }

func (s *Source) Comments(trackID string, offset int) (*music.CommentPage, error) {
	return nil, music.ErrUnsupported
}
func (s *Source) LikeComment(trackID, commentID string, on bool) error {
	return music.ErrUnsupported
}
func (s *Source) PostComment(trackID, text string) (*music.Comment, error) {
	return nil, music.ErrUnsupported
}

// lrcTime is how a line of LRC starts, which tells it from plain words.
var lrcTime = regexp.MustCompile(`\[\d+:\d{2}`)

// Lyrics answers the .lrc beside the file, or else what's in its tags if it's
// timed: plain words can't follow the song.
func (s *Source) Lyrics(id string) (*music.Lyrics, error) {
	it, ok := s.cat.Load().byID[id]
	if !ok {
		return nil, music.ErrUnavailable
	}
	base := strings.TrimSuffix(it.Path, filepath.Ext(it.Path))
	for _, ext := range []string{".lrc", ".LRC"} {
		if b, err := os.ReadFile(base + ext); err == nil {
			return &music.Lyrics{LRC: strings.TrimPrefix(string(b), "\xef\xbb\xbf")}, nil
		}
	}
	f, err := os.Open(it.Path)
	if err != nil {
		return &music.Lyrics{}, nil
	}
	defer f.Close()
	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	if t := readTags(f, fileType(f, it.Path), size); lrcTime.MatchString(t.lyrics) {
		return &music.Lyrics{LRC: t.lyrics}, nil
	}
	return &music.Lyrics{}, nil
}

// Stream answers the file itself. The quality asked for doesn't come into it.
func (s *Source) Stream(id, quality string) (*music.Stream, error) {
	it, ok := s.cat.Load().byID[id]
	if !ok {
		return nil, music.ErrUnavailable
	}
	f, err := os.Open(it.Path)
	if err != nil {
		return nil, music.ErrUnavailable
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, music.ErrUnavailable
	}
	st := &music.Stream{
		URL:     music.FileURL(it.Path),
		Type:    fileType(f, it.Path),
		Quality: "high",
		Size:    info.Size(),
		Gain:    it.Gain,
		Peak:    it.Peak,
	}
	switch st.Type {
	case "flac", "wav":
		st.Quality = "lossless"
	case "m4a":
		if t, ok := m4aTrack(f, info.Size()); ok && lossless(t) {
			st.Quality = "lossless"
		}
	}
	return st, nil
}

func (s *Source) RemotePlay(ids []string, start int, position int64) error {
	return music.ErrUnsupported
}
func (s *Source) RemotePause() error                       { return music.ErrUnsupported }
func (s *Source) RemoteResume() error                      { return music.ErrUnsupported }
func (s *Source) RemoteSeek(position int64) error          { return music.ErrUnsupported }
func (s *Source) RemoteVolume(percent int) error           { return music.ErrUnsupported }
func (s *Source) RemoteState() (*music.RemoteState, error) { return nil, music.ErrUnsupported }
func (s *Source) RemoteDevices() ([]music.Device, error)   { return nil, music.ErrUnsupported }
func (s *Source) RemoteUse(deviceID string) error          { return music.ErrUnsupported }

// library is what library.json keeps: the IDs of tracks that may be away for
// now, and back when their folder is.
type library struct {
	Folders   []string   `json:"folders,omitempty"`
	Likes     []string   `json:"likes,omitempty"` // newest first
	Playlists []playlist `json:"playlists,omitempty"`
	Recent    []string   `json:"recent,omitempty"` // last played first
}

type playlist struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Tracks []string `json:"tracks,omitempty"`
}

func (l *library) playlist(id string) *playlist {
	for i := range l.Playlists {
		if l.Playlists[i].ID == id {
			return &l.Playlists[i]
		}
	}
	return nil
}

func noPlaylist(id string) error { return fmt.Errorf("no playlist %q", id) }

func (s *Source) libraryPath() string { return filepath.Join(s.dir, "library.json") }

func (s *Source) changePlaylist(id string, fn func(*playlist) error) error {
	return s.change(func() error {
		p := s.lib.playlist(id)
		if p == nil {
			return noPlaylist(id)
		}
		return fn(p)
	}, true)
}

// change makes a change to the library and writes it out, then says so if
// told to. The write happens outside mu: what only reads isn't held up by the
// disk. Of changes made at once, the last one's write holds them all.
func (s *Source) change(fn func() error, notify bool) error {
	s.mu.Lock()
	if err := fn(); err != nil {
		s.mu.Unlock()
		return err
	}
	b, err := json.MarshalIndent(&s.lib, "", "  ")
	s.saves++
	seq := s.saves
	s.mu.Unlock()
	if err == nil {
		s.saveMu.Lock()
		if seq > s.saved {
			if err = writeAtomic(s.libraryPath(), b); err == nil {
				s.saved = seq
			}
		}
		s.saveMu.Unlock()
	}
	if notify {
		s.notify()
	}
	return err
}

// writeAtomic writes a file whole or not at all: to another name beside it,
// then over it.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // gone already, once renamed
	_, err = f.Write(data)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

var (
	_ music.Source   = (*Source)(nil)
	_ music.Recorder = (*Source)(nil)
)
