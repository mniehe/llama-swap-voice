package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		_, synthErr := client.Synthesize(ctx, &Speech{Input: "hi", SpeakerRef: ref, Format: WAV})
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

const (
	clipBytes   = "clip bytes"
	upstreamWAV = "RIFF audio"
	handlerLags = 50 * time.Millisecond
)

type recordedCall struct {
	path        string
	contentType string
	body        string
}

type recordingUpstream struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []recordedCall
	respond []http.HandlerFunc
}

// newRecordingUpstream answers the nth request with the nth handler and every
// later one with the last.
func newRecordingUpstream(t *testing.T, respond ...http.HandlerFunc) (*recordingUpstream, *httptest.Server) {
	t.Helper()

	rec := &recordingUpstream{t: t, respond: respond}
	server := httptest.NewServer(http.HandlerFunc(rec.serve))
	t.Cleanup(server.Close)

	return rec, server
}

func (r *recordingUpstream) serve(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(req.Body)
	assert.NoError(r.t, err)

	r.mu.Lock()
	r.calls = append(r.calls, recordedCall{path: req.URL.Path, contentType: req.Header.Get("Content-Type"), body: string(body)})
	respond := r.respond[min(len(r.calls), len(r.respond))-1]
	r.mu.Unlock()

	respond(w, req)
}

func (r *recordingUpstream) recorded() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.calls)
}

func write(t *testing.T, w io.Writer, text string) {
	_, err := io.WriteString(w, text)
	assert.NoError(t, err)
}

func serveAudio(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		write(t, w, upstreamWAV)
	}
}

func answer(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(code), code)
	}
}

func dropConnection(w http.ResponseWriter, _ *http.Request) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

func writeClip(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "alice.wav")
	require.NoError(t, os.WriteFile(path, []byte(clipBytes), 0o600))

	return path
}

func newTestClient(t *testing.T, base string, opts ...Option) *Client {
	t.Helper()

	client, err := New(base, "tts", "asr", &http.Client{}, opts...)
	require.NoError(t, err)

	return client
}

func synthesize(t *testing.T, client *Client, ctx context.Context, speech *Speech) (*Audio, error) {
	t.Helper()

	audio, err := client.Synthesize(ctx, speech)
	if err == nil {
		t.Cleanup(func() { assert.NoError(t, audio.Body.Close()) })
	}

	return audio, err
}

func TestParseBackend_KnownNames_ReturnBackend(t *testing.T) {
	for _, want := range []Backend{LlamaCPP, QwenTTS} {
		got, err := ParseBackend(string(want))

		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

func TestParseBackend_UnknownName_NamesBothBackends(t *testing.T) {
	_, err := ParseBackend("other")

	require.Error(t, err)
	assert.Equal(t, `upstream.ttsbackend must be llamacpp or qwentts, got "other"`, err.Error())
}

func TestNew_UnknownBackend_ReturnsError(t *testing.T) {
	_, err := New("http://127.0.0.1:8080", "tts", "asr", nil, WithBackend("other"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream.ttsbackend")
}

func TestClient_Synthesize_LlamaCPPPostsMultipartWithClip(t *testing.T) {
	rec, fake := newRecordingUpstream(t, serveAudio(t))
	client := newTestClient(t, fake.URL)

	audio, err := synthesize(t, client, t.Context(), &Speech{Voice: "alice", Input: "hello", Lang: "en", SpeakerRef: writeClip(t), Format: WAV})

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, audio.Status)
	calls := rec.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, "/upstream/tts/tts", calls[0].path)
	assert.True(t, strings.HasPrefix(calls[0].contentType, "multipart/form-data; boundary="), calls[0].contentType)
	assert.Contains(t, calls[0].body, clipBytes)
	assert.NotContains(t, calls[0].body, `name="voice"`)
}

func TestClient_Synthesize_QwenTTSPostsJSONWithoutOpeningClip(t *testing.T) {
	rec, fake := newRecordingUpstream(t, serveAudio(t))
	client := newTestClient(t, fake.URL, WithBackend(QwenTTS))
	missing := filepath.Join(t.TempDir(), "never-created.wav")

	audio, err := synthesize(t, client, t.Context(), &Speech{Voice: "robbie", Input: "hello", Lang: "en", SpeakerRef: missing, Format: WAV})

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, audio.Status)
	body, err := io.ReadAll(audio.Body)
	require.NoError(t, err)
	assert.Equal(t, upstreamWAV, string(body))
	calls := rec.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, "/upstream/tts/v1/tts", calls[0].path)
	assert.Equal(t, "application/json", calls[0].contentType)
	assert.Equal(t, `{"voice":"robbie","input":"hello","response_format":"wav"}`, calls[0].body)
}

