package player

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/cmplx"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/egoist/mygo/audio"
	gomp3 "github.com/hajimehoshi/go-mp3"

	"mygo-music/internal/music"
)

// A download fetches a song as it plays: readers wait for the bytes they
// need, so playback starts with the first ones. The song comes into a file
// of the cache, which keeps it once it has all come in, and is read from
// there: the system holds what of it was read lately, rather than the
// app's memory, as much again as the song. Songs the cache would not keep
// come into memory.
type download struct {
	mu     sync.Mutex
	cond   sync.Cond
	file   *os.File // where the song is, or nil: then data
	data   []byte
	n      int64 // the bytes in so far
	size   int64 // expected, or 0 if unknown
	done   bool
	err    error
	cancel context.CancelFunc
	// part is the file's path until the cache keeps it, which keep does
	// once the song has all come in: the path it is at then.
	part string
	keep func(part string, n int64) (string, bool)
	// users counts what reads or writes the song: the stream decoding it,
	// until it closes, and the goroutine fetching it. The last to go
	// closes the file, and removes it unless the cache kept it.
	users  int
	closed bool
	// onDisk says the song is a whole file, to walk through at will.
	onDisk bool
}

// startDownload fetches url, of size bytes if known and with headers, into
// part, a file that keep keeps once it has all come in: or into memory,
// without one.
func startDownload(url string, size int64, headers map[string]string, part *os.File, keep func(string, int64) (string, bool)) *download {
	ctx, cancel := context.WithCancel(context.Background())
	d := &download{size: size, cancel: cancel, users: 2}
	d.cond.L = &d.mu
	switch {
	case part != nil:
		d.file, d.part, d.keep = part, part.Name(), keep
	case size > 0:
		d.data = make([]byte, 0, prealloc(size))
	}
	go d.run(ctx, url, headers)
	return d
}

// maxPrealloc is the most memory set aside for a song before its bytes
// come in: the size it is said to be is the API's word, which a song that
// turns out longer only outgrows.
const maxPrealloc = 32 << 20

func prealloc(size int64) int64 { return min(size, maxPrealloc) }

// downloaded returns a download that has all of b in already.
func downloaded(b []byte) *download {
	d := &download{data: b, n: int64(len(b)), size: int64(len(b)), done: true, cancel: func() {}, users: 1}
	d.cond.L = &d.mu
	return d
}

// opened returns a download that has all of the file f in already, of
// size bytes, as a song from the cache or from disk.
func opened(f *os.File, size int64) *download {
	d := &download{file: f, n: size, size: size, done: true, cancel: func() {}, users: 1, onDisk: true}
	d.cond.L = &d.mu
	return d
}

func (d *download) run(ctx context.Context, url string, headers map[string]string) {
	defer d.release()
	finish := func(err error) {
		d.mu.Lock()
		d.done, d.err = true, err
		d.cond.Broadcast()
		d.mu.Unlock()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		finish(err)
		return
	}
	req.Header.Set("User-Agent", music.Agent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := music.HeadersStay(http.DefaultClient, req.URL.Hostname(), headers).Do(req)
	if err != nil {
		finish(err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		finish(errors.New(res.Status))
		return
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := res.Body.Read(buf)
		if n > 0 {
			d.write(buf[:n])
		}
		if err == io.EOF {
			// Whole if as long as promised: the cache's.
			if d.part != "" && (d.size <= 0 || d.n == d.size) {
				d.hand()
			}
			finish(nil)
			return
		}
		if err != nil {
			finish(err)
			return
		}
	}
}

// write adds b to what is in. Only run writes, so it reads the file and
// what is in without the lock, which guards them from readers.
func (d *download) write(b []byte) {
	if d.file != nil {
		if _, err := d.file.Write(b); err != nil {
			d.toMemory()
		}
	}
	d.mu.Lock()
	if d.file == nil {
		d.data = append(d.data, b...)
	}
	d.n += int64(len(b))
	d.cond.Broadcast()
	d.mu.Unlock()
}

// toMemory moves the song into memory from a file that takes no more, as
// on a full disk: it plays on, but is not kept.
func (d *download) toMemory() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.data = make([]byte, d.n, max(prealloc(d.size), d.n))
	got, _ := d.file.ReadAt(d.data, 0)
	d.data = d.data[:got]
	d.n = int64(got)
	d.file.Close()
	os.Remove(d.part)
	d.file, d.part = nil, ""
}

// hand gives the song, all in, to the cache, and reads it from where it
// is kept. Windows moves no file that is open: it closes the file first,
// and opens it again where it went, while no one reads it.
func (d *download) hand() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.file.Close()
	path, ok := d.keep(d.part, d.n)
	f, err := os.Open(path)
	if err != nil {
		d.file, d.n, d.done, d.err = nil, 0, true, err
		return
	}
	d.file = f
	if ok {
		d.part = ""
	}
}

