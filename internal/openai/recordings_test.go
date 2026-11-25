package openai

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/recordings"
)

const (
	recordingsPath    = "/recordings"
	contentTypeForm   = "application/x-www-form-urlencoded"
	unknownID         = "20261125T153000Z-00000000"
	recordedText      = "turn on the kitchen lights"
	scriptTranscript  = `<script>alert("x")</script> & more`
	escapedTranscript = `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; more`
)

var clockStart = time.Date(2026, time.November, 25, 15, 30, 0, 0, time.UTC)

// newRecordingHarness serves the HTTP API with recordings on, saving into a
// fresh store.
func newRecordingHarness(t *testing.T, transcript string) (*harness, *recordings.Store, string) {
	t.Helper()

	dir := t.TempDir()
	store, err := recordings.Open(dir, steppingClock(), discardLogger())
	require.NoError(t, err)

	h := newHarness(t, serveTranscript(t, transcript), func(cfg *Config) { cfg.Recorder = store })
	h.service = New(h.pipeline, discardLogger(), WithRecordings(store))
	h.server.Config.Handler = h.service.Handler()

	return h, store, dir
}

// steppingClock moves a minute on every reading, so recordings saved one
// after the other never share a second.
func steppingClock() func() time.Time {
	var steps atomic.Int64
	return func() time.Time {
		return clockStart.Add(time.Duration(steps.Add(1)) * time.Minute)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)

	return names
}

func onlyRecording(t *testing.T, store *recordings.Store) recordings.Recording {
	t.Helper()

	list, err := store.List()
	require.NoError(t, err)
	require.Len(t, list, 1)

	return list[0]
}

func (h *harness) postForm(path string, form url.Values) *http.Response {
	h.t.Helper()

	return h.post(path, contentTypeForm, strings.NewReader(form.Encode()))
}

func TestTranscription_RecordingsOn_SavesOpusAndMetadata(t *testing.T) {
	h, store, dir := newRecordingHarness(t, recordedText)

	resp := h.transcribe(nil, []byte(streamedWAV))

	require.Equal(t, http.StatusOK, resp.StatusCode)
	rec := onlyRecording(t, store)
	assert.Equal(t, []string{rec.ID + ".json", rec.ID + ".opus"}, dirNames(t, dir))
	assert.Equal(t, "openai", rec.Source)
	assert.Equal(t, recordedText, rec.Transcript)
	assert.False(t, rec.Keep)
}

func TestRecordingsPage_ListsAndEscapesTranscript(t *testing.T) {
	h, store, _ := newRecordingHarness(t, scriptTranscript)
	require.Equal(t, http.StatusOK, h.transcribe(nil, []byte(streamedWAV)).StatusCode)
	rec := onlyRecording(t, store)

	resp := h.get(recordingsPath)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, contentTypeHTML, resp.Header.Get("Content-Type"))
	page := string(readBody(t, resp))
	assert.Contains(t, page, escapedTranscript)
	assert.NotContains(t, page, "<script>")
	assert.Contains(t, page, `<audio controls preload="none" src="/recordings/`+rec.ID+`/audio">`)
	assert.Contains(t, page, `action="/recordings/`+rec.ID+`/keep"`)
	assert.Contains(t, page, "openai")
}

func TestRecordingsPage_NewestFirst(t *testing.T) {
	h, store, _ := newRecordingHarness(t, recordedText)
	require.Equal(t, http.StatusOK, h.transcribe(nil, []byte(streamedWAV)).StatusCode)
	older := onlyRecording(t, store)
	require.Equal(t, http.StatusOK, h.transcribe(nil, []byte(streamedWAV)).StatusCode)
	list, err := store.List()
	require.NoError(t, err)
	require.Len(t, list, 2)
	newer := list[0]
	require.NotEqual(t, older.ID, newer.ID)

	page := string(readBody(t, h.get(recordingsPath)))

	newerAt := strings.Index(page, newer.ID)
	olderAt := strings.Index(page, older.ID)
	require.GreaterOrEqual(t, newerAt, 0)
	require.GreaterOrEqual(t, olderAt, 0)
	assert.Less(t, newerAt, olderAt, "the newest recording comes first")
	assert.Contains(t, page, `<time datetime="`+newer.At.Format(time.RFC3339)+`">`)
}

