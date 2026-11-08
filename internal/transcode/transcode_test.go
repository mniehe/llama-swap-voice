package transcode

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/fakeffmpeg"
)

func TestMain(m *testing.M) {
	fakeffmpeg.RunIfRequested()
	os.Exit(m.Run())
}

func TestEncodeArgs_Options_StretchesRawPCM(t *testing.T) {
	const pcm = "-f s16le -ar 24000 -ac 1"
	tests := []struct {
		opts Options
		want string
	}{
		{Options{Speed: 1.15}, "-hide_banner -loglevel error " + pcm + " -i pipe:0 -filter:a atempo=1.15 " + pcm + " pipe:1"},
		{Options{Speed: 0.25}, "-hide_banner -loglevel error " + pcm + " -i pipe:0 -filter:a atempo=0.5,atempo=0.5 " + pcm + " pipe:1"},
		{Options{Speed: 2}, "-hide_banner -loglevel error " + pcm + " -i pipe:0 -filter:a atempo=2 " + pcm + " pipe:1"},
		{Options{Speed: 1}, "-hide_banner -loglevel error " + pcm + " -i pipe:0 " + pcm + " pipe:1"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, strings.Join(EncodeArgs(tt.opts), " "), "%+v", tt.opts)
	}
}

func TestEncoder_Encode_StreamsThroughFFmpeg(t *testing.T) {
	fake := fakeffmpeg.Install(t)

	out, err := New(fake.Path).Encode(t.Context(), strings.NewReader("pcm stream"), Options{Speed: 1.5})
	require.NoError(t, err)

	data, err := io.ReadAll(out)
	require.NoError(t, err)
	require.NoError(t, out.Close())

	assert.Equal(t, "pcm stream", string(data))
	assert.Contains(t, fake.Events(), fakeffmpeg.EventEOF, "want stdin EOF")
}

func TestEncoder_Encode_FailureReportsStderrOnClose(t *testing.T) {
	fake := fakeffmpeg.Install(t)
	fake.FailWith(t, "Invalid argument")

	out, err := New(fake.Path).Encode(t.Context(), strings.NewReader("pcm stream"), Options{Speed: 1.5})
	require.NoError(t, err)

	_, err = io.ReadAll(out)
	require.NoError(t, err)

	err = out.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid argument")
}

func TestEncoder_Encode_CapsCapturedStderr(t *testing.T) {
	const noise = "z"
	fake := fakeffmpeg.Install(t)
	fake.FailWith(t, strings.Repeat(noise, 10*maxStderr))

	out, err := New(fake.Path).Encode(t.Context(), strings.NewReader("pcm stream"), Options{Speed: 1.5})
	require.NoError(t, err)

	_, err = io.ReadAll(out)
	require.NoError(t, err)

	err = out.Close()
	require.Error(t, err)
	assert.Equal(t, maxStderr, strings.Count(err.Error(), noise), "stderr bytes kept in the error")
}

func TestEncoder_Encode_MissingBinaryReturnsError(t *testing.T) {
	_, err := New("/nonexistent/ffmpeg").Encode(t.Context(), strings.NewReader(""), Options{Speed: 1.5})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "start ffmpeg")
}

func TestLimitedBuffer_Write_KeepsPrefix(t *testing.T) {
	buf := &limitedBuffer{limit: 4}
	var w io.Writer = buf

	n, err := w.Write([]byte("abcdef"))

	require.NoError(t, err)
	assert.Equal(t, 6, n, "reports the whole write so ffmpeg never blocks on stderr")
	assert.Equal(t, "abcd", buf.String())
}