// Close stops the download and wakes its readers: the stream lets the
// song go.
func (d *download) Close() {
	d.cancel()
	d.mu.Lock()
	if !d.done {
		d.done, d.err = true, context.Canceled
	}
	d.cond.Broadcast()
	closed := d.closed
	d.closed = true
	d.mu.Unlock()
	if !closed {
		d.release()
	}
}

// share counts one more user of the song, as a second stream decoding it, unless
// it is gone already.
func (d *download) share() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.users <= 0 {
		return false
	}
	d.users++
	return true
}

// release lets the song go for one of its users. With none left, nothing
// reads it any more: its file closes, and goes unless the cache kept it.
func (d *download) release() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.users--; d.users > 0 {
		return
	}
	if d.file != nil {
		d.file.Close()
	}
	if d.part != "" {
		os.Remove(d.part)
	}
	d.file, d.data, d.n, d.part = nil, nil, 0, ""
}

// Progress returns the share of the song downloaded so far.
func (d *download) Progress() float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.done && d.err == nil {
		return 1
	}
	if d.size <= 0 {
		return 0
	}
	return float64(d.n) / float64(d.size)
}

// waitFor waits until n bytes are in, or the download ended, and returns
// how many there are.
func (d *download) waitFor(n int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.n < n && !d.done {
		d.cond.Wait()
	}
	return d.n
}

// received returns how many bytes are in, and whether they're all of them.
func (d *download) received() (n int64, done bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n, d.done
}

// readAt reads into b what is in from off, as much as fits. Its caller
// holds the lock.
func (d *download) readAt(b []byte, off int64) (int, error) {
	n := min(int64(len(b)), d.n-off)
	if n <= 0 {
		return 0, nil
	}
	if d.file == nil {
		return copy(b, d.data[off:d.n]), nil
	}
	return d.file.ReadAt(b[:n], off)
}

// peek returns up to n bytes from off, as far as they are in.
func (d *download) peek(off int64, n int) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := make([]byte, n)
	got, _ := d.readAt(b, off)
	return b[:got]
}

// A window reads a download a piece at a time, for scanning it byte by
// byte without reading the file for each.
type window struct {
	dl  *download
	off int64
	b   []byte
}

// at returns up to n bytes from off, as far as they are in.
func (w *window) at(off int64, n int) []byte {
	if off < w.off || off+int64(n) > w.off+int64(len(w.b)) {
		w.off, w.b = off, w.dl.peek(off, max(n, 32<<10))
	}
	return w.b[off-w.off : min(int64(len(w.b)), off-w.off+int64(n))]
}

// A downloadReader reads a download from an offset, waiting for its bytes.
// It is no io.Seeker, so go-mp3 does not read the whole file up front to
// find where each frame starts.
type downloadReader struct {
	d   *download
	pos int64
}

func (r *downloadReader) Read(p []byte) (int, error) {
	d := r.d
	d.mu.Lock()
	defer d.mu.Unlock()
	for d.n <= r.pos && !d.done {
		d.cond.Wait()
	}
	if d.n <= r.pos {
		if d.err != nil {
			return 0, d.err
		}
		return 0, io.EOF
	}
	n, err := d.readAt(p, r.pos)
	if n == 0 {
		return 0, cmp.Or(err, io.ErrUnexpectedEOF)
	}
	r.pos += int64(n)
	return n, nil
}

