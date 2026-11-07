package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/pion/opus"
	"github.com/stretchr/testify/require"
)

const (
	fuzzDuration      = 10 * time.Second
	fuzzExecTimeout   = 5 * time.Second
	packetLengthBytes = 2
	seedPacketCount   = 12
)

var (
	fuzzLimits     = Limits{MaxDuration: fuzzDuration}
	opusRates      = []int{8000, 12000, 16000, 24000, opusRate}
	opusChannels   = []int{1, maxChannels}
	fuzzSeedFiles  = []string{"speech.wav", "speech.opus", "speech-stereo.opus", "speech.mp3", "speech.m4a"}
	opusSeedFiles  = []string{"speech.opus", "speech-stereo.opus"}
	typedAudioErrs = []error{ErrUnsupported, ErrCorrupt, ErrTooLong}
)

func FuzzNormaliseUpload(f *testing.F) {
	for _, name := range fuzzSeedFiles {
		f.Add(fixture(f, name))
	}
	f.Add([]byte(panickingMP3))

	f.Fuzz(func(t *testing.T, data []byte) {
		wav, err := normaliseWithin(t, data)
		if err != nil {
			requireTypedError(t, err)
			return
		}

		out := parseOutput(t, wav)
		require.Equal(t, uint32(targetRate), out.rate)
		require.Equal(t, uint16(targetChannels), out.channels)
		require.LessOrEqual(t, len(out.samples), int(fuzzDuration/time.Second)*targetRate+kernelTaps)
	})
}

type normalised struct {
	wav []byte
	err error
}

// The fuzzer has no per-input timeout, so a decoder that never returns would
// otherwise stall every worker without saving the input that caused it.
func normaliseWithin(t *testing.T, data []byte) ([]byte, error) {
	done := make(chan normalised, 1)
	go func() {
		wav, err := NormaliseUpload(t.Context(), bytes.NewReader(data), fuzzLimits)
		done <- normalised{wav: wav, err: err}
	}()

	select {
	case result := <-done:
		return result.wav, result.err
	case <-time.After(fuzzExecTimeout):
		t.Fatalf("NormaliseUpload did not return within %s", fuzzExecTimeout)
		return nil, nil
	}
}

func requireTypedError(t *testing.T, err error) {
	t.Helper()

	for _, typed := range typedAudioErrs {
		if errors.Is(err, typed) {
			return
		}
	}

	t.Fatalf("NormaliseUpload returned an untyped error: %v", err)
}

// FuzzOpusPacket feeds pion/opus raw packets at every output layout, the
// coverage its own Ogg-wrapped fuzzer leaves out. The input is a run of
// packets, each prefixed with its length, so state carries between them.
func FuzzOpusPacket(f *testing.F) {
	for _, name := range opusSeedFiles {
		packets := fixturePackets(f, name)
		f.Add(joinPackets(packets))
		f.Add(packets[0])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		packets := splitPackets(data)
		for _, rate := range opusRates {
			for _, channels := range opusChannels {
				decodePackets(t, rate, channels, packets)
			}
		}
	})
}

func decodePackets(t *testing.T, rate, channels int, packets [][]byte) {
	decoder, err := opus.NewDecoderWithOutput(rate, channels)
	require.NoError(t, err)

	out := make([]float32, opusMaxPacketFrames*channels)
	for _, packet := range packets {
		frames, err := decoder.DecodeToFloat32(packet, out)
		if err != nil {
			continue
		}
		require.LessOrEqual(t, frames*channels, len(out))
	}
}

func fixturePackets(t testing.TB, name string) [][]byte {
	t.Helper()

	reader := newOggReader(fixture(t, name))
	var packets [][]byte
	for len(packets) < seedPacketCount+2 {
		page, err := reader.nextPage()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		packets = append(packets, page.packets...)
	}

	return packets[2:]
}

func joinPackets(packets [][]byte) []byte {
	var data []byte
	for _, packet := range packets {
		data = le16(data, len(packet))
		data = append(data, packet...)
	}

	return data
}

func splitPackets(data []byte) [][]byte {
	var packets [][]byte
	for len(data) >= packetLengthBytes {
		size := int(binary.LittleEndian.Uint16(data))
		data = data[packetLengthBytes:]
		size = min(size, len(data))
		packets = append(packets, data[:size])
		data = data[size:]
	}

	return packets
}
