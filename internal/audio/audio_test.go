package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fixtureFrames   = 2 * targetRate
	testLimit       = 20 * time.Minute
	toneHz          = 440
	toneAmplitude   = 0.5
	toneSeconds     = 1
	rmsTolerance    = 0.02
	maxLagFrames    = targetRate / 10
	mp3PaddingLimit = targetRate / 10
	wavFormatALaw   = 6
	wavFormatPCMTag = formatPCM
	pcm8Bits        = 8
	threeChannels   = 3
	highRate        = 96000
	narrowbandRate  = 8000
	longWAVSeconds  = 1300
	lowRateSeconds  = 1
	unsupportedText = "unsupported"
	corruptedText   = "corrupted"
	noAudioText     = "no audio"
	chainedText     = "chained or multiplexed Ogg"
	fixtureSeconds  = 2

	testSerial        = 7
	oggFirstPage      = 0x02
	oggLastPage       = 0x04
	fixturePreSkip    = 312
	granuleShift      = opusRate
	floodBytes        = 25 << 20
	floodLimit        = time.Minute
	floodDeadline     = 6 * time.Second
	floodBudget       = 5 * time.Second
	packetsPerPage    = 255
	maxPageSegments   = 255
	giantPacketBytes  = 16 << 20
	giantPacketBudget = 10 << 20
	id3AllocBudget    = 1 << 20
	id3PaddingBytes   = 100
)

// A two-byte Opus packet holding six 20 ms CELT frames: 120 ms of audio,
// the most a packet may carry, for the least input.
var tinyOpusPacket = []byte{0xBB, 0x06}

var noGranule uint64 = math.MaxUint64

// A 50-byte MPEG-2 Layer III frame that makes go-mp3 v0.3.4 index out of
// range in getScaleFactorsMpeg2; found by fuzzing the library directly.
const panickingMP3 = "\xff\xf3 0000000\xd800000000000000000000000000000000000000000"

var limits = Limits{MaxDuration: testLimit}

type decoded struct {
	rate     uint32
	channels uint16
	bits     uint16
	format   uint16
	samples  []int16
}

func parseOutput(t *testing.T, wav []byte) decoded {
	t.Helper()

	require.GreaterOrEqual(t, len(wav), wavHeaderBytes)
	require.Equal(t, riffMagic, string(wav[:4]))
	require.Equal(t, waveMagic, string(wav[8:12]))
	require.Equal(t, fmtChunkID, string(wav[12:16]))
	require.Equal(t, dataChunkID, string(wav[36:40]))
	require.Equal(t, len(wav)-riffHeaderBytes, int(binary.LittleEndian.Uint32(wav[4:])), "RIFF size")
	require.Equal(t, len(wav)-wavHeaderBytes, int(binary.LittleEndian.Uint32(wav[40:])), "data size")

	samples := make([]int16, (len(wav)-wavHeaderBytes)/sampleBytes)
	_, err := binary.Decode(wav[wavHeaderBytes:], binary.LittleEndian, samples)
	require.NoError(t, err)

	return decoded{
		format:   binary.LittleEndian.Uint16(wav[20:]),
		channels: binary.LittleEndian.Uint16(wav[22:]),
		rate:     binary.LittleEndian.Uint32(wav[24:]),
		bits:     binary.LittleEndian.Uint16(wav[34:]),
		samples:  samples,
	}
}

func normalise(t *testing.T, data []byte) decoded {
	t.Helper()

	wav, err := NormaliseUpload(t.Context(), bytes.NewReader(data), limits)
	require.NoError(t, err)

	out := parseOutput(t, wav)
	require.Equal(t, decoded{format: formatPCM, channels: targetChannels, rate: targetRate, bits: targetBits},
		decoded{format: out.format, channels: out.channels, rate: out.rate, bits: out.bits})

	return out
}

func fixture(t testing.TB, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return data
}

type wavSpec struct {
	tag        int
	extensible bool
	rate       int
	bits       int
	channels   [][]float32
}

func oneByte(value int) byte {
	if value < 0 || value > math.MaxUint8 {
		panic("value does not fit in a byte")
	}

	return byte(value)
}

func le16(data []byte, value int) []byte {
	if value < 0 || value > math.MaxUint16 {
		panic("value does not fit in 16 bits")
	}

	return binary.LittleEndian.AppendUint16(data, uint16(value))
}

