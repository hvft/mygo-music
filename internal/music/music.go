// Package music is what the player and its views know of music: tracks,
// albums and the like, whichever source they came from, and what a source
// has to do. Sources are plugins: see internal/plugin.
package music

import (
	"errors"
	"strings"
	"time"
)

// IDs are the source's own and mean nothing outside it.

type Artist struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ImageURL string `json:"imageUrl,omitempty"`
	// Following says the listener follows them, for a source with followArtist.
	Following bool `json:"following,omitempty"`
}

type Album struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	ImageURL string   `json:"imageUrl,omitempty"`
	Artists  []Artist `json:"artists,omitempty"`
	Released string   `json:"released,omitempty"` // as the source has it, such as 2024-03-01
	// Saved says the listener saved it, for a source with saveAlbum.
	Saved bool `json:"saved,omitempty"`
}

type Track struct {
	ID string `json:"id"`
	// Source is the ID of the source the track came from, which it plays
	// through. The source's own answers leave it out; it's filled in for them.
	Source   string   `json:"source,omitempty"`
	Name     string   `json:"name"`
	Artists  []Artist `json:"artists,omitempty"`
	Album    Album    `json:"album"`
	Duration int64    `json:"duration"` // milliseconds
	// URL is the track's page at its source, for sharing.
	URL string `json:"url,omitempty"`
	// Unavailable says the listener can't play it, for a source that knows.
	Unavailable bool `json:"unavailable,omitempty"`

	// An episode of a podcast is a track like any other, that says which show
	// it's of, what it's about and when it came out, as the source has it.
	ShowID      string `json:"showId,omitempty"`
	Description string `json:"description,omitempty"`
	Published   string `json:"published,omitempty"`
}

// ArtistNames joins the track's artists as the credits read.
func (t *Track) ArtistNames() string {
	names := make([]string, len(t.Artists))
	for i, a := range t.Artists {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

func (t *Track) Length() time.Duration { return time.Duration(t.Duration) * time.Millisecond }

type Playlist struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ImageURL    string  `json:"imageUrl,omitempty"`
	Description string  `json:"description,omitempty"`
	Owner       string  `json:"owner,omitempty"`
	TrackCount  int     `json:"trackCount,omitempty"`
	Mine        bool    `json:"mine,omitempty"`  // the listener's own
	Saved       bool    `json:"saved,omitempty"` // saved from someone else
	Fixed       bool    `json:"fixed,omitempty"` // the listener's, but kept by the source: liked songs, say
	Tracks      []Track `json:"tracks,omitempty"`
}

// ArtistPage is what an artist's page shows.
type ArtistPage struct {
	Artist Artist  `json:"artist"`
	Tracks []Track `json:"tracks,omitempty"`
	Albums []Album `json:"albums,omitempty"`
}

// AlbumPage is what an album's page shows.
type AlbumPage struct {
	Album  Album   `json:"album"`
	Tracks []Track `json:"tracks,omitempty"`
}

// Account is who is signed in to a source.
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ImageURL string `json:"imageUrl,omitempty"`
	// Page is the Browse page of the listener's own, such as their profile,
	// which the account menu opens as My Page.
	Page string `json:"page,omitempty"`
}

// SearchKind is what a search looks for.
type SearchKind string

const (
	SearchTracks    SearchKind = "tracks"
	SearchAlbums    SearchKind = "albums"
	SearchArtists   SearchKind = "artists"
	SearchPlaylists SearchKind = "playlists"
	SearchShows     SearchKind = "shows"
)

// SearchResult is a page of what a search found; only the kind searched for
// is filled in.
type SearchResult struct {
	Tracks    []Track    `json:"tracks,omitempty"`
	Albums    []Album    `json:"albums,omitempty"`
	Artists   []Artist   `json:"artists,omitempty"`
	Playlists []Playlist `json:"playlists,omitempty"`
	Shows     []Show     `json:"shows,omitempty"`
	More      bool       `json:"more,omitempty"` // there is a next page
}

