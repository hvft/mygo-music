package player

import (
	"cmp"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"slices"
	"sync"

	"github.com/pion/opus"
)

// Opus decodes at 48 kHz whatever rate it was recorded at, and its packets
// last 2.5 to 120 milliseconds.
const (
	opusRate  = 48000
	opusFrame = 5760 // the frames of a packet at its longest
)

// opusStream decodes an Ogg Opus download for package audio, to stereo: a
// mono file in both ears. A whole file is indexed page by page, for its
// length and for seeks; one still coming in plays right away with the length
// its source says, and waits for the rest only if it's asked to seek.
//
// Its samples are counted in granule positions, which an Ogg Opus page says:
// 48 kHz frames from the start of the stream, including the padding the
// encoder put before the sound. The frames the app plays are those less the
// padding, and stop where the last page says the sound ends.
type opusStream struct {
	dl      *download
	dec     opus.Decoder
	ch      int     // the file's channels: 1 or 2
	gain    float32 // what its header asks the sound be played at
	preSkip int64   // the padding at the start, in frames
	frames  int64   // the length, or -1 if unknown
	gpos    int64   // the granule of the frame Read returns next
	skip    int64   // frames to drop before it
	// The page being played, and what the page before left unfinished.
	off      int64
	serial   uint32
	packets  [][]byte
	partial  []byte
	page     []byte
	dropCont bool // drop the rest of a packet that began on the page before
	headLeft int  // packets of the header to drop, at the first page of sound
	// pages is every page, once indexed, and audio the first with sound.
	pages    []oggPage
	audio    int
	audioOff int64
	headPkts int
	indexed  bool
	buf      []float32 // a packet decoded
	pend     []float32 // frames decoded, not yet returned
	at       int       // where in pend they start
	spec     *spectrum
	drop     func() // lets the download go, as the stream closes
	once     sync.Once
}

// oggPage is an Ogg page's header: where it starts, how long it is, and the
// granule position of the last packet it finishes, or -1 for a page that
// finishes none.
type oggPage struct {
	off     int64
	n       int64
	granule int64
	serial  uint32
	flags   byte
	nsegs   int
}

// oggPageAt reads the header of the page at off, waiting for its bytes, and
// says false at the end of the stream.
func oggPageAt(dl *download, off int64) (p oggPage, ok bool) {
	if dl.waitFor(off+27) < off+27 {
		return p, false
	}
	h := dl.peek(off, 27)
	if string(h[:4]) != "OggS" || h[4] != 0 {
		return p, false
	}
	p = oggPage{
		off:     off,
		flags:   h[5],
		granule: int64(binary.LittleEndian.Uint64(h[6:14])),
		serial:  binary.LittleEndian.Uint32(h[14:18]),
		nsegs:   int(h[26]),
	}
	end := off + 27 + int64(p.nsegs)
	if dl.waitFor(end) < end {
		return p, false
	}
	p.n = end - off
	for _, l := range dl.peek(off+27, p.nsegs) {
		p.n += int64(l)
	}
	return p, true
}

func openOpusStream(dl *download, durationMs int64) (*opusStream, error) {
	s := &opusStream{dl: dl, frames: -1, gain: 1, drop: dl.Close}
	if err := s.readHead(); err != nil {
		return nil, err
	}
	dec, err := opus.NewDecoderWithOutput(opusRate, s.ch)
	if err != nil {
		return nil, err
	}
	s.dec, s.buf = dec, make([]float32, opusFrame*2)
	s.skip = s.preSkip // the encoder's padding, before the sound
	s.off, s.headLeft = s.audioOff, s.headPkts
	dl.mu.Lock()
	whole := dl.done || dl.onDisk
	dl.mu.Unlock()
	if whole {
		s.index()
	} else if durationMs > 0 {
		s.frames = durationMs * opusRate / 1000
	}
	s.spec = newSpectrum(opusRate)
	return s, nil
}