func le32(data []byte, value int) []byte {
	if value < 0 || value > math.MaxUint32 {
		panic("value does not fit in 32 bits")
	}

	return binary.LittleEndian.AppendUint32(data, uint32(value))
}

func (s wavSpec) bytes() []byte {
	channels := len(s.channels)
	width := s.bits / bitsPerByte
	frames := len(s.channels[0])

	var data []byte
	for frame := range frames {
		for _, channel := range s.channels {
			data = appendSample(data, s.tag, s.bits, channel[frame])
		}
	}

	tag := s.tag
	if s.extensible {
		tag = wavFormatExtensible
	}
	format := le16(nil, tag)
	format = le16(format, channels)
	format = le32(format, s.rate)
	format = le32(format, s.rate*channels*width)
	format = le16(format, channels*width)
	format = le16(format, s.bits)
	if s.extensible {
		format = le16(format, extensibleBytes-minFormatBytes-2)
		format = le16(format, s.bits)
		format = le32(format, 0)
		format = le16(format, s.tag)
		format = append(format, extensibleGUIDTail...)
	}

	wav := []byte(riffMagic)
	wav = le32(wav, len(waveMagic)+chunkHeaderBytes+len(format)+chunkHeaderBytes+len(data))
	wav = append(wav, waveMagic...)
	wav = append(wav, fmtChunkID...)
	wav = le32(wav, len(format))
	wav = append(wav, format...)
	wav = append(wav, dataChunkID...)
	wav = le32(wav, len(data))

	return append(wav, data...)
}

const (
	pcm8Scale  = 127
	pcm8Offset = 128
)

func appendSample(data []byte, tag, bits int, value float32) []byte {
	scaled := float64(value)
	switch {
	case tag == wavFormatFloat:
		return binary.LittleEndian.AppendUint32(data, math.Float32bits(value))
	case bits == pcm24Bits:
		sample, err := binary.Append(nil, binary.LittleEndian, int32(math.Round(scaled*pcm24Scale)))
		if err != nil {
			panic(err)
		}
		return append(data, sample[:pcm24Bits/bitsPerByte]...)
	case bits == pcm8Bits:
		return append(data, uint8(math.Round(scaled*pcm8Scale)+pcm8Offset))
	default:
		sample, err := binary.Append(data, binary.LittleEndian, int16(math.Round(scaled*pcm16Scale)))
		if err != nil {
			panic(err)
		}
		return sample
	}
}

func tone(rate, seconds int, amplitude float64) []float32 {
	samples := make([]float32, rate*seconds)
	for index := range samples {
		samples[index] = float32(amplitude * math.Sin(2*math.Pi*toneHz*float64(index)/float64(rate)))
	}

	return samples
}

func rms(samples []int16) float64 {
	var sum float64
	for _, sample := range samples {
		value := float64(sample) / s16Scale
		sum += value * value
	}

	return math.Sqrt(sum / float64(len(samples)))
}

// bestCorrelation is the highest normalised cross-correlation between the
// two signals over lags up to maxLag, which absorbs codec delay: go-mp3 does
// not trim the encoder delay, which puts speech.mp3 70 ms late.
func bestCorrelation(reference, candidate []int16, maxLag int) float64 {
	best := -1.0
	for lag := 0; lag <= maxLag; lag++ {
		var dot, energyRef, energyCand float64
		for index := range min(len(reference), len(candidate)-lag) {
			ref := float64(reference[index])
			cand := float64(candidate[index+lag])
			dot += ref * cand
			energyRef += ref * ref
			energyCand += cand * cand
		}
		best = max(best, dot/math.Sqrt(energyRef*energyCand))
	}

	return best
}

