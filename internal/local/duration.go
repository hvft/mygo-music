package local

import (
	"encoding/binary"
	"io"

	"github.com/jfreymuth/oggvorbis"
)

// Tags don't say how long a song is, and decoding it all to find out would make
// a scan take as long as the music does. MP3 and FLAC say near their start, and
// Ogg on its last page.

// id3v2Size is how many bytes an ID3v2 tag at the start takes, if there's one.
func id3v2Size(r io.ReaderAt) int64 {
	var h [10]byte
	if _, err := r.ReadAt(h[:], 0); err != nil || string(h[:3]) != "ID3" {
		return 0
	}
	size := int64(h[6]&0x7f)<<21 | int64(h[7]&0x7f)<<14 | int64(h[8]&0x7f)<<7 | int64(h[9]&0x7f)
	size += 10
	if h[5]&0x10 != 0 { // a footer
		size += 10
	}
	return size
}

// flacDuration reads the milliseconds in STREAMINFO, which is always the first
// block, or 0 if it doesn't say.
func flacDuration(r io.ReaderAt) int64 {
	var b [4 + 4 + 18]byte
	if _, err := r.ReadAt(b[:], id3v2Size(r)); err != nil || string(b[:4]) != "fLaC" || b[4]&0x7f != 0 {
		return 0
	}
	info := b[8:]
	rate := int64(info[10])<<12 | int64(info[11])<<4 | int64(info[12])>>4
	samples := int64(info[13]&0x0f)<<32 | int64(binary.BigEndian.Uint32(info[14:18]))
	if rate == 0 {
		return 0
	}
	return samples * 1000 / rate
}

// frameHeader is what an MP3 frame's first four bytes say.
type frameHeader struct {
	version int // 1 or 2, and 25 for MPEG 2.5
	layer   int
	bitRate int // in kilobits a second
	rate    int // in samples a second
	mono    bool
	samples int // in the frame
	length  int // of the frame, in bytes
}

var mp3BitRates = [5][15]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}, // MPEG 1, layer I
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},    // layer II
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},     // layer III
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},    // MPEG 2 and 2.5, layer I
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},         // layers II and III
}

var mp3Rates = map[int][3]int{
	1:  {44100, 48000, 32000},
	2:  {22050, 24000, 16000},
	25: {11025, 12000, 8000},
}

// parseFrameHeader reads a frame's header, if b starts with one. Free-format
// frames count as none: they don't say their bit rate, so nothing can be told
// from them.
func parseFrameHeader(b []byte) (frameHeader, bool) {
	var h frameHeader
	if len(b) < 4 || b[0] != 0xff || b[1]&0xe0 != 0xe0 {
		return h, false
	}
	switch b[1] >> 3 & 3 {
	case 0:
		h.version = 25
	case 2:
		h.version = 2
	case 3:
		h.version = 1
	default:
		return h, false
	}
	h.layer = 4 - int(b[1]>>1&3)
	bitIndex, rateIndex := int(b[2]>>4), int(b[2]>>2&3)
	if h.layer == 4 || bitIndex == 0 || bitIndex == 15 || rateIndex == 3 {
		return h, false
	}
	table := h.layer - 1
	if h.version != 1 {
		table = 3
		if h.layer != 1 {
			table = 4
		}
	}
	h.bitRate = mp3BitRates[table][bitIndex]
	h.rate = mp3Rates[h.version][rateIndex]
	h.mono = b[3]>>6 == 3
	pad := int(b[2] >> 1 & 1)
	switch {
	case h.layer == 1:
		h.samples = 384
		h.length = (12000*h.bitRate/h.rate + pad) * 4
	case h.layer == 3 && h.version != 1:
		h.samples = 576
		h.length = 72000*h.bitRate/h.rate + pad
	default:
		h.samples = 1152
		h.length = 144000*h.bitRate/h.rate + pad
	}
	return h, true
}

// mp3Search is how far past the tag the audio may start, after what some
// taggers leave there.
const mp3Search = 128 << 10

