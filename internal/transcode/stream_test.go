package transcode

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	ffmpegEnv        = "LLAMA_SWAP_VOICE_FFMPEG"
	ffmpegName       = "ffmpeg"
	firstByteTimeout = 5 * time.Second
	stretchedSpeed   = 1.15
	pcmSampleBytes   = 2

	// Half a second of 24 kHz mono s16le, less than ffmpeg's 32 KiB output
	// buffer, so output before the input ends proves ffmpeg flushes.
	pausedPCMBytes = pcmSampleRate / 2 * pcmSampleBytes
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

func TestEncoder_Encode_RealFFmpegStretchesBeforeInputEnds(t *testing.T) {
	ffmpeg := realFFmpeg(t)
	src, sink := io.Pipe()
	out, err := New(ffmpeg).Encode(t.Context(), src, Options{Speed: stretchedSpeed})
	require.NoError(t, err)

	written := make(chan error, 1)
	go func() {
		_, writeErr := sink.Write(make([]byte, pausedPCMBytes))
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

	require.NoError(t, <-written)
	require.NoError(t, sink.Close())
	_, err = io.Copy(io.Discard, out)
	require.NoError(t, err)
	require.NoError(t, out.Close())
}