func TestNormaliseUpload_SpeechFixtures_DecodeTo16kMono(t *testing.T) {
	reference := normalise(t, fixture(t, "speech.wav")).samples
	require.Len(t, reference, fixtureFrames)

	tests := []struct {
		name           string
		minFrames      int
		maxFrames      int
		minCorrelation float64
	}{
		{name: "speech.opus", minFrames: fixtureFrames, maxFrames: fixtureFrames, minCorrelation: 0.9},
		{name: "speech-stereo.opus", minFrames: fixtureFrames, maxFrames: fixtureFrames, minCorrelation: 0.9},
		{name: "speech.mp3", minFrames: fixtureFrames, maxFrames: fixtureFrames + mp3PaddingLimit, minCorrelation: 0.95},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := normalise(t, fixture(t, tt.name))

			assert.GreaterOrEqual(t, len(out.samples), tt.minFrames)
			assert.LessOrEqual(t, len(out.samples), tt.maxFrames)
			correlation := bestCorrelation(reference, out.samples, maxLagFrames)
			t.Logf("%s: %d frames, correlation %.4f", tt.name, len(out.samples), correlation)
			assert.GreaterOrEqual(t, correlation, tt.minCorrelation, "decoded audio does not match the source speech")
		})
	}
}

func silence(frames int) []float32 {
	return make([]float32, frames)
}

func TestNormaliseUpload_WAVVariants_ResampleAndDownmix(t *testing.T) {
	const cd = 44100
	mono := toneAmplitude / math.Sqrt2
	tests := []struct {
		name    string
		spec    wavSpec
		wantRMS float64
	}{
		{"s16 mono 16 kHz", wavSpec{tag: formatPCM, rate: targetRate, bits: pcm16Bits, channels: [][]float32{tone(targetRate, toneSeconds, toneAmplitude)}}, mono},
		{"s16 stereo 44.1 kHz", wavSpec{tag: formatPCM, rate: cd, bits: pcm16Bits, channels: [][]float32{tone(cd, toneSeconds, toneAmplitude), silence(cd)}}, mono / 2},
		{"s24 mono 48 kHz", wavSpec{tag: formatPCM, rate: maxSourceRate, bits: pcm24Bits, channels: [][]float32{tone(maxSourceRate, toneSeconds, toneAmplitude)}}, mono},
		{"f32 stereo 8 kHz", wavSpec{tag: wavFormatFloat, rate: narrowbandRate, bits: float32Bits, channels: [][]float32{tone(narrowbandRate, toneSeconds, toneAmplitude), tone(narrowbandRate, toneSeconds, toneAmplitude)}}, mono},
		{"extensible s24 stereo 24 kHz", wavSpec{tag: formatPCM, extensible: true, rate: 24000, bits: pcm24Bits, channels: [][]float32{silence(24000), tone(24000, toneSeconds, toneAmplitude)}}, mono / 2},
		{"extensible f32 mono 32 kHz", wavSpec{tag: wavFormatFloat, extensible: true, rate: 32000, bits: float32Bits, channels: [][]float32{tone(32000, toneSeconds, toneAmplitude)}}, mono},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := normalise(t, tt.spec.bytes())

			assert.Len(t, out.samples, toneSeconds*targetRate)
			assert.InDelta(t, tt.wantRMS, rms(out.samples), tt.wantRMS*rmsTolerance)
		})
	}
}

func TestNormaliseUpload_StreamedWAV_UsesDataToEnd(t *testing.T) {
	wav := wavSpec{tag: formatPCM, rate: targetRate, bits: pcm16Bits, channels: [][]float32{tone(targetRate, toneSeconds, toneAmplitude)}}.bytes()
	binary.LittleEndian.PutUint32(wav[4:], math.MaxUint32)
	binary.LittleEndian.PutUint32(wav[40:], math.MaxUint32)

	out := normalise(t, wav)

	assert.Len(t, out.samples, toneSeconds*targetRate)
}

func TestNormaliseUpload_UnsupportedWAV_Rejected(t *testing.T) {
	one := func(rate int) [][]float32 { return [][]float32{silence(rate)} }
	tests := map[string]wavSpec{
		"8-bit PCM":  {tag: formatPCM, rate: targetRate, bits: pcm8Bits, channels: one(targetRate)},
		"A-law":      {tag: wavFormatALaw, rate: narrowbandRate, bits: pcm8Bits, channels: one(narrowbandRate)},
		"3 channels": {tag: formatPCM, rate: targetRate, bits: pcm16Bits, channels: [][]float32{silence(targetRate), silence(targetRate), silence(targetRate)}},
		"96 kHz":     {tag: formatPCM, rate: highRate, bits: pcm16Bits, channels: one(highRate)},
		"64-bit":     {tag: wavFormatFloat, rate: targetRate, bits: 64, channels: one(targetRate)},
	}
	for name, spec := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NormaliseUpload(t.Context(), bytes.NewReader(spec.bytes()), limits)

			require.ErrorIs(t, err, ErrUnsupported)
			assert.Contains(t, err.Error(), unsupportedText)
		})
	}
}