// Len is how many things the page holds.
func (r *SearchResult) Len() int {
	return len(r.Tracks) + len(r.Albums) + len(r.Artists) + len(r.Playlists) + len(r.Shows)
}

// Page is a page of tracks.
type Page struct {
	Tracks []Track `json:"tracks,omitempty"`
	More   bool    `json:"more,omitempty"`
}

// AlbumList is a page of albums.
type AlbumList struct {
	Albums []Album `json:"albums,omitempty"`
	More   bool    `json:"more,omitempty"`
}

// Show is a podcast. Its episodes are tracks.
type Show struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ImageURL    string `json:"imageUrl,omitempty"`
	Description string `json:"description,omitempty"`
	Publisher   string `json:"publisher,omitempty"`
	// PublisherPage is the Browse page of who publishes it, such as their
	// profile, which their name on the show's page opens.
	PublisherPage string `json:"publisherPage,omitempty"`
	EpisodeCount  int    `json:"episodeCount,omitempty"`
	Subscribed    bool   `json:"subscribed,omitempty"`
}

// ShowPage is a show and a page of its episodes.
type ShowPage struct {
	Show     Show    `json:"show"`
	Episodes []Track `json:"episodes,omitempty"`
	More     bool    `json:"more,omitempty"`
}

// Station is an endless radio: it goes on with what StationNext gives.
type Station struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ImageURL    string `json:"imageUrl,omitempty"`
}

// Comment is what a listener said of a track.
type Comment struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	AvatarURL string `json:"avatarUrl,omitempty"`
	Text      string `json:"text"`
	Time      int64  `json:"time,omitempty"` // milliseconds since 1970
	Likes     int    `json:"likes,omitempty"`
	Liked     bool   `json:"liked,omitempty"`
}

// CommentPage is a page of a track's comments.
type CommentPage struct {
	Comments []Comment `json:"comments,omitempty"`
	Total    int       `json:"total,omitempty"` // of all pages, if the source knows
	More     bool      `json:"more,omitempty"`
}

// Browse is a page a source makes up itself, of sections it chooses: what its
// Home shows, and any other opened from a Link, a MorePage or its Navigation.
type Browse struct {
	Title    string `json:"title,omitempty"`
	Subtitle string `json:"subtitle,omitempty"`
	// ImageURL is a picture of whom the page is about, such as a listener's
	// avatar, which heads it round, as an artist's photo heads theirs.
	ImageURL string `json:"imageUrl,omitempty"`
	// Description goes under the title, a line to each line of it.
	Description string    `json:"description,omitempty"`
	Sections    []Section `json:"sections,omitempty"`
}

// Section is a heading over what it lists. A section may hold several kinds,
// shown in the order of the fields.
type Section struct {
	Title     string     `json:"title,omitempty"`
	Tracks    []Track    `json:"tracks,omitempty"`
	Albums    []Album    `json:"albums,omitempty"`
	Artists   []Artist   `json:"artists,omitempty"`
	Playlists []Playlist `json:"playlists,omitempty"`
	Shows     []Show     `json:"shows,omitempty"`
	Stations  []Station  `json:"stations,omitempty"`
	Links     []Link     `json:"links,omitempty"`
	// MorePage is the Browse page with the rest of it, if there is one.
	MorePage string `json:"morePage,omitempty"`
	// Empty is what the section says while it holds nothing, such as "No
	// public playlists". Without it, an empty section shows only its title.
	Empty string `json:"empty,omitempty"`
	// PageTitle heads the page of the section's songs that its feature card
	// opens, such as "Daily Song Picks" for a card titled "Daily Picks".
	// Without it, the page takes Title.
	PageTitle string `json:"pageTitle,omitempty"`
	// Subtitle follows the count of songs on that page, such as "Picked for
	// your taste, new every day at 6:00".
	Subtitle string `json:"subtitle,omitempty"`
}

