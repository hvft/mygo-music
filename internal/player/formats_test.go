package player

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// seekLands checks that a seek to each frame plays what decoding straight
// through has there, to within tol.
func seekLands(t *testing.T, b []byte, ref []float32, tol float64, frames ...int64) {
	t.Helper()
	s, err := openStream(downloaded(b), 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 4096)
	for _, frame := range frames {
		if err := s.SetPosition(frame); err != nil {
			t.Fatal(err)
		}
		n, err := s.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if d := maxDiff(buf[:n], ref[frame*2:]); d > tol {
			t.Errorf("seek to %d: off by up to %.4f", frame, d)
		}
	}
}

// snr is how far below ref the difference between got and ref is, in dB.
func snr(got, ref []float32) float64 {
	var sig, noise float64
	for i := range min(len(got), len(ref)) {
		sig += float64(ref[i]) * float64(ref[i])
		d := float64(got[i] - ref[i])
		noise += d * d
	}
	return 10 * math.Log10(sig/noise)
}

// An ALAC .m4a plays exactly the samples it was encoded from, the first two
// seconds of seek.flac, and lands on the frame it's asked for.
func TestALAC(t *testing.T) {
	b := readFixture(t, "seek.m4a")
	got := decodeAll(t, b)
	flac := decodeAll(t, readFixture(t, "seek.flac"))
	if len(got) != 2*44100*2 {
		t.Fatalf("decoded %d samples", len(got))
	}
	if d := maxDiff(got, flac); d != 0 {
		t.Fatalf("off from the FLAC by up to %g", d)
	}
	seekLands(t, b, got, 0, 0, 1, 4095, 4096, 50000, 88199)
}