// A stream decodes a download for package audio, and measures the
// spectrum of what it decodes, for the visualizers: an MP3's or a FLAC's.
type stream interface {
	audio.Decoder
	Close() error
	// source returns the download decoded, and its spectrum.
	source() (*download, *spectrum)
}

// openStream opens the song dl fetches, MP3, FLAC, Ogg Vorbis or Opus, WAV,
// AAC, or ALAC or AAC in an MP4, as its first bytes say. An MP3 says nothing
// of its length, nor does AAC still coming in: durationMs is it.
func openStream(dl *download, durationMs int64) (stream, error) {
	dl.waitFor(12)
	if head := dl.peek(0, 12); len(head) == 12 {
		switch {
		case string(head[4:8]) == "ftyp":
			return openMP4Stream(dl)
		case (string(head[:4]) == "RIFF" || string(head[:4]) == "RF64") && string(head[8:12]) == "WAVE":
			return openWAVStream(dl)
		}
	}
	off := id3Size(dl)
	if dl.waitFor(off+7) >= off+4 {
		head := dl.peek(off, 7)
		switch string(head[:4]) {
		case "fLaC":
			return openFLACStream(dl)
		case "OggS":
			if oggOpus(dl) {
				return openOpusStream(dl, durationMs)
			}
			return openOggStream(dl, durationMs)
		}
		if _, _, ok := adtsHeader(head); ok {
			return openADTSStream(dl, off, durationMs)
		}
	}
	return openMP3Stream(dl, durationMs)
}

// oggOpus says whether the Ogg stream dl fetches holds Opus, whose first
// packet says so, or Vorbis.
func oggOpus(dl *download) bool {
	dl.waitFor(27)
	h := dl.peek(0, 27)
	if len(h) < 27 {
		return false
	}
	at := 27 + int64(h[26])
	if dl.waitFor(at+8) < at+8 {
		return false
	}
	return string(dl.peek(at, 8)) == "OpusHead"
}

// openShared opens a second stream of what dl fetches, for decoding apart
// from what plays: it measures no spectrum, and closing it lets the song
// go for it alone, leaving the download to the stream that plays.
func openShared(dl *download, durationMs int64) (stream, error) {
	if !dl.share() {
		return nil, errors.New("player: the song is gone")
	}
	s, err := openStream(dl, durationMs)
	if err != nil {
		dl.release()
		return nil, err
	}
	switch s := s.(type) {
	case *mp3Stream:
		s.spec, s.drop = nil, dl.release
	case *oggStream:
		s.spec, s.drop = nil, dl.release
	case *opusStream:
		s.spec, s.drop = nil, dl.release
	case *wavStream:
		s.spec, s.drop = nil, dl.release
	case *decodedStream:
		s.spec, s.drop = nil, dl.release
	case *adtsStream:
		s.spec, s.drop = nil, dl.release
	}
	return s, nil
}

// mp3Stream decodes a download for package audio. Coming in, it seeks by
// bytes, as if time and bytes were in proportion, as they are at a constant
// bit rate. A whole file it walks through once, for where each frame starts,
// so that a variable bit rate seeks true too. It also measures the spectrum
// of what it decodes, for the visualizers.
type mp3Stream struct {
	dl      *download
	dec     *gomp3.Decoder
	rate    int
	frames  int64 // the song's length in frames, from its duration
	start   int64 // the first audio frame's byte, after the ID3 tag
	pos     int64 // the frame decoded next
	skip    int   // bytes of decoded audio to drop after a seek
	buf     []byte
	spec    *spectrum
	drop    func() // lets the download go, as the stream closes
	closeMu sync.Once
	// starts holds where each MP3 frame starts, of a file on disk, once a
	// seek asked; perFrame is how many frames of sound each holds.
	starts   []int64
	perFrame int64
}

func openMP3Stream(dl *download, durationMs int64) (*mp3Stream, error) {
	s := &mp3Stream{dl: dl, drop: dl.Close}
	dec, err := gomp3.NewDecoder(&downloadReader{d: dl})
	if err != nil {
		return nil, err
	}
	s.dec, s.rate = dec, dec.SampleRate()
	s.frames = durationMs * int64(s.rate) / 1000
	s.start = id3Size(dl)
	if s.frames <= 0 {
		// A source that doesn't say how long it is.
		s.frames = s.estimateFrames()
	}
	s.spec = newSpectrum(s.rate)
	return s, nil
}