func oggPageBytes(serial uint32, flags byte, granule uint64, packets ...[]byte) []byte {
	lacing := make([]byte, 0, len(packets))
	body := make([]byte, 0, len(packets))
	for _, packet := range packets {
		lacing = append(lacing, bytes.Repeat([]byte{oggLacingContinue}, len(packet)/oggLacingContinue)...)
		lacing = append(lacing, oneByte(len(packet)%oggLacingContinue))
		body = append(body, packet...)
	}

	return oggRawPage(serial, flags, granule, lacing, body)
}

func oggRawPage(serial uint32, flags byte, granule uint64, lacing, body []byte) []byte {
	page := []byte(oggMagic)
	page = append(page, oggVersion, flags)
	page = binary.LittleEndian.AppendUint64(page, granule)
	page = binary.LittleEndian.AppendUint32(page, serial)
	page = binary.LittleEndian.AppendUint32(page, 0)
	page = binary.LittleEndian.AppendUint32(page, 0)
	page = append(page, oneByte(len(lacing)))
	page = append(page, lacing...)
	page = append(page, body...)
	binary.LittleEndian.PutUint32(page[oggChecksumOffset:], oggChecksum(page))

	return page
}

// editOggPages rewrites each page of an Ogg stream in place and fixes up its
// checksum, the way a hostile muxer would.
func editOggPages(t *testing.T, data []byte, edit func(page []byte)) []byte {
	t.Helper()

	var out []byte
	for len(data) > 0 {
		require.GreaterOrEqual(t, len(data), oggHeaderBytes)
		lacingEnd := oggHeaderBytes + int(data[oggSegmentCountByte])
		size := lacingEnd
		for _, segment := range data[oggHeaderBytes:lacingEnd] {
			size += int(segment)
		}

		page := bytes.Clone(data[:size])
		data = data[size:]
		edit(page)
		binary.LittleEndian.PutUint32(page[oggChecksumOffset:], oggChecksum(page))
		out = append(out, page...)
	}

	return out
}

func pageGranule(page []byte) uint64 {
	return binary.LittleEndian.Uint64(page[oggGranuleOffset:])
}

func setPageGranule(page []byte, granule uint64) {
	binary.LittleEndian.PutUint64(page[oggGranuleOffset:], granule)
}

func opusHeaderPages() []byte {
	head := []byte(opusMagic)
	head = append(head, 1, 1)
	head = le16(head, fixturePreSkip)
	head = le32(head, opusRate)
	head = le16(head, 0)
	head = append(head, opusMappingRTP)

	tags := []byte(opusTagsMagic)
	tags = le32(tags, 0)
	tags = le32(tags, 0)

	pages := oggPageBytes(testSerial, oggFirstPage, 0, head)
	return append(pages, oggPageBytes(testSerial, 0, 0, tags)...)
}

func tinyPackets(count int) [][]byte {
	packets := make([][]byte, count)
	for index := range packets {
		packets[index] = tinyOpusPacket
	}

	return packets
}

func allocatedBy(call func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	call()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

func TestNormaliseUpload_OtherFormats_Unsupported(t *testing.T) {
	tests := map[string][]byte{
		"M4A":         fixture(t, "speech.m4a"),
		"WebM":        []byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01\x42\xf7\x81\x01webm"),
		"AAC ADTS":    []byte("\xff\xf1\x50\x80\x02\x1f\xfc\x21\x00\x49\x90\x02\x19\x00\x23\x80"),
		"Ogg Vorbis":  oggPageBytes(1, 0x02, 0, []byte("\x01vorbis\x00\x00\x00\x00\x01\x80\xbb\x00\x00")),
		"AVI":         []byte("RIFF\x24\x00\x00\x00AVI LIST\x00\x00\x00\x00"),
		"empty":       nil,
		"plain text":  []byte("hello, this is not audio"),
		"MPEG Layer2": []byte("\xff\xfd\x90\x00" + "0000000000000000"),
		"FLAC":        []byte("fLaC\x00\x00\x00\x22junkjunkjunk"),
		"MPEG-2.5":    []byte("\xff\xe3\x38\xc0" + "0000000000000000"),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NormaliseUpload(t.Context(), bytes.NewReader(data), limits)

			require.ErrorIs(t, err, ErrUnsupported)
			assert.Equal(t, "unsupported audio format", err.Error())
		})
	}
}

