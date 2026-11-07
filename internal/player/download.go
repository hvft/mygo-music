package player

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mygo-music/internal/music"
)

// Song is a track's audio found for the listener to keep a copy of, in a file
// of their own: kept in the cache, a file on disk, or at its source.
type Song struct {
	// Type is its file's: one of songTypes, as its source said.
	Type string
	file *os.File      // kept in the cache, or on disk
	st   *music.Stream // or else where it streams from
}

// FindSong finds track t of src at quality, to save, as the player would play
// it: kept in the cache, or as src streams it. It blocks, for src is asked; run
// it off the main thread, and close what it returns, or save it.
func (p *Player) FindSong(src music.Source, t music.Track, quality string) (*Song, error) {
	if f, st, ok := p.Cache.find(t.Source, t.ID, quality); ok {
		return &Song{Type: st.Type, file: f}, nil
	}
	if src == nil {
		return nil, errNoSource
	}
	st, err := src.Stream(t.ID, quality)
	if err != nil {
		return nil, err
	}
	if st == nil || st.URL == "" {
		return nil, music.ErrUnavailable
	}
	s := &Song{Type: songType(st.Type), st: st}
	if path, ok := music.PathOf(st.URL); ok {
		if s.file, err = os.Open(path); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// songTypes are the types of song the app knows, which name their files.
var songTypes = []string{"mp3", "flac", "ogg", "opus", "wav", "m4a", "aac"}

// songType is the type a stream says it is, to name its file by: one the app
// knows, or else mp3, as the cache takes it. What a source sent may hold
// anything.
func songType(typ string) string {
	if typ = strings.ToLower(typ); slices.Contains(songTypes, typ) {
		return typ
	}
	return "mp3"
}

// Close lets go of the song, unsaved.
func (s *Song) Close() {
	if s.file != nil {
		s.file.Close()
	}
}

// SaveTo writes the song to path, in place of what's there, and closes it. It
// comes into a file of its own beside path first, so one cut short leaves path
// as it was.
func (s *Song) SaveTo(path string) error {
	defer s.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.part")
	if err != nil {
		return err
	}
	err = s.copy(tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// copy writes all of the song to w.
func (s *Song) copy(w io.Writer) error {
	if s.file != nil {
		_, err := io.Copy(w, s.file)
		return err
	}
	req, err := http.NewRequest("GET", s.st.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", music.Agent)
	for k, v := range s.st.Headers {
		req.Header.Set(k, v)
	}
	res, err := music.HeadersStay(http.DefaultClient, req.URL.Hostname(), s.st.Headers).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New(res.Status)
	}
	_, err = io.Copy(w, res.Body)
	return err
}
