package pipeline

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	ttsModel        = "qwen3-tts"
	asrModel        = "qwen3-asr"
	ttsRoute        = "/upstream/" + ttsModel + "/tts"
	asrRoute        = "/upstream/" + asrModel + "/v1/audio/transcriptions"
	upstreamHeader  = "RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xff"
	upstreamFrames  = "audio frames"
	upstreamAudio   = upstreamHeader + upstreamFrames
	streamedWAV     = "RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xff\x01\x02\x03\x04"
	contentTypeWAV  = "audio/wav"
	contentTypeJSON = "application/json"
	fakeFormMemory  = 1 << 20
	harnessTimeout  = 10 * time.Second
	harnessMaxChars = 100
	waitTimeout     = time.Second
	stuckTimeout    = 3 * time.Second
	shortDeadline   = 300 * time.Millisecond
	deadlineSlack   = 700 * time.Millisecond
	transcriptText  = "hello"
)

type upstreamCall struct {
	path   string
	fields map[string]string
	files  map[string][]byte
}

type fakeUpstream struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	calls   []upstreamCall
	respond http.HandlerFunc
}

func newFakeUpstream(t *testing.T, respond http.HandlerFunc) *fakeUpstream {
	f := &fakeUpstream{t: t, respond: respond}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	call := upstreamCall{path: r.URL.Path, fields: map[string]string{}, files: map[string][]byte{}}
	if err := r.ParseMultipartForm(fakeFormMemory); err != nil {
		assert.NoError(f.t, err, "upstream got an invalid request")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for key, values := range r.MultipartForm.Value {
		call.fields[key] = values[0]
	}
	for key, headers := range r.MultipartForm.File {
		call.files[key] = readPart(f.t, headers[0])
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	f.respond(w, r)
}

func readPart(t *testing.T, header *multipart.FileHeader) []byte {
	file, err := header.Open()
	if !assert.NoError(t, err) {
		return nil
	}
	defer func() { assert.NoError(t, file.Close()) }()

	data, err := io.ReadAll(file)
	assert.NoError(t, err)

	return data
}

func (f *fakeUpstream) callsTo(path string) []upstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	var matched []upstreamCall
	for _, call := range f.calls {
		if call.path == path {
			matched = append(matched, call)
		}
	}

	return matched
}

func (f *fakeUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

func newPipeline(t *testing.T, respond http.HandlerFunc, tweak ...func(*Config)) (*Pipeline, *fakeUpstream) {
	t.Helper()

	return newLoggedPipeline(t, discardLogger(), respond, tweak...)
}

func newLoggedPipeline(t *testing.T, log *slog.Logger, respond http.HandlerFunc, tweak ...func(*Config)) (*Pipeline, *fakeUpstream) {
	t.Helper()

	return newClientPipeline(t, log, &http.Client{}, respond, tweak...)
}

func newClientPipeline(t *testing.T, log *slog.Logger, httpClient *http.Client, respond http.HandlerFunc, tweak ...func(*Config)) (*Pipeline, *fakeUpstream) {
	t.Helper()

	dir := t.TempDir()
	fake := newFakeUpstream(t, respond)
	cfg := Config{
		TTSModel:         ttsModel,
		ASRModel:         asrModel,
		Voices:           map[string]string{"alice": writeFile(t, dir, "alice.mp3"), "narrator": writeFile(t, dir, "narrator.wav")},
		DefaultVoice:     "alice",
		Lang:             "en",
		RequestTimeout:   harnessTimeout,
		MaxInputChars:    harnessMaxChars,
		MaxConcurrent:    4,
		MaxConcurrentTTS: 8,
		TTSConcurrency:   1,
	}
	for _, apply := range tweak {
		apply(&cfg)
	}

	client, err := upstream.New(fake.server.URL, ttsModel, asrModel, httpClient)
	require.NoError(t, err)

	p, err := New(&cfg, client, log)
	require.NoError(t, err)

	return p, fake
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func writeFile(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(name+" reference audio"), 0o600))

	return path
}

func writeOrFail(t *testing.T, w io.Writer, text string) {
	_, err := io.WriteString(w, text)
	assert.NoError(t, err)
}

func serveBody(t *testing.T, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, body)
	}
}

func serveWAV(t *testing.T) http.HandlerFunc {
	return serveBody(t, upstreamAudio)
}

func serveTranscript(t *testing.T, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"type": "transcript.text.done", "text": text}))
	}
}

func stall(_ http.ResponseWriter, r *http.Request) {
	<-r.Context().Done()
}

func pipelineError(t *testing.T, err error) *Error {
	t.Helper()

	var failed *Error
	require.True(t, errors.As(err, &failed), "error %v (%T) is not a pipeline error", err, err)

	return failed
}

// requireKind fails unless err is a pipeline *Error of kind and, when message
// is not empty, with that message.
func requireKind(t *testing.T, err error, kind Kind, message string) {
	t.Helper()

	failed := pipelineError(t, err)
	assert.Equal(t, kind, failed.Kind, "kind of %q", failed.Message)
	if message != "" {
		assert.Equal(t, message, failed.Message)
	}
}

func prepare(t *testing.T, p *Pipeline, voice, input string) SpeechJob {
	t.Helper()

	job, err := p.PrepareSpeech(voice, input)
	require.NoError(t, err)

	return job
}

func readSpeech(t *testing.T, speech *Speech) string {
	t.Helper()

	data, err := io.ReadAll(speech.PCM)
	require.NoError(t, err)
	require.NoError(t, speech.PCM.Close())

	return string(data)
}