func TestNormaliseUpload_DurationLimit_CountsDecodedFrames(t *testing.T) {
	limit := Limits{MaxDuration: lowRateSeconds * time.Second}
	atLimit := wavSpec{tag: formatPCM, rate: narrowbandRate, bits: pcm16Bits, channels: [][]float32{silence(narrowbandRate)}}
	overLimit := wavSpec{tag: formatPCM, rate: narrowbandRate, bits: pcm16Bits, channels: [][]float32{silence(narrowbandRate + 1)}}

	_, err := NormaliseUpload(t.Context(), bytes.NewReader(atLimit.bytes()), limit)
	require.NoError(t, err, "exactly the limit is allowed")

	_, err = NormaliseUpload(t.Context(), bytes.NewReader(overLimit.bytes()), limit)
	require.ErrorIs(t, err, ErrTooLong)
}

func TestNormaliseUpload_LongNarrowbandWAV_TooLong(t *testing.T) {
	long := wavSpec{tag: formatPCM, rate: narrowbandRate, bits: pcm16Bits, channels: [][]float32{silence(narrowbandRate * longWAVSeconds)}}

	_, err := NormaliseUpload(t.Context(), bytes.NewReader(long.bytes()), limits)

	require.ErrorIs(t, err, ErrTooLong)
}

func TestNormaliseUpload_OpusOverLimit_TooLong(t *testing.T) {
	_, err := NormaliseUpload(t.Context(), bytes.NewReader(fixture(t, "speech.opus")), Limits{MaxDuration: time.Second})

	require.ErrorIs(t, err, ErrTooLong)
}

func TestNormaliseUpload_CorruptInput_Rejected(t *testing.T) {
	opus := fixture(t, "speech.opus")
	badChecksum := bytes.Clone(opus)
	badChecksum[len(badChecksum)-1] ^= 0xFF

	tests := map[string][]byte{
		"Ogg checksum":       badChecksum,
		"Ogg truncated":      opus[:len(opus)/2],
		"Opus headers only":  opus[:oggHeaderBytes+1+opusHeadBytes],
		"WAV no data":        []byte("RIFF\x04\x00\x00\x00WAVE"),
		"WAV short fmt":      []byte("RIFF\x10\x00\x00\x00WAVEfmt \x04\x00\x00\x00\x01\x00\x01\x00"),
		"MP3 decoder panics": []byte(panickingMP3),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NormaliseUpload(t.Context(), bytes.NewReader(data), limits)

			require.ErrorIs(t, err, ErrCorrupt)
			assert.Contains(t, err.Error(), corruptedText)
		})
	}
}

func TestNormaliseUpload_CancelledContext_ReturnsContextError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NormaliseUpload(ctx, bytes.NewReader(fixture(t, "speech.wav")), limits)

	require.ErrorIs(t, err, context.Canceled)
}

func TestResampler_Push_StreamingMatchesWhole(t *testing.T) {
	input := tone(maxSourceRate, toneSeconds, toneAmplitude)

	whole := newResampler(maxSourceRate, targetRate)
	want := whole.flush(whole.push(nil, input))

	chunked := newResampler(maxSourceRate, targetRate)
	var got []int16
	for start, size := 0, 1; start < len(input); start, size = start+size, size*2%997+1 {
		got = chunked.push(got, input[start:min(start+size, len(input))])
	}
	got = chunked.flush(got)

	assert.Len(t, want, toneSeconds*targetRate)
	assert.Equal(t, want, got)
}

func TestResampler_Push_UpsamplesWithoutLoss(t *testing.T) {
	r := newResampler(narrowbandRate, targetRate)

	samples := r.flush(r.push(nil, tone(narrowbandRate, toneSeconds, toneAmplitude)))

	assert.Len(t, samples, toneSeconds*targetRate)
	assert.InDelta(t, toneAmplitude/math.Sqrt2, rms(samples), toneAmplitude*rmsTolerance)
}

