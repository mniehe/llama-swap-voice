package audio

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// libopusPreSkip is libopus's 6.5 ms lookahead counted at 48 kHz, the
	// pre-skip every Opus stream this package writes must declare, whatever
	// its input rate.
	libopusPreSkip = 312
	// replyBitrate and recordingTargetBitrate are written out rather than
	// taken from the package constants, so a changed constant fails here.
	replyBitrate           = 24000
	recordingTargetBitrate = 32000
	// recordingToneSeconds is long enough for VBR to settle near its target.
	recordingToneSeconds = 10
	// vbrBandDivisor bounds the measured rate to the target plus or minus an
	// eighth: VBR and Ogg framing move it a little, a wrong target moves it
	// more.
	vbrBandDivisor = 8
)

// configuredBitrate returns the target bitrate the libopus encoder behind
// encoder was configured with.
func configuredBitrate(t *testing.T, encoder io.WriteCloser) int {
	t.Helper()

	opus, ok := encoder.(*opusEncoder)
	require.True(t, ok, "encoder is %T", encoder)
	bitrate, err := opus.codec.configuredBitrate()
	require.NoError(t, err)

	return bitrate
}

func opusHeadOf(t *testing.T, stream []byte) (head opusHead, inputRate uint32) {
	t.Helper()

	pages := oggPages(t, stream)
	require.NotEmpty(t, pages)
	require.NotEmpty(t, pages[0].packets)
	packet := pages[0].packets[0]
	head, err := parseOpusHead(packet)
	require.NoError(t, err)

	return head, binary.LittleEndian.Uint32(packet[opusRateOffset:])
}

func TestNewEncoder_Opus_HeadDeclaresReplyRateAndPreSkip(t *testing.T) {
	pcm := s16le(tone(outputRate, toneSeconds, toneAmplitude))

	head, inputRate := opusHeadOf(t, encode(t, Opus, pcm))

	assert.Equal(t, uint32(outputRate), inputRate, "input sample rate")
	assert.Equal(t, libopusPreSkip, head.preSkip, "pre-skip at 48 kHz")
}

func TestNewEncoder_Opus_ConfiguredAtReplyBitrate(t *testing.T) {
	encoder, err := NewEncoder(Opus, io.Discard)
	require.NoError(t, err)
	defer func() { require.NoError(t, encoder.Close()) }()

	assert.Equal(t, replyBitrate, configuredBitrate(t, encoder))
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
	assert.Equal(t, uint64(libopusPreSkip), preSkip, "pre-skip at 48 kHz")
	assert.Equal(t, preSkip+uint64(len(samples))*opusRate/RecordingRate, final, "final granule is pre-skip plus the input length at 48 kHz")
}

func TestNewRecordingEncoder_ConfiguredAtRecordingBitrate(t *testing.T) {
	encoder, err := NewRecordingEncoder(io.Discard)
	require.NoError(t, err)
	defer func() { require.NoError(t, encoder.Close()) }()

	assert.Equal(t, recordingTargetBitrate, configuredBitrate(t, encoder))
}

// A smoke test of the whole stream's size; the configured bitrate above is
// what pins the target.
func TestNewRecordingEncoder_SizeNearTarget(t *testing.T) {
	samples := tone(RecordingRate, recordingToneSeconds, toneAmplitude)

	encoded := encodeRecording(t, s16le(samples))

	bitsPerSecond := float64(len(encoded)*bitsPerByte) / recordingToneSeconds
	t.Logf("measured %.0f bps", bitsPerSecond)
	assert.InDelta(t, recordingTargetBitrate, bitsPerSecond, recordingTargetBitrate/vbrBandDivisor, "encoded at about %d bps", recordingTargetBitrate)
}