func TestStatusError_DecodesErrorEnvelope(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"envelope":       {`{"error":{"message":"queue full"}}`, "queue full"},
		"plain text":     {" model crashed \n", "model crashed"},
		"other json":     {`{"error":"not an object"}`, `{"error":"not an object"}`},
		"empty message":  {`{"error":{"message":""}}`, `{"error":{"message":""}}`},
		"blank message":  {`{"error":{"message":"  \n"}}`, `{"error":{"message":"  \n"}}`},
		"padded message": {`{"error":{"message":" queue full "}}`, "queue full"},
		"empty body":     {"", http.StatusText(http.StatusServiceUnavailable)},
		"truncated json": {`{"error":{"message":"queue`, `{"error":{"message":"queue`},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tt.body))}

			var statusErr *StatusError
			require.ErrorAs(t, statusError(resp), &statusErr)
			assert.Equal(t, tt.want, statusErr.Message)
			assert.Equal(t, http.StatusServiceUnavailable, statusErr.Code)
		})
	}
}

func TestClient_Synthesize_ErrorCarriesRetryAfter(t *testing.T) {
	const seconds = "5"
	_, fake := newRecordingUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(retryAfterHeader, seconds)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		write(t, w, `{"error":{"message":"queue full"}}`)
	})
	client := newTestClient(t, fake.URL, WithBackend(QwenTTS))

	_, err := synthesize(t, client, t.Context(), &Speech{Voice: "robbie", Input: "hello", Format: WAV})

	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, StatusError{Code: http.StatusServiceUnavailable, Message: "queue full", RetryAfter: seconds}, *statusErr)
}

func TestClient_Synthesize_RetriesOnceBeforeAnyResponse(t *testing.T) {
	tests := map[string]struct {
		respond   []http.HandlerFunc
		wantCalls int
		wantCode  int
		retried   bool
	}{
		"dropped connection then audio": {[]http.HandlerFunc{dropConnection, serveAudio(t)}, 2, 0, true},
		"502 then audio":                {[]http.HandlerFunc{answer(http.StatusBadGateway), serveAudio(t)}, 2, 0, true},
		"502 twice":                     {[]http.HandlerFunc{answer(http.StatusBadGateway)}, 2, http.StatusBadGateway, false},
		"503 is not retried":            {[]http.HandlerFunc{answer(http.StatusServiceUnavailable), serveAudio(t)}, 1, http.StatusServiceUnavailable, false},
		"500 is not retried":            {[]http.HandlerFunc{answer(http.StatusInternalServerError), serveAudio(t)}, 1, http.StatusInternalServerError, false},
		"audio is not retried":          {[]http.HandlerFunc{serveAudio(t)}, 1, 0, false},
	}

	for name, tt := range tests {
		for _, backend := range []Backend{LlamaCPP, QwenTTS} {
			t.Run(name+"/"+string(backend), func(t *testing.T) {
				rec, fake := newRecordingUpstream(t, tt.respond...)
				client := newTestClient(t, fake.URL, WithBackend(backend))

				audio, err := synthesize(t, client, t.Context(), &Speech{Voice: "alice", Input: "hello", SpeakerRef: writeClip(t), Format: WAV})

				calls := rec.recorded()
				require.Len(t, calls, tt.wantCalls)
				for _, call := range calls[1:] {
					assert.Equal(t, calls[0], call, "the retry sent a different request")
				}

				if tt.wantCode != 0 {
					var statusErr *StatusError
					require.ErrorAs(t, err, &statusErr)
					assert.Equal(t, tt.wantCode, statusErr.Code)
					return
				}

				require.NoError(t, err)
				assert.Equal(t, tt.retried, audio.Retried)
			})
		}
	}
}