// Link opens another Browse page.
type Link struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	ImageURL string `json:"imageUrl,omitempty"`
	Page     string `json:"page"`
}

// NavItem is an entry of the sidebar: a Browse page of the source's own, by
// Page, or one of the app's, by Builtin.
type NavItem struct {
	Title string `json:"title"`
	Page  string `json:"page,omitempty"`
	// Builtin is home, liked, savedAlbums, stations or shows. One the source
	// can't do doesn't show.
	Builtin string `json:"builtin,omitempty"`
	// Icon names one of the app's: home, note, album, artist, playlist, radio,
	// podcast, chart, calendar, heart, search or star.
	Icon string `json:"icon,omitempty"`
}

// Stream is audio the app plays itself: what a source of PlaybackStream
// answers for a track.
type Stream struct {
	URL     string            `json:"url"`
	Type    string            `json:"type"` // mp3, flac, ogg, opus, wav, m4a or aac
	Quality string            `json:"quality,omitempty"`
	BitRate int               `json:"bitRate,omitempty"`
	Size    int64             `json:"size,omitempty"`
	Peak    float64           `json:"peak,omitempty"`
	Gain    float64           `json:"gain,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Lyrics are a track's words: as LRC, the simple way, or as timed Lines, which
// can time each word, and are used if there are any.
type Lyrics struct {
	LRC         string      `json:"lrc,omitempty"`
	Translation string      `json:"translation,omitempty"` // LRC too
	Lines       []LyricLine `json:"lines,omitempty"`
}

// LyricLine is a line of timed lyrics, in milliseconds from the track's start.
type LyricLine struct {
	Start       int64       `json:"start"`
	End         int64       `json:"end,omitempty"`
	Text        string      `json:"text,omitempty"` // its words joined, if left out
	Translation string      `json:"translation,omitempty"`
	Words       []LyricWord `json:"words,omitempty"`
}

// LyricWord is a word of a line, as it's sung.
type LyricWord struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"`
	Text  string `json:"text"`
}

// Device is somewhere a source of PlaybackRemote can play.
type Device struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active,omitempty"`
}

// RemoteState is what a source of PlaybackRemote is playing.
type RemoteState struct {
	Playing  bool   `json:"playing"`
	TrackID  string `json:"trackId,omitempty"`
	Position int64  `json:"position"` // milliseconds
	Duration int64  `json:"duration"` // milliseconds
	Volume   int    `json:"volume"`   // 0 to 100
	Device   string `json:"device,omitempty"`
}

// Playback is how a source's tracks get heard.
type Playback string

const (
	// PlaybackStream sources give audio for the app to play.
	PlaybackStream Playback = "stream"
	// PlaybackRemote sources play elsewhere, in their own app, and the app
	// only tells them what to do.
	PlaybackRemote Playback = "remote"
)