// readHead reads the stream's OpusHead and tags, and leaves the stream where
// the sound starts: the page, and the packets of the header still on it.
func (s *opusStream) readHead() error {
	off, packets, pages := int64(0), 0, 0
	var partial []byte
	for {
		p, ok := oggPageAt(s.dl, off)
		if !ok {
			if packets == 0 {
				return errors.New("opus: no OpusHead")
			}
			return errors.New("opus: no OpusTags")
		}
		off = p.off + p.n
		if pages == 0 {
			s.serial = p.serial
		}
		pages++
		if p.serial != s.serial {
			continue
		}
		b := make([]byte, p.n)
		if _, err := (&downloadFile{dl: s.dl}).ReadAt(b, p.off); err != nil {
			return err
		}
		pkts, rest := splitPackets(b[27:27+p.nsegs], b[27+p.nsegs:], partial)
		partial = rest
		for i, pkt := range pkts {
			if packets == 0 {
				if len(pkt) < 19 || string(pkt[:8]) != "OpusHead" {
					return errors.New("opus: not an Opus stream")
				}
				s.ch = int(pkt[9])
				s.preSkip = int64(binary.LittleEndian.Uint16(pkt[10:12]))
				s.gain = opusGain(int16(binary.LittleEndian.Uint16(pkt[16:18])))
				if s.ch < 1 || s.ch > 2 {
					return errors.New("opus: not mono or stereo")
				}
			}
			if packets++; packets == 2 {
				s.audioOff, s.headPkts = p.off, i+1
				return nil
			}
		}
	}
}

// opusGain is the gain a header asks for, in decibels as Q7.8, as a factor to
// multiply the sound by.
func opusGain(db int16) float32 {
	if db == 0 {
		return 1
	}
	return float32(math.Pow(10, float64(db)/5120))
}

// index walks every page of the stream, for its length and for seeks, and
// waits for the rest of the download to do it.
func (s *opusStream) index() {
	if s.indexed {
		return
	}
	s.indexed = true
	s.dl.waitFor(1 << 62)
	last := int64(0)
	for off := int64(0); ; {
		p, ok := oggPageAt(s.dl, off)
		if !ok {
			break
		}
		off = p.off + p.n
		if p.serial != s.serial {
			continue
		}
		// A page that finishes no packet is still at the granule of the one
		// before, which keeps the pages in order for a seek to search.
		if p.granule >= 0 {
			last = p.granule
		}
		p.granule = last
		if p.off == s.audioOff {
			s.audio = len(s.pages)
		}
		s.pages = append(s.pages, p)
	}
	if last > 0 {
		s.frames = max(last-s.preSkip, 0)
	}
}

func (s *opusStream) SampleRate() int { return opusRate }
func (s *opusStream) Channels() int   { return 2 }

func (s *opusStream) Len() int64 { return s.frames }

func (s *opusStream) Read(dst []float32) (int, error) {
	want := len(dst) / 2
	if want == 0 {
		return 0, nil
	}
	for len(s.pend)-s.at < want*2 {
		pkt, ok := s.nextPacket()
		if !ok {
			if len(s.pend) == s.at {
				return 0, io.EOF
			}
			break
		}
		s.decode(pkt)
	}
	n := min(want*2, len(s.pend)-s.at)
	copy(dst, s.pend[s.at:s.at+n])
	// The frame returned first is as far back as what's left of pend.
	s.spec.feed(max(s.gpos-int64(len(s.pend)-s.at)/2-s.preSkip, 0), dst[:n])
	if s.at += n; s.at == len(s.pend) {
		s.pend, s.at = s.pend[:0], 0
	}
	return n, nil
}

// decode decodes a packet into pend, as stereo, dropping the frames before
// the one asked for and those past the end of the sound.
func (s *opusStream) decode(pkt []byte) {
	n, err := s.dec.DecodeToFloat32(pkt, s.buf)
	if err != nil || n <= 0 {
		return // a packet gone bad: on to the next
	}
	if s.ch == 1 {
		// In place and from the end, so as not to write over what's left.
		for i := n - 1; i >= 0; i-- {
			s.buf[2*i], s.buf[2*i+1] = s.buf[i], s.buf[i]
		}
	}
	out := s.buf[:n*2]
	if s.gain != 1 {
		for i, v := range out {
			out[i] = v * s.gain
		}
	}
	if s.skip > 0 {
		drop := min(s.skip, int64(n))
		out, s.skip, s.gpos = out[drop*2:], s.skip-drop, s.gpos+drop
	}
	if s.frames >= 0 {
		if over := s.gpos - s.preSkip + int64(len(out)/2) - s.frames; over > 0 {
			out = out[:max(int64(len(out))-over*2, 0)]
		}
	}
	s.pend = append(s.pend, out...)
	s.gpos += int64(len(out) / 2)
}

// nextPacket returns the next packet of the stream, reading pages as it needs
// them, and says false at its end.
func (s *opusStream) nextPacket() ([]byte, bool) {
	for len(s.packets) == 0 {
		if !s.readPage() {
			return nil, false
		}
	}
	pkt := s.packets[0]
	s.packets = s.packets[1:]
	return pkt, true
}