func TestNormaliseUpload_BadID3Tag_CorruptWithoutAllocating(t *testing.T) {
	tests := map[string][]byte{
		"size bytes not syncsafe": []byte("ID3\x04\x00\x00\xff\xff\xff\xff"),
		"size past the upload":    []byte("ID3\x04\x00\x00\x7f\x7f\x7f\x7f"),
		"footer past the upload":  []byte("ID3\x04\x00\x10\x00\x00\x00\x00"),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			var err error
			allocated := allocatedBy(func() {
				_, err = NormaliseUpload(t.Context(), bytes.NewReader(data), limits)
			})

			require.ErrorIs(t, err, ErrCorrupt)
			assert.Contains(t, err.Error(), "ID3")
			assert.Less(t, allocated, uint64(id3AllocBudget), "bytes allocated")
		})
	}
}

func TestNormaliseUpload_ValidID3Tag_Skipped(t *testing.T) {
	plain := fixture(t, "speech.mp3")
	tagged := append([]byte("ID3\x04\x00\x00\x00\x00\x00"), oneByte(id3PaddingBytes))
	tagged = append(tagged, make([]byte, id3PaddingBytes)...)
	tagged = append(tagged, plain...)

	assert.Equal(t, normalise(t, plain).samples, normalise(t, tagged).samples)
}

func TestNormaliseUpload_MPEG25AfterID3Tag_Unsupported(t *testing.T) {
	data := []byte("ID3\x04\x00\x00\x00\x00\x00\x00" + "\xff\xe3\x38\xc0" + "0000000000000000")

	_, err := NormaliseUpload(t.Context(), bytes.NewReader(data), limits)

	require.ErrorIs(t, err, ErrUnsupported)
	assert.Contains(t, err.Error(), unsupportedText)
}

func TestNormaliseUpload_DecoderPanic_CarriesStack(t *testing.T) {
	_, err := NormaliseUpload(t.Context(), bytes.NewReader([]byte(panickingMP3)), limits)

	require.ErrorIs(t, err, ErrCorrupt)
	assert.Contains(t, err.Error(), "index out of range")

	var crash interface{ Stack() []byte }
	require.ErrorAs(t, err, &crash)
	assert.Contains(t, string(crash.Stack()), "go-mp3")
}

func TestNormaliseUpload_OpusGranuleZero_Corrupt(t *testing.T) {
	zeroed := editOggPages(t, fixture(t, "speech.opus"), func(page []byte) {
		setPageGranule(page, 0)
	})

	_, err := NormaliseUpload(t.Context(), bytes.NewReader(zeroed), limits)

	require.ErrorIs(t, err, ErrCorrupt)
}

func TestNormaliseUpload_OpusGranuleZeroFlood_FailsFast(t *testing.T) {
	page := oggPageBytes(testSerial, 0, 0, tinyPackets(packetsPerPage)...)
	flood := opusHeaderPages()
	for len(flood) < floodBytes {
		flood = append(flood, page...)
	}

	ctx, cancel := context.WithTimeout(t.Context(), floodDeadline)
	defer cancel()

	start := time.Now()
	_, err := NormaliseUpload(ctx, bytes.NewReader(flood), Limits{MaxDuration: floodLimit})
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, errors.Is(err, ErrCorrupt) || errors.Is(err, ErrTooLong), "want a typed error, got %v", err)
	assert.Less(t, elapsed, floodBudget)
}

func TestNormaliseUpload_OpusTrimmedFrames_CountAgainstLimit(t *testing.T) {
	keep := uint64(fixturePreSkip + opusRate)
	stream := opusHeaderPages()
	stream = append(stream, oggPageBytes(testSerial, oggLastPage, keep, tinyPackets(packetsPerPage)...)...)

	_, err := NormaliseUpload(t.Context(), bytes.NewReader(stream), Limits{MaxDuration: fixtureSeconds * time.Second})

	require.ErrorIs(t, err, ErrTooLong)
}