// estimateSize is how long a song of no known size is, to seek in it before
// all of it is in: as long as its first frame's bit rate makes its length, as
// a source converting a song as it's fetched writes it at one bit rate. With
// all of it in, or no frame to go by, it's what came.
func (s *mp3Stream) estimateSize() int64 {
	s.dl.waitFor(s.start + 4)
	s.dl.mu.Lock()
	done := s.dl.done
	s.dl.mu.Unlock()
	if kbps := frameBitRate(s.dl.peek(s.start, 4)); !done && kbps > 0 {
		return s.start + s.frames*int64(kbps)*1000/8/int64(s.rate)
	}
	return s.dl.waitFor(math.MaxInt64)
}

// id3Size returns the length of the ID3v2 tag at the start of the file.
func id3Size(dl *download) int64 {
	dl.waitFor(10)
	b := dl.peek(0, 10)
	if len(b) < 10 || string(b[:3]) != "ID3" {
		return 0
	}
	size := int64(b[6]&0x7f)<<21 | int64(b[7]&0x7f)<<14 | int64(b[8]&0x7f)<<7 | int64(b[9]&0x7f)
	return 10 + size
}

func (s *mp3Stream) SampleRate() int { return s.rate }
func (s *mp3Stream) Channels() int   { return 2 }
func (s *mp3Stream) Len() int64      { return s.frames }

func (s *mp3Stream) Read(dst []float32) (int, error) {
	n := len(dst) / 2 * 4
	if cap(s.buf) < n {
		s.buf = make([]byte, n)
	}
	b := s.buf[:n]
	for s.skip > 0 {
		got, err := io.ReadFull(s.dec, b[:min(n, s.skip)])
		s.skip -= got
		if err != nil {
			s.skip = 0
		}
	}
	got, err := io.ReadFull(s.dec, b)
	got -= got % 4
	samples := got / 2
	for i := range samples {
		dst[i] = float32(int16(binary.LittleEndian.Uint16(b[2*i:]))) / (1 << 15)
	}
	frames := int64(samples / 2)
	s.spec.feed(s.pos, dst[:samples])
	s.pos += frames
	if samples > 0 {
		return samples, nil
	}
	if err == io.ErrUnexpectedEOF || err == nil {
		err = io.EOF
	}
	return 0, err
}

// preroll is how many frames of sound a seek decodes before the one asked
// for, and drops. An MP3 frame borrows bits from the frames before it, up
// to 511 bytes back, which at 128 kbps is two frames, and its sound
// overlaps theirs: the first few decoded from the middle of a file are
// noise, as loud as the loudest sound.
const preroll = 4 * 1152

func (s *mp3Stream) SetPosition(frame int64) error {
	var off int64
	if s.dl.onDisk {
		return s.seekFile(frame)
	}
	if from := frame - preroll; from > 0 && s.frames > 0 {
		size := s.dl.size
		if size <= 0 {
			size = s.estimateSize()
		}
		off = s.start + int64(float64(size-s.start)*float64(from)/float64(s.frames))
		off = findFrame(s.dl, off)
	}
	dec, err := gomp3.NewDecoder(&downloadReader{d: s.dl, pos: off})
	if err != nil {
		return err
	}
	s.dec, s.pos = dec, frame
	// Drop what comes before frame: the preroll, or near the start, all
	// of it from there.
	s.skip = int(min(frame, preroll)) * 4
	return nil
}

// seekFile goes to frame of a file on disk, by where its frames start.
func (s *mp3Stream) seekFile(frame int64) error {
	if s.starts == nil {
		s.indexFrames()
	}
	var off, at int64
	if from := frame - preroll; from > 0 && len(s.starts) > 0 {
		i := min(from/s.perFrame, int64(len(s.starts)-1))
		off, at = s.starts[i], i*s.perFrame
	}
	dec, err := gomp3.NewDecoder(&downloadReader{d: s.dl, pos: off})
	if err != nil {
		return err
	}
	s.dec, s.pos = dec, frame
	s.skip = int(frame-at) * 4
	return nil
}

