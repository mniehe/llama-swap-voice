package transcode

import (
	"bytes"
	"io"
	"os"
	"slices"
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

func TestEncodeArgs_Options_BuildsCommandLine(t *testing.T) {
	tests := []struct {
		opts Options
		want string
	}{
		{Options{Format: Opus, Speed: 1}, "-hide_banner -loglevel error -probesize 32 -analyzeduration 0 -f wav -i pipe:0 -f ogg -c:a libopus pipe:1"},
		{Options{Format: MP3, Speed: 1.15}, "-hide_banner -loglevel error -probesize 32 -analyzeduration 0 -f wav -i pipe:0 -filter:a atempo=1.15 -f mp3 -c:a libmp3lame pipe:1"},
		{Options{Format: WAV, Speed: 0.25}, "-hide_banner -loglevel error -probesize 32 -analyzeduration 0 -f wav -i pipe:0 -filter:a atempo=0.5,atempo=0.5 -f wav -c:a pcm_s16le pipe:1"},
		{Options{Format: FLAC}, "-hide_banner -loglevel error -probesize 32 -analyzeduration 0 -f wav -i pipe:0 -f flac -c:a flac pipe:1"},
		{Options{Format: AAC, Speed: 2}, "-hide_banner -loglevel error -probesize 32 -analyzeduration 0 -f wav -i pipe:0 -filter:a atempo=2 -f adts -c:a aac pipe:1"},
		{Options{Format: PCM, Speed: 1}, "-hide_banner -loglevel error -probesize 32 -analyzeduration 0 -f wav -i pipe:0 -ar 24000 -ac 1 -f s16le -c:a pcm_s16le pipe:1"},
		{Options{Format: PCM, Speed: 1.5}, "-hide_banner -loglevel error -probesize 32 -analyzeduration 0 -f wav -i pipe:0 -filter:a atempo=1.5 -ar 24000 -ac 1 -f s16le -c:a pcm_s16le pipe:1"},
	}
	for _, tt := range tests {
		args, err := EncodeArgs(tt.opts)

		require.NoError(t, err)
		assert.Equal(t, tt.want, strings.Join(args, " "), "%+v", tt.opts)
	}
}

func TestEncodeArgs_UnknownFormat_ReturnsError(t *testing.T) {
	_, err := EncodeArgs(Options{Format: "ogg"})

	require.Error(t, err)
}

func TestEncoder_Encode_StreamsThroughFFmpeg(t *testing.T) {
	fake := fakeffmpeg.Install(t)

	out, err := New(fake.Path).Encode(t.Context(), strings.NewReader("wav stream"), Options{Format: FLAC, Speed: 1})
	require.NoError(t, err)

	data, err := io.ReadAll(out)
	require.NoError(t, err)
	require.NoError(t, out.Close())

	assert.Equal(t, "wav stream", string(data))
	assert.Contains(t, fake.Events(), fakeffmpeg.EventEOF, "want stdin EOF")
}

func TestEncoder_Encode_FailureReportsStderrOnClose(t *testing.T) {
	fake := fakeffmpeg.Install(t)
	fake.FailWith(t, "Unknown encoder 'libopus'")

	out, err := New(fake.Path).Encode(t.Context(), strings.NewReader("wav stream"), Options{Format: Opus})
	require.NoError(t, err)

	_, err = io.ReadAll(out)
	require.NoError(t, err)

	err = out.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Unknown encoder")
}

func TestEncoder_Encode_CapsCapturedStderr(t *testing.T) {
	const noise = "z"
	fake := fakeffmpeg.Install(t)
	fake.FailWith(t, strings.Repeat(noise, 10*maxStderr))

	out, err := New(fake.Path).Encode(t.Context(), strings.NewReader("wav stream"), Options{Format: Opus})
	require.NoError(t, err)

	_, err = io.ReadAll(out)
	require.NoError(t, err)

	err = out.Close()
	require.Error(t, err)
	assert.Equal(t, maxStderr, strings.Count(err.Error(), noise), "stderr bytes kept in the error")
}

func TestEncoder_Encode_MissingBinaryReturnsError(t *testing.T) {
	_, err := New("/nonexistent/ffmpeg").Encode(t.Context(), strings.NewReader(""), Options{Format: MP3})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "start ffmpeg")
}

func TestEncoder_ToASRInput_ReportsFFmpegStderr(t *testing.T) {
	fake := fakeffmpeg.Install(t)
	fake.FailWith(t, "Invalid data found when processing input")

	_, err := New(fake.Path).ToASRInput(t.Context(), bytes.NewReader([]byte("junk")))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Invalid data found")
}

func TestEncoder_ToASRInput_CapsDecodedSize(t *testing.T) {
	fake := fakeffmpeg.Install(t)
	encoder := New(fake.Path)
	encoder.maxASRBytes = 16

	_, err := encoder.ToASRInput(t.Context(), strings.NewReader(strings.Repeat("x", 64)))

	require.ErrorIs(t, err, ErrTooLong)
}

func TestEncoder_ToASRInput_ReadsSeekableFile(t *testing.T) {
	fake := fakeffmpeg.Install(t)

	out, err := New(fake.Path).ToASRInput(t.Context(), strings.NewReader("m4a bytes"))
	require.NoError(t, err)

	args := fake.Args()
	inputFlag := slices.Index(args, "-i")
	require.GreaterOrEqual(t, inputFlag, 0, "ffmpeg args %v lack -i", args)
	input := args[inputFlag+1]

	assert.NotEqual(t, stdinPipe, input, "want a file input")
	assert.Contains(t, args, "-xerror")
	assert.Equal(t, "m4a bytes", string(out), "want the uploaded bytes")
	assert.NoFileExists(t, input, "temporary input was not removed")
}

func TestLimitedBuffer_Write_KeepsPrefix(t *testing.T) {
	buf := &limitedBuffer{limit: 4}
	var w io.Writer = buf

	n, err := w.Write([]byte("abcdef"))

	require.NoError(t, err)
	assert.Equal(t, 6, n, "reports the whole write so ffmpeg never blocks on stderr")
	assert.Equal(t, "abcd", buf.String())
}
