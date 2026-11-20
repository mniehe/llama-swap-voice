package pipeline

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errBodyPanic = errors.New("upstream body panicked")

// panickingTransport makes the first reply body panic once it has delivered
// safeBytes; later replies pass through untouched.
type panickingTransport struct {
	next      *http.Transport
	safeBytes int
	calls     atomic.Int32
}

func (pt *panickingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := pt.next.RoundTrip(req)
	if err != nil || pt.calls.Add(1) > 1 {
		return resp, err
	}

	resp.Body = &panickingBody{ReadCloser: resp.Body, left: pt.safeBytes}
	return resp, nil
}

type panickingBody struct {
	io.ReadCloser
	left int
}

func (b *panickingBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		panic(errBodyPanic)
	}

	n, err := b.ReadCloser.Read(p[:min(len(p), b.left)])
	b.left -= n
	return n, err
}

func newPanickingPipeline(t *testing.T, safeBytes int) *Pipeline {
	t.Helper()

	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	httpClient := &http.Client{Transport: &panickingTransport{next: transport, safeBytes: safeBytes}}
	p, _ := newClientPipeline(t, discardLogger(), httpClient, serveWAV(t))

	return p
}

// speakOrPanic returns what Speak panicked with, or what it returned when it
// did not panic.
func speakOrPanic(ctx context.Context, p *Pipeline, job SpeechJob) (outcome any) {
	defer func() {
		if panicked := recover(); panicked != nil {
			outcome = panicked
		}
	}()

	speech, err := p.Speak(ctx, job, normalSpeed)
	if err != nil {
		return err
	}

	return speech.PCM.Close()
}

func TestPipeline_Speak_PanicFreesGate(t *testing.T) {
	tests := map[string]int{
		"wav header":   0,
		"first sample": len(upstreamHeader),
	}

	for name, safeBytes := range tests {
		t.Run(name, func(t *testing.T) {
			p := newPanickingPipeline(t, safeBytes)
			job := prepare(t, p, "", "hello")

			require.Equal(t, errBodyPanic, speakOrPanic(t.Context(), p, job))
			assert.Equal(t, 1, p.ttsGate.available(), "a panic in Speak kept the TTS slot")

			ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
			defer cancel()

			speech, err := p.Speak(ctx, job, normalSpeed)
			require.NoError(t, err, "the speech request after the panic")
			assert.Equal(t, upstreamFrames, readSpeech(t, speech))
		})
	}
}