// readPage reads the page at off and its packets, joining a packet that ran
// over from the page before, and says false at the end of the stream.
func (s *opusStream) readPage() bool {
	for {
		p, ok := oggPageAt(s.dl, s.off)
		if !ok {
			return false
		}
		if p.serial != s.serial {
			s.off = p.off + p.n
			continue
		}
		if cap(s.page) < int(p.n) {
			s.page = make([]byte, p.n)
		}
		b := s.page[:p.n]
		if _, err := (&downloadFile{dl: s.dl}).ReadAt(b, p.off); err != nil {
			return false
		}
		s.off = p.off + p.n
		segs, data := b[27:27+p.nsegs], b[27+p.nsegs:]
		if s.dropCont {
			s.dropCont = false
			if p.flags&1 != 0 {
				segs, data = dropContinued(segs, data)
			}
		}
		s.packets, s.partial = splitPackets(segs, data, s.partial)
		if s.headLeft > 0 {
			n := min(s.headLeft, len(s.packets))
			s.packets, s.headLeft = s.packets[n:], s.headLeft-n
		}
		// The last page says where the sound ends, and so how long it is,
		// for a stream whose source said nothing.
		if s.frames < 0 && p.granule >= 0 && s.lastPage(s.off) {
			s.frames = max(p.granule-s.preSkip, 0)
		}
		if len(s.packets) > 0 {
			return true
		}
	}
}

// lastPage says whether the page ending at off is the stream's last: no more
// bytes are in, and none are coming.
func (s *opusStream) lastPage(off int64) bool {
	n, done := s.dl.received()
	return off >= n && done
}

// SetPosition goes to the frame, starting over at the page that holds it and
// dropping what comes before it. That waits for the rest of the download.
func (s *opusStream) SetPosition(frame int64) error {
	frame = max(frame, 0)
	if s.frames >= 0 {
		frame = min(frame, s.frames)
	}
	s.index()
	if len(s.pages) == 0 {
		return errors.New("opus: no pages")
	}
	target := frame + s.preSkip
	i, found := slices.BinarySearchFunc(s.pages, target, func(p oggPage, t int64) int {
		return cmp.Compare(p.granule, t)
	})
	if !found {
		i--
	}
	i = max(i, s.audio)
	// The page's sound begins where the page before it ended.
	var start int64
	if i > 0 {
		start = s.pages[i-1].granule
	}
	s.off, s.gpos, s.skip = s.pages[i].off, start, target-start
	s.packets, s.partial, s.pend, s.at = nil, nil, s.pend[:0], 0
	s.dropCont, s.headLeft = i != s.audio, 0
	if i == s.audio {
		s.headLeft = s.headPkts
	}
	// The packets decode apart from those before, whose sound overlaps them
	// by a frame at most, which the first dropped frames cover.
	return s.dec.Init(opusRate, s.ch)
}

func (s *opusStream) Close() error {
	s.once.Do(s.drop)
	return nil
}

func (s *opusStream) source() (*download, *spectrum) { return s.dl, s.spec }

// splitPackets splits a page's segments into the packets they hold, beginning
// the first with partial, the start of a packet the page before left
// unfinished. It returns the start of the packet this page leaves unfinished,
// if it leaves one.
func splitPackets(segs, data, partial []byte) ([][]byte, []byte) {
	var pkts [][]byte
	begin, at := 0, 0
	for _, l := range segs {
		at += int(l)
		if l == 255 {
			continue // the packet goes on
		}
		pkt := data[begin:at]
		if partial != nil {
			pkt = append(append([]byte(nil), partial...), pkt...)
			partial = nil
		}
		pkts = append(pkts, pkt)
		begin = at
	}
	if begin == len(data) {
		return pkts, nil
	}
	rest := data[begin:]
	if partial != nil {
		rest = append(append([]byte(nil), partial...), rest...)
	}
	return pkts, rest
}

// dropContinued drops the segments of a page that finish a packet begun on
// the page before, which is no packet of its own: a seek lands in the middle
// of one.
func dropContinued(segs, data []byte) ([]byte, []byte) {
	n := 0
	for len(segs) > 0 {
		l := segs[0]
		segs, n = segs[1:], n+int(l)
		if l < 255 {
			break
		}
	}
	return segs, data[min(n, len(data)):]
}