func TestNormaliseUpload_OpusGranuleOrigin_TrimsSameAsZeroOrigin(t *testing.T) {
	for _, name := range opusSeedFiles {
		t.Run(name, func(t *testing.T) {
			original := fixture(t, name)
			shifted := editOggPages(t, original, func(page []byte) {
				if granule := pageGranule(page); granule != 0 {
					setPageGranule(page, granule+granuleShift)
				}
			})

			out := normalise(t, shifted)

			assert.Len(t, out.samples, fixtureFrames)
			assert.Equal(t, normalise(t, original).samples, out.samples)
		})
	}
}

func TestNormaliseUpload_OpusAtLimit_Passes(t *testing.T) {
	for _, name := range opusSeedFiles {
		t.Run(name, func(t *testing.T) {
			wav, err := NormaliseUpload(t.Context(), bytes.NewReader(fixture(t, name)), Limits{MaxDuration: fixtureSeconds * time.Second})

			require.NoError(t, err)
			assert.Len(t, parseOutput(t, wav).samples, fixtureFrames)
		})
	}
}

func TestNormaliseUpload_OggAfterEndOfStream_Rejected(t *testing.T) {
	opus := fixture(t, "speech.opus")
	reserialled := editOggPages(t, opus, func(page []byte) {
		page[oggSerialOffset]++
	})

	t.Run("chained", func(t *testing.T) {
		_, err := NormaliseUpload(t.Context(), bytes.NewReader(append(bytes.Clone(opus), reserialled...)), limits)

		require.ErrorIs(t, err, ErrUnsupported)
		assert.Contains(t, err.Error(), chainedText)
	})

	t.Run("same serial", func(t *testing.T) {
		_, err := NormaliseUpload(t.Context(), bytes.NewReader(append(bytes.Clone(opus), opus...)), limits)

		require.ErrorIs(t, err, ErrCorrupt)
	})
}

func TestNormaliseUpload_GiantOggPacket_CorruptWithinMemoryBudget(t *testing.T) {
	segments := bytes.Repeat([]byte{oggLacingContinue}, maxPageSegments)
	body := make([]byte, maxPageSegments*oggLacingContinue)
	giant := opusHeaderPages()
	giant = append(giant, oggRawPage(testSerial, 0, noGranule, segments, body)...)
	page := oggRawPage(testSerial, oggContinued, noGranule, segments, body)
	for len(giant) < giantPacketBytes {
		giant = append(giant, page...)
	}
	control := make([]byte, len(giant))

	var err, controlErr error
	decoding := allocatedBy(func() {
		_, err = NormaliseUpload(t.Context(), bytes.NewReader(giant), limits)
	})
	reading := allocatedBy(func() {
		_, controlErr = NormaliseUpload(t.Context(), bytes.NewReader(control), limits)
	})

	require.ErrorIs(t, controlErr, ErrUnsupported)
	assert.ErrorIs(t, err, ErrCorrupt)
	assert.Less(t, decoding-min(decoding, reading), uint64(giantPacketBudget), "bytes allocated beyond reading the upload")
}

func TestNormaliseUpload_WAVDataSize_MustMatch(t *testing.T) {
	declared := wavSpec{tag: formatPCM, rate: targetRate, bits: pcm16Bits, channels: [][]float32{silence(3 * targetRate)}}.bytes()
	binary.LittleEndian.PutUint32(declared[40:], 10*targetRate*sampleBytes)

	partial := wavSpec{tag: formatPCM, rate: targetRate, bits: pcm16Bits, channels: [][]float32{silence(targetRate)}}.bytes()
	partial = append(partial, 0)
	binary.LittleEndian.PutUint32(partial[40:], targetRate*sampleBytes+1)

	tests := map[string][]byte{
		"10 s declared, 3 s present": declared,
		"partial sample frame":       partial,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NormaliseUpload(t.Context(), bytes.NewReader(data), limits)

			require.ErrorIs(t, err, ErrCorrupt)
		})
	}
}

func TestNormaliseUpload_NoAudio_Corrupt(t *testing.T) {
	tests := map[string][]byte{
		"WAV header only":   wavSpec{tag: formatPCM, rate: targetRate, bits: pcm16Bits, channels: [][]float32{{}}}.bytes(),
		"Opus headers only": opusHeaderPages(),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NormaliseUpload(t.Context(), bytes.NewReader(data), limits)

			require.ErrorIs(t, err, ErrCorrupt)
			assert.Contains(t, err.Error(), noAudioText)
		})
	}
}
