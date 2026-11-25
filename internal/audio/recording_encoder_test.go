package audio

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// replyOpusSHA256 is the SHA-256 of the Opus reply for a one-second
	// 24 kHz tone, recorded at fe6c344 before recordings shared the Opus
	// encoder, with each page's random serial and its checksum zeroed. libopus
	// picks SIMD code at run time, so a CPU without AVX2 may encode other
	// bytes; the pinned flake's libopus on an AVX2 host produced this value.
	replyOpusSHA256 = "eccadf3aaa5714ec7448de1ee9bfcb5ec08f62734230efd60cb4d0ae36749cd9"
	// recordingToneSeconds is long enough for VBR to settle near its target.
	recordingToneSeconds = 10
)

// withoutSerials zeroes the serial number and checksum of every Ogg page, the
// only bytes that differ between two encodings of the same input.
func withoutSerials(t *testing.T, stream []byte) []byte {
	t.Helper()

	out := append([]byte(nil), stream...)
	for page := out; len(page) > 0; {
		require.GreaterOrEqual(t, len(page), oggHeaderBytes)
		segments := int(page[oggSegmentCountByte])
		size := oggHeaderBytes + segments
		require.GreaterOrEqual(t, len(page), size)
		for _, lacing := range page[oggHeaderBytes:size] {
			size += int(lacing)
		}
		require.GreaterOrEqual(t, len(page), size)

		clear(page[oggSerialOffset : oggSerialOffset+4])
		clear(page[oggChecksumOffset : oggChecksumOffset+oggChecksumBytes])
		page = page[size:]
	}

	return out
}

func TestNewEncoder_Opus_ReplyBytesUnchanged(t *testing.T) {
	pcm := s16le(tone(outputRate, toneSeconds, toneAmplitude))

	sum := sha256.Sum256(withoutSerials(t, encode(t, Opus, pcm)))

	assert.Equal(t, replyOpusSHA256, hex.EncodeToString(sum[:]))
}

func encodeRecording(t *testing.T, pcm []byte) []byte {
	t.Helper()

	var out bytes.Buffer
	encoder, err := NewRecordingEncoder(&out)
	require.NoError(t, err)

	_, err = io.Copy(encoder, bytes.NewReader(pcm))
	require.NoError(t, err)
	require.NoError(t, encoder.Close())

	return out.Bytes()
}

func TestNewRecordingEncoder_RoundTripsThroughNormaliseUpload(t *testing.T) {
	samples := tone(RecordingRate, toneSeconds, toneAmplitude)
	created, destroyed := opusCreated.Load(), opusDestroyed.Load()

	out := normalise(t, encodeRecording(t, s16le(samples)))

	frame := RecordingRate / opusFramesPerSecond
	assert.InDelta(t, len(samples), len(out.samples), float64(frame), "decoded length within one Opus frame")
	assert.InDelta(t, toneAmplitude/math.Sqrt2, rms(out.samples), 2*rmsTolerance)
	assert.Equal(t, int64(1), opusCreated.Load()-created)
	assert.Equal(t, opusCreated.Load()-created, opusDestroyed.Load()-destroyed, "libopus encoder leaked")
}

func TestNewRecordingEncoder_HeadReportsSixteenKilohertz(t *testing.T) {
	samples := tone(RecordingRate, toneSeconds, toneAmplitude)

	pages := oggPages(t, encodeRecording(t, s16le(samples)))

	require.NotEmpty(t, pages)
	head := pages[0].packets[0]
	assert.Equal(t, uint32(RecordingRate), binary.LittleEndian.Uint32(head[opusRateOffset:]), "input sample rate")
	final := pages[len(pages)-1].granule
	_, err := parseOpusHead(head)
	require.NoError(t, err)
	preSkip := uint64(binary.LittleEndian.Uint16(head[opusPreSkipOffset:]))
	assert.Equal(t, preSkip+uint64(len(samples))*opusRate/RecordingRate, final, "final granule is pre-skip plus the input length at 48 kHz")
}

func TestNewRecordingEncoder_BitrateNearTarget(t *testing.T) {
	samples := tone(RecordingRate, recordingToneSeconds, toneAmplitude)

	encoded := encodeRecording(t, s16le(samples))

	bitsPerSecond := float64(len(encoded)*bitsPerByte) / recordingToneSeconds
	assert.InDelta(t, recordingBitrate, bitsPerSecond, recordingBitrate/2, "encoded at about %d bps", recordingBitrate)
}
