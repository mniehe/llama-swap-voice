package audio

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	streamWaitTimeout = 2 * time.Second
	listChunkID       = "LIST"
)

func speechWAV(samples []float32) []byte {
	return wavSpec{tag: formatPCM, rate: outputRate, bits: pcm16Bits, channels: [][]float32{samples}}.bytes()
}

// streamedWAV rewrites the RIFF and data sizes to the 0xFFFFFFFF sentinel
// llama-server sends when it does not know the length up front.
func streamedWAV(wav []byte) []byte {
	streamed := bytes.Clone(wav)
	binary.LittleEndian.PutUint32(streamed[riffHeaderBytes-chunkIDBytes:], wavUnknownSize)
	binary.LittleEndian.PutUint32(streamed[wavHeaderBytes-chunkIDBytes:], wavUnknownSize)

	return streamed
}

func readPCM(t *testing.T, r io.Reader) []byte {
	t.Helper()

	wav, err := NewWAVReader(r)
	require.NoError(t, err)

	pcm, err := io.ReadAll(wav)
	require.NoError(t, err)

	return pcm
}

func TestNewWAVReader_StreamedSizes_YieldsEverySampleAfterHeader(t *testing.T) {
	wav := streamedWAV(speechWAV(tone(outputRate, toneSeconds, toneAmplitude)))

	pcm := readPCM(t, iotest.OneByteReader(bytes.NewReader(wav)))

	assert.Equal(t, wav[wavHeaderBytes:], pcm)
}

func TestNewWAVReader_DeclaredSizes_StopsAtDataEnd(t *testing.T) {
	wav := speechWAV(tone(outputRate, toneSeconds, toneAmplitude))
	trailing := append(bytes.Clone(wav), listChunkID+"\x04\x00\x00\x00junk"...)

	pcm := readPCM(t, bytes.NewReader(trailing))

	assert.Equal(t, wav[wavHeaderBytes:], pcm)
}

func TestNewWAVReader_SkipsChunksBeforeData(t *testing.T) {
	wav := speechWAV(tone(outputRate, toneSeconds, toneAmplitude))
	list := []byte(listChunkID + "\x05\x00\x00\x00odd!!\x00")
	dataStart := wavHeaderBytes - chunkHeaderBytes
	withList := append(append(bytes.Clone(wav[:dataStart]), list...), wav[dataStart:]...)

	pcm := readPCM(t, bytes.NewReader(withList))

	assert.Equal(t, wav[wavHeaderBytes:], pcm)
}

func TestNewWAVReader_ReturnsBeforeBodyEnds(t *testing.T) {
	wav := streamedWAV(speechWAV(tone(outputRate, toneSeconds, toneAmplitude)))
	src, sink := io.Pipe()
	defer src.Close()

	go func() {
		_, err := sink.Write(wav[:wavHeaderBytes+outputRate])
		assert.NoError(t, err)
	}()

	got := make(chan []byte, 1)
	go func() {
		wavReader, err := NewWAVReader(src)
		if !assert.NoError(t, err) {
			got <- nil
			return
		}
		first := make([]byte, outputRate)
		n, err := io.ReadFull(wavReader, first)
		assert.NoError(t, err)
		got <- first[:n]
	}()

	select {
	case first := <-got:
		assert.Equal(t, wav[wavHeaderBytes:wavHeaderBytes+outputRate], first)
	case <-time.After(streamWaitTimeout):
		require.Fail(t, "WAV reader waited for the end of the body")
	}
}

func TestNewWAVReader_RejectsOtherLayouts(t *testing.T) {
	samples := tone(outputRate, toneSeconds, toneAmplitude)
	tests := map[string]wavSpec{
		"16 kHz":  {tag: formatPCM, rate: targetRate, bits: pcm16Bits, channels: [][]float32{samples}},
		"stereo":  {tag: formatPCM, rate: outputRate, bits: pcm16Bits, channels: [][]float32{samples, samples}},
		"float32": {tag: wavFormatFloat, rate: outputRate, bits: float32Bits, channels: [][]float32{samples}},
	}
	for name, spec := range tests {
		_, err := NewWAVReader(bytes.NewReader(spec.bytes()))

		require.ErrorIs(t, err, ErrUnsupported, name)
	}
}

func TestNewWAVReader_RejectsMalformedHeaders(t *testing.T) {
	wav := speechWAV(tone(outputRate, toneSeconds, toneAmplitude))
	hugeFormat := bytes.Clone(wav)
	binary.LittleEndian.PutUint32(hugeFormat[riffPreambleBytes+chunkIDBytes:], wavUnknownSize)
	dataFirst := append(bytes.Clone(wav[:riffPreambleBytes]), dataChunkID+"\x00\x00\x00\x00"...)

	tests := map[string][]byte{
		"empty":             nil,
		"not RIFF":          []byte("OggS" + string(wav[4:])),
		"cut in header":     wav[:wavHeaderBytes-2],
		"no data chunk":     wav[:wavHeaderBytes-chunkHeaderBytes],
		"huge fmt chunk":    hugeFormat,
		"data before fmt":   dataFirst,
		"cut in fmt chunk":  wav[:riffPreambleBytes+chunkHeaderBytes+4],
		"unknown sentinels": []byte(listChunkID + string(wav[4:])),
	}
	for name, data := range tests {
		_, err := NewWAVReader(bytes.NewReader(data))

		require.ErrorIs(t, err, ErrCorrupt, name)
	}
}

func TestWAVReader_Read_TrailingHalfSampleIsCorrupt(t *testing.T) {
	wav := speechWAV(tone(outputRate, toneSeconds, toneAmplitude))
	declared := append(bytes.Clone(wav), 0)
	dataSize := declared[wavHeaderBytes-chunkIDBytes:]
	binary.LittleEndian.PutUint32(dataSize, binary.LittleEndian.Uint32(dataSize)+1)

	tests := map[string][]byte{
		"streamed sizes": append(streamedWAV(wav), 0),
		"declared sizes": declared,
		"only half":      append(streamedWAV(wav)[:wavHeaderBytes], 0),
	}
	for name, data := range tests {
		wavReader, err := NewWAVReader(bytes.NewReader(data))
		require.NoError(t, err, name)

		_, err = io.ReadAll(wavReader)

		require.ErrorIs(t, err, ErrCorrupt, name)
	}
}

func TestWAVReader_Read_TruncatedDeclaredDataIsError(t *testing.T) {
	wav := speechWAV(tone(outputRate, toneSeconds, toneAmplitude))
	wavReader, err := NewWAVReader(bytes.NewReader(wav[:len(wav)-10]))
	require.NoError(t, err)

	_, err = io.ReadAll(wavReader)

	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
