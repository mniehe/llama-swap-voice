package audio

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWAVHeader_MatchesCanonicalPCMHeader(t *testing.T) {
	tests := []struct {
		name     string
		rate     int
		bits     int
		channels int
	}{
		{name: "16 kHz 16-bit mono", rate: 16000, bits: pcm16Bits, channels: 1},
		{name: "48 kHz 16-bit stereo", rate: 48000, bits: pcm16Bits, channels: 2},
		{name: "8 kHz 24-bit mono", rate: 8000, bits: pcm24Bits, channels: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			samples := tone(tc.rate, toneSeconds, toneAmplitude)
			layout := make([][]float32, tc.channels)
			for channel := range layout {
				layout[channel] = samples
			}
			want := wavSpec{tag: formatPCM, rate: tc.rate, bits: tc.bits, channels: layout}.bytes()
			data := want[wavHeaderBytes:]

			header, err := WAVHeader(tc.rate, tc.bits/bitsPerByte, tc.channels, len(data))
			require.NoError(t, err)

			assert.Equal(t, want[:wavHeaderBytes], header)
		})
	}
}

func TestWAVHeader_WrappedPCMDecodes(t *testing.T) {
	pcm := s16le(tone(targetRate, toneSeconds, toneAmplitude))

	header, err := WAVHeader(targetRate, sampleBytes, targetChannels, len(pcm))
	require.NoError(t, err)

	wav, err := NormaliseUpload(t.Context(), bytes.NewReader(append(header, pcm...)), Limits{MaxDuration: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, append(header, pcm...), wav)
}

func TestWAVHeader_RejectsValuesTheHeaderCannotHold(t *testing.T) {
	tests := []struct {
		name                             string
		rate, width, channels, dataBytes int
	}{
		{name: "zero rate", rate: 0, width: 2, channels: 1},
		{name: "negative rate", rate: -1, width: 2, channels: 1},
		{name: "rate over 32 bits", rate: math.MaxUint32 + 1, width: 2, channels: 1},
		{name: "zero width", rate: 16000, width: 0, channels: 1},
		{name: "width over 32 bits per sample", rate: 16000, width: 5, channels: 1},
		{name: "zero channels", rate: 16000, width: 2, channels: 0},
		{name: "channels over 16 bits", rate: 16000, width: 1, channels: math.MaxUint16 + 1},
		{name: "byte rate over 32 bits", rate: math.MaxUint32 / 2, width: 2, channels: 2},
		{name: "negative data", rate: 16000, width: 2, channels: 1, dataBytes: -1},
		{name: "riff size over 32 bits", rate: 16000, width: 2, channels: 1, dataBytes: math.MaxUint32},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header, err := WAVHeader(tc.rate, tc.width, tc.channels, tc.dataBytes)

			require.Error(t, err)
			assert.Nil(t, header)
		})
	}
}

func TestWAVHeader_LargestDataFits(t *testing.T) {
	largest := math.MaxUint32 - (wavHeaderBytes - riffHeaderBytes)

	header, err := WAVHeader(targetRate, sampleBytes, targetChannels, largest)

	require.NoError(t, err)
	assert.Len(t, header, wavHeaderBytes)
}
