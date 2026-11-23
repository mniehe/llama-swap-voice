package openai

import (
	"bytes"
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

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	ttsModel        = "qwen3-tts"
	asrModel        = "qwen3-asr"
	aliceRef        = "alice reference audio"
	narratorRef     = "narrator reference audio"
	waitTimeout     = time.Second
	stuckTimeout    = 3 * time.Second
	ttsRoute        = "/upstream/" + ttsModel + "/tts"
	qwenTTSRoute    = "/upstream/" + ttsModel + "/v1/tts"
	asrRoute        = "/upstream/" + asrModel + "/v1/audio/transcriptions"
	upstreamWAVs    = "RIFF\xff\xff\xff\xffWAVEfmt "
	upstreamHeader  = upstreamWAVs + "\x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xff"
	upstreamAudio   = upstreamHeader + "audio frames"
	speechPath      = "/v1/audio/speech"
	fakeFormMemory  = 1 << 20
	harnessTimeout  = 10 * time.Second
	harnessMaxChars = 100
	normalSpeed     = 1.0
	transcodedName  = "audio.wav"
)

// Config, errBusy and errShuttingDown keep the names these tests used when
// the HTTP package owned them; the busy text is pinned here as public text.
type Config = pipeline.Config

var (
	errBusy         = errors.New("too many concurrent requests, retry shortly")
	errShuttingDown = pipeline.ErrShuttingDown
)

type upstreamCall struct {
	path        string
	contentType string
	fields      map[string]string
	files       map[string][]byte
	names       map[string]string
	json        map[string]any
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
	call := upstreamCall{
		path:        r.URL.Path,
		contentType: r.Header.Get("Content-Type"),
		fields:      map[string]string{},
		files:       map[string][]byte{},
		names:       map[string]string{},
	}

	read := f.readMultipart
	if call.contentType == contentTypeJSON {
		read = readJSON
	}

	if err := read(r, &call); err != nil {
		assert.NoError(f.t, err, "upstream got an invalid request")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	f.respond(w, r)
}

func (f *fakeUpstream) readMultipart(r *http.Request, call *upstreamCall) error {
	if err := r.ParseMultipartForm(fakeFormMemory); err != nil {
		return err
	}

	for key, values := range r.MultipartForm.Value {
		call.fields[key] = values[0]
	}

	for key, headers := range r.MultipartForm.File {
		call.files[key] = readPart(f.t, headers[0])
		call.names[key] = headers[0].Filename
	}

	return nil
}

func readJSON(r *http.Request, call *upstreamCall) error {
	return json.NewDecoder(r.Body).Decode(&call.json)
}

func readPart(t *testing.T, header *multipart.FileHeader) []byte {
	file, err := header.Open()
	if !assert.NoError(t, err) {
		return nil
	}
	defer closeQuietly(t, file)

	data, err := io.ReadAll(file)
	assert.NoError(t, err)

	return data
}

func (f *fakeUpstream) lastCall() upstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	require.NotEmpty(f.t, f.calls, "upstream was not called")

	return f.calls[len(f.calls)-1]
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

type harness struct {
	t        *testing.T
	upstream *fakeUpstream
	client   *upstream.Client
	pipeline *pipeline.Pipeline
	service  *Server
	server   *httptest.Server
	cfg      Config
}

func newHarness(t *testing.T, respond http.HandlerFunc, tweak ...func(*Config)) *harness {
	t.Helper()

	return newBackendHarness(t, upstream.LlamaCPP, respond, tweak...)
}

func newBackendHarness(t *testing.T, backend upstream.Backend, respond http.HandlerFunc, tweak ...func(*Config)) *harness {
	t.Helper()

	dir := t.TempDir()
	alice := writeFile(t, dir, "alice.mp3", aliceRef)
	narrator := writeFile(t, dir, "narrator.wav", narratorRef)

	h := &harness{t: t, upstream: newFakeUpstream(t, respond)}
	h.cfg = Config{
		TTSModel:         ttsModel,
		ASRModel:         asrModel,
		Voices:           map[string]string{"alice": alice, "bm_fable": alice, "narrator": narrator},
		DefaultVoice:     "alice",
		Lang:             "en",
		RequestTimeout:   harnessTimeout,
		MaxInputChars:    harnessMaxChars,
		MaxConcurrent:    4,
		MaxConcurrentTTS: 8,
		TTSConcurrency:   1,
	}

	for _, apply := range tweak {
		apply(&h.cfg)
	}

	var err error
	h.client, err = upstream.New(h.upstream.server.URL, ttsModel, asrModel, &http.Client{}, upstream.WithBackend(backend))
	require.NoError(t, err)

	h.serve(discardLogger())

	return h
}

// serve builds the pipeline and the service on log and starts serving them.
func (h *harness) serve(log *slog.Logger) {
	h.t.Helper()

	var err error
	h.pipeline, err = pipeline.New(&h.cfg, h.client, log)
	require.NoError(h.t, err)

	h.service = New(h.pipeline, log)
	h.server = httptest.NewServer(h.service.Handler())
	h.t.Cleanup(h.server.Close)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func (h *harness) speech(path string, body any) *http.Response {
	h.t.Helper()

	payload, err := json.Marshal(body)
	require.NoError(h.t, err)

	return h.post(path, "application/json", bytes.NewReader(payload))
}

func (h *harness) transcribe(fields map[string]string, audio []byte) *http.Response {
	h.t.Helper()

	contentType, body := transcriptionForm(h.t, fields, audio)

	return h.post(transcriptionPath, contentType, bytes.NewReader(body))
}

func transcriptionForm(t *testing.T, fields map[string]string, audio []byte) (contentType string, body []byte) {
	t.Helper()

	buf := &bytes.Buffer{}
	form := multipart.NewWriter(buf)
	part, err := form.CreateFormFile("file", "clip.wav")
	require.NoError(t, err)

	_, err = part.Write(audio)
	require.NoError(t, err)

	for key, value := range fields {
		require.NoError(t, form.WriteField(key, value))
	}
	require.NoError(t, form.Close())

	return form.FormDataContentType(), buf.Bytes()
}

func (h *harness) post(path, contentType string, body io.Reader) *http.Response {
	h.t.Helper()

	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.server.URL+path, body)
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", contentType)

	return h.do(req)
}

func (h *harness) get(path string) *http.Response {
	h.t.Helper()

	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.server.URL+path, http.NoBody)
	require.NoError(h.t, err)

	return h.do(req)
}

func (h *harness) do(req *http.Request) *http.Response {
	h.t.Helper()

	resp, err := http.DefaultClient.Do(req)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { closeQuietly(h.t, resp.Body) })

	return resp
}

func closeQuietly(t *testing.T, closer io.Closer) {
	assert.NoError(t, closer.Close())
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return data
}

func errorMessage(t *testing.T, resp *http.Response) string {
	t.Helper()

	var body errorBody
	require.NoError(t, json.Unmarshal(readBody(t, resp), &body), "error body is not JSON")
	require.NotEmpty(t, body.Error.Message, "error body has no error.message")

	return body.Error.Message
}

func writeOrFail(t *testing.T, w io.Writer, text string) {
	_, err := io.WriteString(w, text)
	assert.NoError(t, err)
}

func serveWAV(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, upstreamAudio)
	}
}

func serveTranscript(t *testing.T, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"type": "transcript.text.done", "text": text}))
	}
}