func TestRecordingAudio_ServedAsOgg(t *testing.T) {
	h, store, dir := newRecordingHarness(t, recordedText)
	require.Equal(t, http.StatusOK, h.transcribe(nil, []byte(streamedWAV)).StatusCode)
	rec := onlyRecording(t, store)

	resp := h.get(recordingsPath + "/" + rec.ID + "/audio")

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "audio/ogg", resp.Header.Get("Content-Type"))
	want, err := os.ReadFile(filepath.Join(dir, rec.ID+".opus"))
	require.NoError(t, err)
	assert.Equal(t, want, readBody(t, resp))
}

func TestRecordingKeep_PersistsAndRedirects(t *testing.T) {
	h, store, _ := newRecordingHarness(t, recordedText)
	require.Equal(t, http.StatusOK, h.transcribe(nil, []byte(streamedWAV)).StatusCode)
	rec := onlyRecording(t, store)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	keepPath := h.server.URL + recordingsPath + "/" + rec.ID + "/keep"

	for _, keep := range []bool{true, false, true} {
		value := "false"
		if keep {
			value = "true"
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, keepPath, strings.NewReader("keep="+value))
		require.NoError(t, err)
		req.Header.Set("Content-Type", contentTypeForm)
		resp, err := client.Do(req)
		require.NoError(t, err)
		closeQuietly(t, resp.Body)

		assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
		assert.Equal(t, recordingsPath, resp.Header.Get("Location"))
		assert.Equal(t, keep, onlyRecording(t, store).Keep)
	}
}

func TestRecordingKeep_InvalidValueIsBadRequest(t *testing.T) {
	h, store, _ := newRecordingHarness(t, recordedText)
	require.Equal(t, http.StatusOK, h.transcribe(nil, []byte(streamedWAV)).StatusCode)
	rec := onlyRecording(t, store)

	for _, form := range []url.Values{{"keep": {"yes"}}, {}, {"keep": {strings.Repeat("t", maxKeepFormBytes)}}} {
		resp := h.postForm(recordingsPath+"/"+rec.ID+"/keep", form)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "form %v", form)
		assert.NotEmpty(t, errorMessage(t, resp))
	}
	assert.False(t, onlyRecording(t, store).Keep)
}

func TestRecordingRoutes_InvalidOrUnknownIDIsNotFound(t *testing.T) {
	h, _, dir := newRecordingHarness(t, recordedText)
	planted := filepath.Join(dir, "x.opus")
	require.NoError(t, os.WriteFile(planted, []byte("not yours"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x.json"), []byte(`{"keep":false}`), 0o600))

	for _, id := range []string{"x", "..%2Fx", unknownID, "20261125T153000Z-ZZZZZZZZ"} {
		audio := h.get(recordingsPath + "/" + id + "/audio")
		assert.Equal(t, http.StatusNotFound, audio.StatusCode, "audio %q", id)
		assert.Equal(t, contentTypeJSON, audio.Header.Get("Content-Type"))
		assert.NotEmpty(t, errorMessage(t, audio))

		keep := h.postForm(recordingsPath+"/"+id+"/keep", url.Values{"keep": {"true"}})
		assert.Equal(t, http.StatusNotFound, keep.StatusCode, "keep %q", id)
		assert.NotEmpty(t, errorMessage(t, keep))
	}

	data, err := os.ReadFile(filepath.Join(dir, "x.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"keep":false}`, string(data))
}

func TestRecordingRoutes_NotUnderV1(t *testing.T) {
	h, _, _ := newRecordingHarness(t, recordedText)

	resp := h.get("/v1" + recordingsPath)

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestRecordingRoutes_DisabledAreNotFound(t *testing.T) {
	h := newHarness(t, serveTranscript(t, recordedText))

	for _, resp := range []*http.Response{
		h.get(recordingsPath),
		h.get(recordingsPath + "/" + unknownID + "/audio"),
		h.postForm(recordingsPath+"/"+unknownID+"/keep", url.Values{"keep": {"true"}}),
	} {
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
		assert.Contains(t, errorMessage(t, resp), "not found")
	}
}