// indexFrames walks through the frames of the file, which also tells its
// length to the frame.
func (s *mp3Stream) indexFrames() {
	w := window{dl: s.dl}
	s.starts, s.perFrame = []int64{}, 1152
	off := findFrame(s.dl, s.start)
	for {
		b := w.at(off, 4)
		size := frameSize(b)
		if size == 0 {
			break
		}
		if len(s.starts) == 0 && b[1]>>3&3 != 3 {
			s.perFrame = 576 // MPEG 2 and 2.5
		}
		s.starts = append(s.starts, off)
		off += int64(size)
	}
	if n := int64(len(s.starts)) * s.perFrame; n > 0 {
		s.frames = n
	}
}

// estimateFrames says how long the song is from its size and the bit rate of
// its first frame, or 0 when it can't.
func (s *mp3Stream) estimateFrames() int64 {
	size := s.dl.size
	if size <= 0 {
		return 0
	}
	s.dl.waitFor(s.start + 4)
	b := s.dl.peek(s.start, 4)
	n := frameSize(b)
	if n == 0 {
		return 0
	}
	// A frame of n bytes holds a set number of frames of sound.
	perFrame := int64(1152)
	if b[1]>>3&3 != 3 {
		perFrame = 576
	}
	return (size - s.start) / int64(n) * perFrame
}

func (s *mp3Stream) source() (*download, *spectrum) { return s.dl, s.spec }

func (s *mp3Stream) Close() error {
	s.closeMu.Do(s.drop)
	return nil
}

// findFrame returns the start of the first MP3 frame at or after off whose
// header the next frame's confirms, so that bytes that only look like one
// within the audio do not count.
func findFrame(dl *download, off int64) int64 {
	w := window{dl: dl}
	for tries := 0; tries < 64<<10; tries++ {
		have := dl.waitFor(off + 4)
		if have < off+4 {
			return off
		}
		size := frameSize(w.at(off, 4))
		if size > 0 {
			dl.waitFor(off + int64(size) + 4)
			next := w.at(off+int64(size), 4)
			if len(next) < 4 || frameSize(next) > 0 {
				return off
			}
		}
		off++
	}
	return off
}

var (
	bitratesV1 = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	bitratesV2 = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	rates      = [4]int{44100, 48000, 32000, 0}
)

// frameBitRate returns the bit rate in kbps of the MPEG layer III frame whose
// header b starts with, or 0 if it starts with none.
func frameBitRate(b []byte) int {
	if frameSize(b) == 0 {
		return 0
	}
	if (b[1]>>3)&3 == 3 {
		return bitratesV1[b[2]>>4]
	}
	return bitratesV2[b[2]>>4]
}

// frameSize returns the length of the MPEG layer III frame whose header b
// starts with, or 0 if it starts with none.
func frameSize(b []byte) int {
	if len(b) < 4 || b[0] != 0xff || b[1]&0xe0 != 0xe0 {
		return 0
	}
	version := (b[1] >> 3) & 3 // 3: MPEG 1, 2: MPEG 2, 0: MPEG 2.5
	layer := (b[1] >> 1) & 3   // 1: layer III
	if version == 1 || layer != 1 {
		return 0
	}
	bi, ri := b[2]>>4, (b[2]>>2)&3
	if bi == 0 || bi == 15 || ri == 3 {
		return 0
	}
	pad := int(b[2]>>1) & 1
	rate := rates[ri]
	switch version {
	case 3:
		return 144*bitratesV1[bi]*1000/rate + pad
	case 2:
		return 72*bitratesV2[bi]*1000/(rate/2) + pad
	default:
		return 72*bitratesV2[bi]*1000/(rate/4) + pad
	}
}

// The spectrum of a song, measured in chunks of fftSize frames as it
// decodes, a second ahead of what plays: the visualizers look up the
// chunk playing. It keeps only the chunks of the last spectrumKeep
// decoded, in a ring, so that an audiobook of hours holds no more than
// a song does; a seek decodes, and so measures, again from where it lands.

