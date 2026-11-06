package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/fakeffmpeg"
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
	asrRoute        = "/upstream/" + asrModel + "/v1/audio/transcriptions"
	upstreamWAVs    = "RIFF\xff\xff\xff\xffWAVEfmt "
	upstreamAudio   = upstreamWAVs + "audio frames"
	speechPath      = "/v1/audio/speech"
	fakeFormMemory  = 1 << 20
	harnessTimeout  = 10 * time.Second
	harnessMaxChars = 100
)

func TestMain(m *testing.M) {
	fakeffmpeg.RunIfRequested()
	os.Exit(m.Run())
}

type upstreamCall struct {
	path   string
	fields map[string]string
	files  map[string][]byte
	names  map[string]string
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
	if err := r.ParseMultipartForm(fakeFormMemory); err != nil {
		assert.NoError(f.t, err, "upstream got invalid multipart")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	call := upstreamCall{path: r.URL.Path, fields: map[string]string{}, files: map[string][]byte{}, names: map[string]string{}}
	for key, values := range r.MultipartForm.Value {
		call.fields[key] = values[0]
	}

	for key, headers := range r.MultipartForm.File {
		call.files[key] = readPart(f.t, headers[0])
		call.names[key] = headers[0].Filename
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

func (f *fakeUpstream) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.calls)
}

type harness struct {
	t        *testing.T
	upstream *fakeUpstream
	ffmpeg   *fakeffmpeg.Fake
	service  *Server
	server   *httptest.Server
	cfg      Config
}

func newHarness(t *testing.T, respond http.HandlerFunc, tweak ...func(*Config)) *harness {
	t.Helper()

	dir := t.TempDir()
	alice := writeFile(t, dir, "alice.mp3", aliceRef)
	narrator := writeFile(t, dir, "narrator.wav", narratorRef)

	h := &harness{t: t, upstream: newFakeUpstream(t, respond), ffmpeg: fakeffmpeg.Install(t)}
	h.cfg = Config{
		TTSModel:       ttsModel,
		ASRModel:       asrModel,
		Voices:         map[string]string{"alice": alice, "bm_fable": alice, "narrator": narrator},
		DefaultVoice:   "alice",
		Lang:           "en",
		FFmpeg:         h.ffmpeg.Path,
		RequestTimeout: harnessTimeout,
		MaxInputChars:  harnessMaxChars,
		MaxConcurrent:  4,
	}

	for _, apply := range tweak {
		apply(&h.cfg)
	}

	client, err := upstream.New(h.upstream.server.URL, ttsModel, asrModel, &http.Client{})
	require.NoError(t, err)

	h.service, err = New(&h.cfg, client, discardLogger())
	require.NoError(t, err)

	h.server = httptest.NewServer(h.service.Handler())
	t.Cleanup(h.server.Close)

	return h
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

	buf := &bytes.Buffer{}
	form := multipart.NewWriter(buf)
	part, err := form.CreateFormFile("file", "clip.wav")
	require.NoError(h.t, err)

	_, err = part.Write(audio)
	require.NoError(h.t, err)

	for key, value := range fields {
		require.NoError(h.t, form.WriteField(key, value))
	}
	require.NoError(h.t, form.Close())

	return h.post("/v1/audio/transcriptions", form.FormDataContentType(), buf)
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

func containsRun(haystack []string, run ...string) bool {
	for i := range haystack {
		if i+len(run) <= len(haystack) && slices.Equal(haystack[i:i+len(run)], run) {
			return true
		}
	}

	return false
}
