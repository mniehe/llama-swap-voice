package openai

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

var errBodyPanic = errors.New("upstream body panicked")

// panickingTransport makes the first reply body panic on its first read;
// later replies pass through untouched.
type panickingTransport struct {
	next  *http.Transport
	calls atomic.Int32
}

func (pt *panickingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := pt.next.RoundTrip(req)
	if err != nil || pt.calls.Add(1) > 1 {
		return resp, err
	}

	resp.Body = panickingBody{ReadCloser: resp.Body}
	return resp, nil
}

type panickingBody struct {
	io.ReadCloser
}

func (panickingBody) Read([]byte) (int, error) {
	panic(errBodyPanic)
}

func TestServer_HandleSpeech_BodyPanicFreesGate(t *testing.T) {
	h := newHarness(t, serveWAV(t), func(cfg *Config) { cfg.RequestTimeout = waitTimeout })

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	var err error
	h.client, err = upstream.New(h.upstream.server.URL, ttsModel, asrModel, &http.Client{Transport: &panickingTransport{next: transport}})
	require.NoError(t, err)
	h.serve(discardLogger())

	first := sendSpeech(t, h, "panics", "wav")
	require.NoError(t, first.err)
	assert.Equal(t, http.StatusInternalServerError, first.status, first.body)

	next := sendSpeech(t, h, "after", "wav")
	require.NoError(t, next.err)
	assert.Equal(t, http.StatusOK, next.status, "a panic kept the TTS slot: %s", next.body)
}