func TestClient_Synthesize_CancelledFirstAttemptIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	rec, fake := newRecordingUpstream(t, func(http.ResponseWriter, *http.Request) {
		cancel()
		time.Sleep(handlerLags)
	})
	client := newTestClient(t, fake.URL, WithBackend(QwenTTS))

	_, err := synthesize(t, client, ctx, &Speech{Voice: "alice", Input: "hello", Format: WAV})

	require.ErrorIs(t, err, context.Canceled)
	assert.Len(t, rec.recorded(), 1)
}

func TestClient_Transcribe_IsNotRetried(t *testing.T) {
	rec, fake := newRecordingUpstream(t, answer(http.StatusBadGateway), serveAudio(t))
	client := newTestClient(t, fake.URL)

	_, err := client.Transcribe(t.Context(), Recording{Name: "clip.wav", Data: strings.NewReader(clipBytes)})

	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadGateway, statusErr.Code)
	assert.Len(t, rec.recorded(), 1)
}

func TestClient_Synthesize_FailedRetrySaysSo(t *testing.T) {
	_, fake := newRecordingUpstream(t, answer(http.StatusBadGateway))
	client := newTestClient(t, fake.URL)

	_, err := synthesize(t, client, t.Context(), &Speech{Input: "hello", SpeakerRef: writeClip(t), Format: WAV})

	var statusErr *StatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadGateway, statusErr.Code)
	assert.Contains(t, err.Error(), "retried once")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestClient_Synthesize_CancelArrivingWith502IsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	rec, fake := newRecordingUpstream(t, answer(http.StatusBadGateway))
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)

	var attempts atomic.Int32
	cancelling := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempts.Add(1)
		resp, err := transport.RoundTrip(req)
		cancel()
		return resp, err
	})
	client, err := New(fake.URL, "tts", "asr", &http.Client{Transport: cancelling}, WithBackend(QwenTTS))
	require.NoError(t, err)

	_, err = synthesize(t, client, ctx, &Speech{Voice: "alice", Input: "hello", Format: WAV})

	require.Error(t, err)
	assert.Equal(t, int32(1), attempts.Load(), "a second attempt started after cancellation")
	assert.Len(t, rec.recorded(), 1)
}

func redirectToSelf(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
}

func TestClient_RedirectIsNotFollowed(t *testing.T) {
	tests := map[string]func(*testing.T, *Client) error{
		"speech": func(t *testing.T, client *Client) error {
			_, err := synthesize(t, client, t.Context(), &Speech{Input: "hello", SpeakerRef: writeClip(t), Format: WAV})
			return err
		},
		"transcription": func(t *testing.T, client *Client) error {
			_, err := client.Transcribe(t.Context(), Recording{Name: "clip.wav", Data: strings.NewReader(clipBytes)})
			return err
		},
	}

	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			rec, fake := newRecordingUpstream(t, redirectToSelf)
			client := newTestClient(t, fake.URL)

			err := call(t, client)

			var statusErr *StatusError
			require.ErrorAs(t, err, &statusErr)
			assert.Equal(t, http.StatusTemporaryRedirect, statusErr.Code)
			assert.Len(t, rec.recorded(), 1)
		})
	}
}