// mp3Duration reads an MP3's milliseconds from the first frame: a VBR file says
// how many frames it has there, and otherwise all frames are alike, so its
// size tells. It answers 0 if no frame is found.
func mp3Duration(r io.ReaderAt, size int64) int64 {
	start := id3v2Size(r)
	if start >= size {
		return 0
	}
	buf := make([]byte, min(mp3Search, size-start))
	n, err := r.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return 0
	}
	buf = buf[:n]
	for i := 0; i+4 <= len(buf); i++ {
		h, ok := parseFrameHeader(buf[i:])
		if !ok || !followed(buf, i, h) {
			continue
		}
		if frames := countedFrames(buf[i:], h); frames > 0 {
			return frames * int64(h.samples) * 1000 / int64(h.rate)
		}
		audio := size - start - int64(i)
		var last [3]byte
		if _, err := r.ReadAt(last[:], size-128); err == nil && string(last[:]) == "TAG" {
			audio -= 128
		}
		return audio * 8 / int64(h.bitRate)
	}
	return 0
}

// followed reports whether the frame at i has a like one after it, as a real
// one does, and a chance pair of bytes doesn't.
func followed(buf []byte, i int, h frameHeader) bool {
	next := i + h.length
	if next+4 > len(buf) {
		return true // nothing there to tell by
	}
	n, ok := parseFrameHeader(buf[next:])
	return ok && n.version == h.version && n.layer == h.layer && n.rate == h.rate
}

// countedFrames reads the count of frames in a Xing, Info or VBRI header, which
// takes the place of the audio in the first frame: 0 if it has none.
func countedFrames(frame []byte, h frameHeader) int64 {
	side := 32 // Xing goes after the side information
	switch {
	case h.version == 1 && h.mono:
		side = 17
	case h.version != 1 && h.mono:
		side = 9
	case h.version != 1:
		side = 17
	}
	if at := 4 + side; len(frame) >= at+12 {
		if tag := string(frame[at : at+4]); tag == "Xing" || tag == "Info" {
			const hasFrames = 1
			if binary.BigEndian.Uint32(frame[at+4:])&hasFrames != 0 {
				return int64(binary.BigEndian.Uint32(frame[at+8:]))
			}
		}
	}
	const vbri = 4 + 32 // always here
	if len(frame) >= vbri+18 && string(frame[vbri:vbri+4]) == "VBRI" {
		return int64(binary.BigEndian.Uint32(frame[vbri+14:]))
	}
	return 0
}

// oggDuration reads an Ogg Vorbis file's milliseconds from its last page, or 0
// if it isn't one, or is one the decoder panics on.
func oggDuration(r io.ReaderAt, size int64) (ms int64) {
	defer func() {
		if recover() != nil {
			ms = 0
		}
	}()
	d, err := oggvorbis.NewReader(io.NewSectionReader(r, 0, size))
	if err != nil || d.SampleRate() == 0 {
		return 0
	}
	return d.Length() * 1000 / int64(d.SampleRate())
}

// opusDuration reads an Ogg Opus file's milliseconds from its last page,
// whose granule position is where the sound ends: 48 kHz frames from the
// start, less the padding the encoder put before it.
func opusDuration(r io.ReaderAt, size int64) int64 {
	g, ok := lastOggGranule(r, size)
	if !ok {
		return 0
	}
	if pre := opusPreSkip(r); g > pre {
		return (g - pre) * 1000 / 48000
	}
	return 0
}

// opusPreSkip is the frames at 48 kHz an Ogg Opus file's encoder put before
// the sound, as its first page says.
func opusPreSkip(r io.ReaderAt) int64 {
	if h := opusHead(r); h != nil {
		return int64(binary.LittleEndian.Uint16(h[10:12]))
	}
	return 0
}

// lastOggGranule finds the granule position of an Ogg file's last page, which
// ends where the file does. A page is at most 27 + 255 + 255*255 bytes, so the
// last of them starts within that much of the end.
func lastOggGranule(r io.ReaderAt, size int64) (int64, bool) {
	n := min(size, int64(27+255+255*255))
	b := make([]byte, n)
	if _, err := r.ReadAt(b, size-n); err != nil && err != io.EOF {
		return 0, false
	}
	for i := len(b) - 27; i >= 0; i-- {
		if string(b[i:i+4]) != "OggS" || b[i+4] != 0 {
			continue
		}
		segs := int(b[i+26])
		if i+27+segs > len(b) {
			continue
		}
		length := 27 + segs
		for _, l := range b[i+27 : i+27+segs] {
			length += int(l)
		}
		if i+length != len(b) {
			continue // not where the file ends: bytes that only look like a page
		}
		if g := int64(binary.LittleEndian.Uint64(b[i+6 : i+14])); g >= 0 {
			return g, true
		}
	}
	return 0, false
}
