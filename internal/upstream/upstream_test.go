package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_URLWithoutHost_ReturnsError(t *testing.T) {
	_, err := New("localhost:8080", "tts", "asr", nil)

	require.Error(t, err)
}

func TestNew_URLWithHost_Succeeds(t *testing.T) {
	client, err := New("http://127.0.0.1:8080", "tts", "asr", nil)

	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:8080/upstream/tts/tts", client.base.JoinPath(routePrefix, "tts", ttsPath).String())
}

func TestDecodeTranscript_TextField_ReturnsText(t *testing.T) {
	text, err := decodeTranscript(strings.NewReader(`{"type":"transcript.text.done","text":"hello"}`))

	require.NoError(t, err)
	assert.Equal(t, "hello", text)
}

func TestDecodeTranscript_MissingText_ReturnsError(t *testing.T) {
	_, err := decodeTranscript(strings.NewReader(`{"type":"error"}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"error"`)
}

func TestDecodeTranscript_NotJSON_ReturnsError(t *testing.T) {
	_, err := decodeTranscript(strings.NewReader(`upstream exploded`))

	require.Error(t, err)
}

func transcriptOfSize(size int) string {
	const envelope = `{"text":""}`
	return `{"text":"` + strings.Repeat("a", size-len(envelope)) + `"}`
}

func TestDecodeTranscript_AtSizeLimit_ReturnsText(t *testing.T) {
	text, err := decodeTranscript(strings.NewReader(transcriptOfSize(maxTranscriptBytes)))

	require.NoError(t, err)
	assert.NotEmpty(t, text)
}

func TestDecodeTranscript_OverSizeLimit_ReturnsError(t *testing.T) {
	_, err := decodeTranscript(strings.NewReader(transcriptOfSize(maxTranscriptBytes + 1)))

	require.ErrorIs(t, err, errTranscriptTooLarge)
}

func TestStatusError_Error_IncludesCodeAndMessage(t *testing.T) {
	err := &StatusError{Code: 503, Message: "loading model"}

	assert.Equal(t, "upstream returned 503: loading model", err.Error())
}

func TestClient_Synthesize_CancelStopsReadingSpeakerRef(t *testing.T) {
	const (
		copyStarts  = 50 * time.Millisecond
		waitTimeout = time.Second
	)

	var calls atomic.Int32
	fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(fake.Close)

	client, err := New(fake.URL, "tts", "asr", &http.Client{})
	require.NoError(t, err)

	ref := filepath.Join(t.TempDir(), "endless.wav")
	require.NoError(t, syscall.Mkfifo(ref, 0o600))
	feed, err := os.OpenFile(ref, os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, feed.Close()) })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, synthErr := client.Synthesize(ctx, Speech{Input: "hi", SpeakerRef: ref, Format: WAV})
		done <- synthErr
	}()

	_, err = feed.WriteString("RIFF")
	require.NoError(t, err)
	time.Sleep(copyStarts)
	cancel()
	_, err = feed.WriteString("more")
	require.NoError(t, err)

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(waitTimeout):
		require.Fail(t, "speaker reference still being read after cancel")
	}
	assert.Zero(t, calls.Load(), "upstream was called")
}
