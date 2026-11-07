package local

import (
	"bufio"
	"encoding/binary"
	"io"
	"strconv"
	"strings"

	_ "github.com/egoist/mygo/audio/aac"  // AAC in MP4
	_ "github.com/egoist/mygo/audio/flac" // FLAC in MP4
	"github.com/egoist/mygo/audio/mp4"
)

// A WAV is chunks of RIFF: its format, its data, and maybe its tags, as an
// ID3 chunk or a LIST of INFO.

// wavChunks calls each with each chunk's ID, where its body is and how long it
// is, until each says to stop.
func wavChunks(r io.ReaderAt, size int64, each func(id string, off, n int64) bool) {
	var h [12]byte
	if _, err := r.ReadAt(h[:], 0); err != nil || string(h[:4]) != "RIFF" && string(h[:4]) != "RF64" || string(h[8:12]) != "WAVE" {
		return
	}
	for off := int64(12); off+8 <= size; {
		if _, err := r.ReadAt(h[:8], off); err != nil {
			return
		}
		n := int64(binary.LittleEndian.Uint32(h[4:8]))
		if n == 0xffffffff || n > size-off-8 {
			n = size - off - 8 // an RF64's data, or one cut short
		}
		if !each(string(h[:4]), off+8, n) {
			return
		}
		off += 8 + n + n&1
	}
}

// wavDuration is how long a WAV's data plays, in milliseconds.
func wavDuration(r io.ReaderAt, size int64) int64 {
	var rate, data int64
	wavChunks(r, size, func(id string, off, n int64) bool {
		switch id {
		case "fmt ":
			var b [12]byte
			if n >= 12 {
				if _, err := r.ReadAt(b[:], off); err == nil {
					rate = int64(binary.LittleEndian.Uint32(b[8:])) // bytes a second
				}
			}
		case "data":
			data = n
			return false
		}
		return true
	})
	if rate <= 0 {
		return 0
	}
	return data * 1000 / rate
}

// wavID3 finds a WAV's ID3 chunk, as a tag of its own to read.
func wavID3(r io.ReaderAt, size int64) *io.SectionReader {
	var found *io.SectionReader
	wavChunks(r, size, func(id string, off, n int64) bool {
		if strings.EqualFold(id, "id3 ") {
			found = io.NewSectionReader(r, off, n)
			return false
		}
		return true
	})
	return found
}

// wavInfo reads a WAV's LIST of INFO: the tags most programs that write WAV
// write, if any.
func wavInfo(r io.ReaderAt, size int64) (t tags) {
	wavChunks(r, size, func(id string, off, n int64) bool {
		if id != "LIST" || n < 4 || n > 1<<20 {
			return true
		}
		b := make([]byte, n)
		if _, err := r.ReadAt(b, off); err != nil || string(b[:4]) != "INFO" {
			return true
		}
		for b = b[4:]; len(b) >= 8; {
			field := string(b[:4])
			l := int(binary.LittleEndian.Uint32(b[4:8]))
			if l > len(b)-8 {
				break
			}
			v := cleanTag(string(b[8 : 8+l]))
			switch field {
			case "INAM":
				t.title = v
			case "IART":
				t.artist = v
			case "IPRD":
				t.album = v
			case "ITRK", "IPRT", "TRCK":
				t.track, _ = strconv.Atoi(strings.SplitN(v, "/", 2)[0])
			case "ICRD":
				if len(v) >= 4 {
					t.year, _ = strconv.Atoi(v[:4])
				}
			}
			b = b[min(8+l+l&1, len(b)):]
		}
		return false
	})
	return t
}

// adtsDuration walks an AAC file's ADTS headers, without decoding the sound:
// each frame holds 1024 samples, at the rate it says.
func adtsDuration(r io.ReaderAt, size int64) int64 {
	rates := [...]int64{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}
	start := id3v2Size(r)
	br := bufio.NewReaderSize(io.NewSectionReader(r, start, size-start), 64<<10)
	var micros int64
	for {
		h, err := br.Peek(7)
		if err != nil || h[0] != 0xff || h[1]&0xf6 != 0xf0 {
			break
		}
		rate := int(h[2] >> 2 & 15)
		length := int(h[3]&3)<<11 | int(h[4])<<3 | int(h[5]>>5)
		if rate >= len(rates) || length < 7 {
			break
		}
		micros += 1024 * int64(1+h[6]&3) * 1000000 / rates[rate]
		if _, err := br.Discard(length); err != nil {
			break
		}
	}
	return micros / 1000
}

// adtsPlayable says whether an AAC file is AAC-LC, as the player decodes:
// what its first frame says.
func adtsPlayable(r io.ReaderAt) bool {
	var h [7]byte
	if _, err := r.ReadAt(h[:], id3v2Size(r)); err != nil {
		return false
	}
	return h[0] == 0xff && h[1]&0xf6 == 0xf0 && h[2]>>6 == 1
}

// m4aTrack reads an .m4a's sound track, if the player plays it: ALAC, FLAC
// or AAC-LC.
func m4aTrack(r io.ReaderAt, size int64) (*mp4.Track, bool) {
	t, err := mp4.ReadTrack(r, size)
	if err != nil || !t.Decodable() {
		return nil, false
	}
	return t, true
}

// opusHead reads an Ogg Opus file's OpusHead, the packet of its first page,
// which says its channels and the padding before its sound: nil if the file
// isn't one.
func opusHead(r io.ReaderAt) []byte {
	var h [27]byte
	if _, err := r.ReadAt(h[:], 0); err != nil || string(h[:4]) != "OggS" {
		return nil
	}
	head := make([]byte, 19)
	if _, err := r.ReadAt(head, 27+int64(h[26])); err != nil || string(head[:8]) != "OpusHead" {
		return nil
	}
	return head
}

// lossless says whether an .m4a's track is ALAC's or FLAC's.
func lossless(t *mp4.Track) bool { return t.Codec == "alac" || t.Codec == "fLaC" }