const (
	fftSize      = 1024
	Bands        = 24
	spectrumKeep = 10 * time.Second
)

type spectrum struct {
	mu     sync.RWMutex
	rate   int
	ring   []measured // chunk c in ring[c%len(ring)], if it's still there
	window [fftSize]float32
	acc    []float32 // mono samples of the chunk being filled
	accAt  int64     // its first frame
	edges  [Bands + 1]int
	fft    []complex128
}

// measured is the bands of a chunk, and which chunk: -1 for none yet.
type measured struct {
	chunk int64
	bands [Bands]float32
}

func newSpectrum(rate int) *spectrum {
	n := max(16, int(int64(rate)*int64(spectrumKeep/time.Second)/fftSize)+1)
	s := &spectrum{rate: rate, ring: make([]measured, n), accAt: -1}
	for i := range s.ring {
		s.ring[i].chunk = -1
	}
	for i := range s.window {
		s.window[i] = float32(0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(fftSize-1)))
	}
	// Bands spaced evenly in pitch, from 40 Hz to 14 kHz.
	lo, hi := math.Log(40), math.Log(14000)
	for i := range s.edges {
		f := math.Exp(lo + (hi-lo)*float64(i)/Bands)
		s.edges[i] = max(1, min(fftSize/2-1, int(f*fftSize/float64(rate))))
	}
	for i := 1; i < len(s.edges); i++ {
		s.edges[i] = max(s.edges[i], s.edges[i-1]+1)
	}
	s.fft = make([]complex128, fftSize)
	return s
}

// feed takes stereo samples decoded from frame at, unless the stream
// measures no spectrum.
func (s *spectrum) feed(at int64, stereo []float32) {
	if s == nil {
		return
	}
	if s.accAt < 0 || at != s.accAt+int64(len(s.acc)) {
		// The start, or a seek: begin a chunk at a chunk's boundary.
		s.acc = s.acc[:0]
		skip := (fftSize - at%fftSize) % fftSize
		if int(skip)*2 >= len(stereo) {
			s.accAt = -1
			return
		}
		stereo = stereo[skip*2:]
		at += skip
		s.accAt = at
	}
	for i := 0; i+1 < len(stereo); i += 2 {
		s.acc = append(s.acc, (stereo[i]+stereo[i+1])/2)
		if len(s.acc) == fftSize {
			s.analyze(s.accAt / fftSize)
			s.accAt += fftSize
			s.acc = s.acc[:0]
		}
	}
}

func (s *spectrum) analyze(chunk int64) {
	if chunk < 0 {
		return
	}
	for i, v := range s.acc {
		s.fft[i] = complex(float64(v*s.window[i]), 0)
	}
	fft(s.fft)
	var bands [Bands]float32
	for b := range Bands {
		var sum float64
		for k := s.edges[b]; k < s.edges[b+1]; k++ {
			sum += cmplx.Abs(s.fft[k])
		}
		mag := sum / float64(s.edges[b+1]-s.edges[b]) / (fftSize / 4)
		// Higher bands carry less energy: tilt them up, as the ear hears.
		mag *= 1 + float64(b)/6
		db := 20 * math.Log10(mag+1e-9)
		bands[b] = float32(math.Max(0, math.Min(1, (db+60)/52)))
	}
	s.mu.Lock()
	s.ring[chunk%int64(len(s.ring))] = measured{chunk, bands}
	s.mu.Unlock()
}

// at returns the bands of frame, or false if not measured.
func (s *spectrum) at(frame int64) ([Bands]float32, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := frame / fftSize
	if c < 0 {
		return [Bands]float32{}, false
	}
	m := s.ring[c%int64(len(s.ring))]
	if m.chunk != c {
		return [Bands]float32{}, false
	}
	return m.bands, true
}

// fft transforms x in place; len(x) is a power of two.
func fft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			wk := complex(1, 0)
			for k := range size / 2 {
				a, b := x[start+k], x[start+k+size/2]*wk
				x[start+k], x[start+k+size/2] = a+b, a-b
				wk *= w
			}
		}
	}
}