// A Setting is something a plugin asks the listener for, such as an API key.
type Setting struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`
	// Link is where to get the value.
	Link string `json:"link,omitempty"`
}

// Info is how a source introduces itself.
type Info struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Version  string    `json:"version,omitempty"`
	Playback Playback  `json:"playback"`
	Settings []Setting `json:"settings,omitempty"`
	// Navigation is the sidebar while the source is active, in this order. With
	// none, the sidebar has an entry for each of the app's pages the source can do.
	Navigation []NavItem `json:"navigation,omitempty"`
}

var (
	// ErrSignIn says the listener has to sign in for this.
	ErrSignIn = errors.New("sign in first")
	// ErrSetup says a setting the source needs is missing.
	ErrSetup = errors.New("the plugin needs setting up")
	// ErrUnavailable says the track can't be played.
	ErrUnavailable = errors.New("not available")
	// ErrUnsupported says the source doesn't do this.
	ErrUnsupported = errors.New("not supported by this source")
)

// Qualities are the levels of audio the listener can ask a source for, worst
// first. A source may have others, or none, and answers what it has.
var Qualities = []string{"standard", "high", "lossless"}

// SignInPrompt is what a source wants shown while the listener signs in.
type SignInPrompt struct {
	// QR is what to show as a QR code, for signing in from a phone.
	QR string `json:"qr,omitempty"`
	// Text says what to do, or how it's going.
	Text string `json:"text,omitempty"`
}

// A Prompter is a source that has something to show while signing in.
type Prompter interface {
	OnSignInPrompt(show func(SignInPrompt))
}

// A Recorder is a source that wants to know what's played, for its Recent.
// A source that keeps that itself, as a service does, needn't be one.
type Recorder interface {
	Played(track Track)
}

// Source is where music comes from. Everything but Info and Can may take a
// while and is called off the main thread. A source that doesn't do something
// answers ErrUnsupported, and Can says so beforehand for the views to leave it
// out.
type Source interface {
	Info() Info
	// Can reports whether the source does what's named, by the name of its
	// plugin's function, such as "lyrics" or "like". It answers at once, on
	// the main thread, without starting anything.
	Can(what string) bool

	Setting(key string) string
	SetSetting(key, value string) error

	SignIn() (*Account, error)
	SignOut() error
	Account() (*Account, error)

	Search(query string, kind SearchKind, offset int) (*SearchResult, error)
	// Suggest says what a search being typed may be after.
	Suggest(query string) ([]string, error)
	// Playlists lists those the listener made and those they saved.
	Playlists() ([]Playlist, error)
	Playlist(id string) (*Playlist, error)
	Album(id string) (*AlbumPage, error)
	Artist(id string) (*ArtistPage, error)
	Liked(offset int) (*Page, error)
	Recent() ([]Track, error)

	// Home is what the Home page shows, for a source that makes up its own.
	Home() (*Browse, error)
	// Browse is any other page it makes up.
	Browse(page string) (*Browse, error)

	IsLiked(ids []string) (map[string]bool, error)
	Like(id string, on bool) error
	// LikeEpisode likes an episode of a podcast, which not every source that
	// likes songs can: a service's liked songs may take none.
	LikeEpisode(id string, on bool) error

	CreatePlaylist(name string) (*Playlist, error)
	RenamePlaylist(id, name string) error
	DeletePlaylist(id string) error
	AddToPlaylist(id string, trackIDs []string) error
	RemoveFromPlaylist(id string, trackIDs []string) error
	// MovePlaylistTrack moves the track at index from to index to.
	MovePlaylistTrack(id string, from, to int) error
	// SavePlaylist saves someone else's playlist, or lets it go.
	SavePlaylist(id string, on bool) error

	FollowArtist(id string, on bool) error
	SaveAlbum(id string, on bool) error
	SavedAlbums(offset int) (*AlbumList, error)

	// Shows lists the podcasts the listener subscribes to.
	Shows() ([]Show, error)
	Show(id string, offset int) (*ShowPage, error)
	SubscribeShow(id string, on bool) error

	Stations() ([]Station, error)
	// StationNext answers the next tracks of a station.
	StationNext(id string) ([]Track, error)

	Comments(trackID string, offset int) (*CommentPage, error)
	LikeComment(trackID, commentID string, on bool) error
	PostComment(trackID, text string) (*Comment, error)

	Lyrics(id string) (*Lyrics, error)

	// Stream answers audio for a track, of PlaybackStream sources.
	Stream(id, quality string) (*Stream, error)

	// The rest is of PlaybackRemote sources.
	RemotePlay(ids []string, start int, position int64) error
	RemotePause() error
	RemoteResume() error
	RemoteSeek(position int64) error
	RemoteVolume(percent int) error
	RemoteState() (*RemoteState, error)
	RemoteDevices() ([]Device, error)
	RemoteUse(deviceID string) error
}
