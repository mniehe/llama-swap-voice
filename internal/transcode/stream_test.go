package transcode

import (
	"encoding/binary"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ffmpegEnv        = "LLAMA_SWAP_VOICE_FFMPEG"
	ffmpegName       = "ffmpeg"
	firstByteTimeout = 5 * time.Second

	toneRate       uint32 = 24000
	toneSeconds    uint32 = 2
	toneChannels   uint16 = 1
	toneSampleBits uint16 = 16
	pcmFormatTag   uint16 = 1
	fmtChunkBytes  uint32 = 16
	riffHeadBytes  uint32 = 8
)

func realFFmpeg(t *testing.T) string {
	t.Helper()

	name := os.Getenv(ffmpegEnv)
	if name == "" {
		name = ffmpegName
	}

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no real ffmpeg (%v); set %s or put ffmpeg on PATH", err, ffmpegEnv)
	}

	self, err := os.Executable()
	require.NoError(t, err)
	if path == self {
		t.Skip("ffmpeg resolves to the fake test binary")
	}

	return path
}

func silentWAV() []byte {
	blockAlign := toneChannels * toneSampleBits / 8
	dataBytes := toneRate * toneSeconds * uint32(blockAlign)

	le := binary.LittleEndian
	wav := []byte("RIFF")
	wav = le.AppendUint32(wav, wavHeaderSize-riffHeadBytes+dataBytes)
	wav = append(wav, "WAVEfmt "...)
	wav = le.AppendUint32(wav, fmtChunkBytes)
	wav = le.AppendUint16(wav, pcmFormatTag)
	wav = le.AppendUint16(wav, toneChannels)
	wav = le.AppendUint32(wav, toneRate)
	wav = le.AppendUint32(wav, toneRate*uint32(blockAlign))
	wav = le.AppendUint16(wav, blockAlign)
	wav = le.AppendUint16(wav, toneSampleBits)
	wav = append(wav, "data"...)
	wav = le.AppendUint32(wav, dataBytes)

	return append(wav, make([]byte, dataBytes)...)
}

type firstRead struct {
	n   int
	err error
}

func readFirst(out io.Reader) <-chan firstRead {
	got := make(chan firstRead, 1)
	go func() {
		n, err := out.Read(make([]byte, maxStderr))
		got <- firstRead{n: n, err: err}
	}()

	return got
}

func TestEncoder_Encode_RealFFmpegStreamsBeforeInputEnds(t *testing.T) {
	ffmpeg := realFFmpeg(t)
	audio := silentWAV()

	for _, format := range slices.Sorted(maps.Keys(codecs)) {
		t.Run(string(format), func(t *testing.T) {
			src, sink := io.Pipe()
			out, err := New(ffmpeg).Encode(t.Context(), src, Options{Format: format, Speed: 1})
			require.NoError(t, err)

			written := make(chan error, 1)
			go func() {
				_, writeErr := sink.Write(audio)
				written <- writeErr
			}()

			got := readFirst(out)
			select {
			case first := <-got:
				assert.Positive(t, first.n, "ffmpeg produced no output before its input ended: %v", first.err)
			case <-time.After(firstByteTimeout):
				assert.Fail(t, "ffmpeg produced no output before its input ended")
				require.NoError(t, sink.Close())
				<-got
			}

			require.NoError(t, sink.Close())
			require.NoError(t, <-written)
			_, err = io.Copy(io.Discard, out)
			require.NoError(t, err)
			require.NoError(t, out.Close())
		})
	}
}