// An AAC .m4a drops the encoder's priming, as its edit list says, so it lines
// up with what it was encoded from and is as long. A seek decodes the packet
// before, whose sound overlaps the one sought to.
func TestAACInMP4(t *testing.T) {
	b := readFixture(t, "seek-aac.m4a")
	s, err := openStream(downloaded(b), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleRate() != 44100 || s.Len() != 3*44100 {
		t.Fatalf("rate %d, length %d", s.SampleRate(), s.Len())
	}
	got := decodeAll(t, b)
	if len(got) != 3*44100*2 {
		t.Fatalf("decoded %d samples", len(got))
	}
	if db := snr(got, decodeAll(t, readFixture(t, "seek.flac"))); db < 20 {
		t.Errorf("%.1f dB from the source", db)
	}
	// Noise the encoder substituted is random each time it's decoded.
	seekLands(t, b, got, 0.05, 0, 1, 4096, 50000, 123457)
}

// An AAC .m4a cut into fragments, as a stream for DASH is, plays what the
// whole one does. Its movie header has no edit list, so the encoder's priming
// plays too: the fragments are what the whole one has after it.
func TestFragmentedAAC(t *testing.T) {
	b := readFixture(t, "seek-aac-frag.m4a")
	got := decodeAll(t, b)
	whole := decodeAll(t, readFixture(t, "seek-aac.m4a"))
	const prime = 1024
	if len(got) < len(whole)+prime*2 {
		t.Fatalf("decoded %d samples, the whole file %d", len(got), len(whole))
	}
	if db := snr(got[prime*2:], whole); db < 20 {
		t.Errorf("%.1f dB from the whole file", db)
	}
	seekLands(t, b, got, 0.05, 0, 1, 4096, 50000, 123457)
}

// AAC's packets each decode to 1024 frames, whatever its container says they
// last: a seek lands where decoding straight through has the frame. Some sites'
// files have a packet near the start a few frames shorter, which put every
// seek past it that much off, as good as other audio for a second after.
func TestAACPacketsOfOtherDurations(t *testing.T) {
	b := bytes.Clone(readFixture(t, "seek-aac-frag.m4a"))
	// The first fragment's packets last 1016 frames, its header says.
	at := bytes.Index(b, []byte("tfhd")) + 4 + 8
	if binary.BigEndian.Uint32(b[at:]) != 1024 {
		t.Fatal("no default duration where expected")
	}
	binary.BigEndian.PutUint32(b[at:], 1016)
	got := decodeAll(t, b)
	for _, frame := range []int64{50000, 100000, 100001} {
		s, err := openStream(downloaded(b), 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetPosition(frame); err != nil {
			t.Fatal(err)
		}
		var read []float32
		buf := make([]float32, 4096)
		for len(read) < 44100 {
			n, err := s.Read(buf)
			read = append(read, buf[:n]...)
			if err != nil {
				break
			}
		}
		if db := snr(read, got[frame*2:]); db < 20 {
			t.Errorf("seek to %d: %.1f dB from decoding straight through", frame, db)
		}
	}
}

// A fragmented .m4a starts playing with only its first fragment in, its length
// from its index, as a stream from a site serving DASH comes in.
func TestFragmentedComingIn(t *testing.T) {
	b := readFixture(t, "seek-aac-frag.m4a")
	got := decodeAll(t, b)
	dl := downloaded(b)
	dl.n, dl.done = 10<<10, false
	s, err := openStream(dl, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len()*2 != int64(len(got)) {
		t.Fatalf("length %d, though %d frames decode", s.Len(), len(got)/2)
	}
	buf := make([]float32, 4096)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if d := maxDiff(buf[:n], got); d > 0.05 {
		t.Errorf("off by up to %.4f", d)
	}
}

// FLAC in a fragmented .m4a, as a lossless DASH stream has it, decodes to the
// very samples of the FLAC file, and lands on the frame it's asked for.
func TestFragmentedFLAC(t *testing.T) {
	b := readFixture(t, "seek-flac-frag.m4a")
	got := decodeAll(t, b)
	flac := decodeAll(t, readFixture(t, "seek.flac"))
	if len(got) != len(flac) {
		t.Fatalf("decoded %d samples, the FLAC %d", len(got), len(flac))
	}
	if d := maxDiff(got, flac); d != 0 {
		t.Fatalf("off from the FLAC by up to %g", d)
	}
	seekLands(t, b, got, 0, 0, 1, 4095, 4096, 50000, 88199)
}

// An AAC file of ADTS frames, whole, knows its length from them; one still
// coming in plays with the length its source says, and a seek past what's
// been found finds the frames up to there.
func TestADTS(t *testing.T) {
	b := readFixture(t, "seek.aac")
	s, err := openStream(downloaded(b), 0)
	if err != nil {
		t.Fatal(err)
	}
	frames := s.Len()
	if s.SampleRate() != 44100 || frames%1024 != 0 || frames < 3*44100 {
		t.Fatalf("rate %d, length %d", s.SampleRate(), frames)
	}
	got := decodeAll(t, b)
	if int64(len(got)) != frames*2 {
		t.Fatalf("decoded %d samples of %d", len(got), frames*2)
	}
	seekLands(t, b, got, 0.05, 0, 1, 4096, 50000, 123457)

	dl := downloaded(b)
	dl.done = false
	s, err = openStream(dl, 3000)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3*44100 {
		t.Fatalf("length %d: not what the source said", s.Len())
	}
	if err := s.SetPosition(123457); err != nil {
		t.Fatal(err)
	}
	buf := make([]float32, 4096)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if d := maxDiff(buf[:n], got[123457*2:]); d > 0.05 {
		t.Errorf("off by up to %.4f", d)
	}
}

// An Ogg Opus file plays at the 48 kHz Opus decodes at, and as long as its
// last page says its sound is: the frames its granule counts, less the padding
// the encoder put before it, and none of what it put after. A seek lands on
// the frame it's asked for.
func TestOpus(t *testing.T) {
	b := readFixture(t, "seek.opus")
	s, err := openStream(downloaded(b), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleRate() != 48000 || s.Len() != 5*48000 {
		t.Fatalf("rate %d, length %d", s.SampleRate(), s.Len())
	}
	got := decodeAll(t, b)
	if len(got) != 5*48000*2 {
		t.Fatalf("decoded %d samples", len(got))
	}
	// What decodes is the song, not the silence of a decoder that failed.
	var peak float32
	for _, v := range got {
		peak = max(peak, float32(math.Abs(float64(v))))
	}
	if peak < 0.3 {
		t.Fatalf("peak %f", peak)
	}
	seekLands(t, b, got, 0.001, 0, 1, 4096, 50000, 123457)
}

// A mono Opus file plays in both ears.
func TestMonoOpus(t *testing.T) {
	got := decodeAll(t, readFixture(t, "seek-mono.opus"))
	if len(got) != 5*48000*2 {
		t.Fatalf("decoded %d samples", len(got))
	}
	for i := 0; i < len(got); i += 2 {
		if got[i] != got[i+1] {
			t.Fatalf("frame %d: %f and %f", i/2, got[i], got[i+1])
		}
	}
}

// An Opus file still coming in plays right away, with the length its source
// says, and plays the same sound as one whose pages are all in.
func TestOpusComingIn(t *testing.T) {
	b := readFixture(t, "seek.opus")
	got := decodeAll(t, b)
	dl := downloaded(b)
	dl.n, dl.done = 30<<10, false
	s, err := openStream(dl, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 5*48000 {
		t.Fatalf("length %d: not what the source said", s.Len())
	}
	buf := make([]float32, 4096)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if d := maxDiff(buf[:n], got); d > 0.05 {
		t.Errorf("off by up to %.4f", d)
	}
}

// wavFile makes a WAV of the format tag, channels and bits given, holding
// data, with a LIST chunk before it as some writers put there.
func wavFile(tag, channels, bits, rate int, data []byte) []byte {
	align := channels * ((bits + 7) / 8)
	le16 := func(v int) []byte { return binary.LittleEndian.AppendUint16(nil, uint16(v)) }
	le32 := func(v int) []byte { return binary.LittleEndian.AppendUint32(nil, uint32(v)) }
	var b []byte
	b = append(b, "RIFF"...)
	b = append(b, le32(0)...)
	b = append(b, "WAVE"...)
	b = append(b, "fmt "...)
	b = append(b, le32(16)...)
	b = append(b, le16(tag)...)
	b = append(b, le16(channels)...)
	b = append(b, le32(rate)...)
	b = append(b, le32(rate*align)...)
	b = append(b, le16(align)...)
	b = append(b, le16(bits)...)
	b = append(b, "LIST"...)
	b = append(b, le32(5)...)
	b = append(b, "INFOx\x00"...) // odd, so padded
	b = append(b, "data"...)
	b = append(b, le32(len(data))...)
	b = append(b, data...)
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	return b
}

// A WAV plays its samples as they are, of whichever kind, mono in both ears,
// and a seek goes to the very frame.
func TestWAV(t *testing.T) {
	const n = 3000
	wave := func(i int) float64 { return 0.8 * math.Sin(float64(i)/7) }
	var s16, s24, f32, u8 []byte
	for i := range n {
		l, r := wave(i), -wave(i)
		s16 = binary.LittleEndian.AppendUint16(s16, uint16(int16(l*32767)))
		s16 = binary.LittleEndian.AppendUint16(s16, uint16(int16(r*32767)))
		v := int32(l * (1<<23 - 1))
		s24 = append(s24, byte(v), byte(v>>8), byte(v>>16))
		f32 = binary.LittleEndian.AppendUint32(f32, math.Float32bits(float32(l)))
		f32 = binary.LittleEndian.AppendUint32(f32, math.Float32bits(float32(r)))
		u8 = append(u8, byte(int(l*127)+128))
	}
	tests := []struct {
		name  string
		file  []byte
		mono  bool
		right float64 // the right channel, of the left's
		tol   float64
	}{
		{"16-bit stereo", wavFile(1, 2, 16, 44100, s16), false, -1, 2.0 / 32767},
		{"24-bit mono", wavFile(1, 1, 24, 48000, s24), true, 1, 1.0 / (1 << 22)},
		{"float stereo", wavFile(3, 2, 32, 44100, f32), false, -1, 1e-7},
		{"8-bit mono", wavFile(1, 1, 8, 8000, u8), true, 1, 2.0 / 127},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeAll(t, tt.file)
			if len(got) != n*2 {
				t.Fatalf("decoded %d samples", len(got))
			}
			for i := range n {
				if d := math.Abs(float64(got[2*i]) - wave(i)); d > tt.tol {
					t.Fatalf("frame %d: left %f, want %f", i, got[2*i], wave(i))
				}
				if d := math.Abs(float64(got[2*i+1]) - tt.right*wave(i)); d > tt.tol {
					t.Fatalf("frame %d: right %f, want %f", i, got[2*i+1], tt.right*wave(i))
				}
			}
			seekLands(t, tt.file, got, 0, 0, 1, 1234, 2999)
		})
	}
}

// A WAV written as it's sent, saying nothing of its data's length, plays to
// the end of the file.
func TestWAVOfUnknownLength(t *testing.T) {
	b := wavFile(1, 2, 16, 44100, make([]byte, 4000))
	binary.LittleEndian.PutUint32(b[len(b)-4000-4:], 0)
	if got := decodeAll(t, b); len(got) != 2000 {
		t.Fatalf("decoded %d samples", len(got))
	}
}
