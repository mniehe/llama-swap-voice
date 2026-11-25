package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/recordings"
)

const (
	recordingsPath     = "/recordings"
	contentTypeForm    = "application/x-www-form-urlencoded"
	recordingsPageSize = 100
	unknownID          = "20261125T153000Z-00000000"
	recordedText       = "turn on the kitchen lights"
	scriptTranscript   = `<script>alert("x")</script> & more`
	escapedTranscript  = `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; more`
)

var clockStart = time.Date(2026, time.November, 25, 15, 30, 0, 0, time.UTC)

// newRecordingHarness serves the HTTP API with recordings on, saving into a
// fresh store.
func newRecordingHarness(t *testing.T, transcript string) (*harness, *recordings.Store, string) {
	t.Helper()

	dir := t.TempDir()
	h, store := newRecordingHarnessIn(t, dir, transcript)

	return h, store, dir
}

// newRecordingHarnessIn is newRecordingHarness over the recordings already in
// dir.
func newRecordingHarnessIn(t *testing.T, dir, transcript string) (*harness, *recordings.Store) {
	t.Helper()

	return newRecordingHarnessAllowing(t, dir, transcript, nil)
}

// newRecordingHarnessAllowing is newRecordingHarnessIn with hosts allowed on
// the recordings routes besides loopback.
func newRecordingHarnessAllowing(t *testing.T, dir, transcript string, hosts []string) (*harness, *recordings.Store) {
	t.Helper()

	store, err := recordings.Open(dir, steppingClock(), discardLogger())
	require.NoError(t, err)

	h := newHarness(t, serveTranscript(t, transcript), func(cfg *Config) { cfg.Recorder = store })
	h.service = New(h.pipeline, discardLogger(), WithRecordings(store, hosts))
	h.server.Config.Handler = h.service.Handler()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Run(ctx, ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return h, store
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

// waitForRecordings waits until the worker has saved count recordings and
// returns them, newest first.
func waitForRecordings(t *testing.T, store *recordings.Store, count int) []recordings.Recording {
	t.Helper()

	var list []recordings.Recording
	require.Eventually(t, func() bool {
		var err error
		list, _, err = store.List("", math.MaxInt)
		return err == nil && len(list) == count
	}, 2*time.Second, 5*time.Millisecond, "want %d recordings", count)

	return list
}

func onlyRecording(t *testing.T, store *recordings.Store) recordings.Recording {
	t.Helper()

	return waitForRecordings(t, store, 1)[0]
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
	newer := waitForRecordings(t, store, 2)[0]
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

// plantRecordings writes count recordings a minute apart into dir, as the
// store would have saved them, and returns their ids oldest first.
func plantRecordings(t *testing.T, dir string, count int) []string {
	t.Helper()

	ids := make([]string, 0, count)
	for i := range count {
		at := clockStart.Add(time.Duration(i) * time.Minute)
		id := at.Format("20060102T150405Z") + fmt.Sprintf("-%08x", i)
		meta := fmt.Sprintf(`{"at":%q,"durationMs":500,"source":"openai","transcript":"planted %d","keep":false}`, at.Format(time.RFC3339Nano), i)
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".json"), []byte(meta), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, id+".opus"), []byte("audio"), 0o600))
		ids = append(ids, id)
	}

	return ids
}

func TestRecordingsPage_ShowsTheNewestPageAndLinksToOlder(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, recordingsPageSize+1)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	resp := h.get(recordingsPath)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	page := string(readBody(t, resp))
	assert.Contains(t, page, "Showing 100 of 101 recordings.")
	assert.NotContains(t, page, ids[0], "the oldest is on the next page")
	assert.Contains(t, page, ids[1])
	assert.Contains(t, page, ids[len(ids)-1])
	assert.Contains(t, page, `<a href="/recordings?before=`+ids[1]+`">Older</a>`)

	older := string(readBody(t, h.get(recordingsPath+"?before="+ids[1])))
	assert.Contains(t, older, "Showing 1 of 101 recordings.")
	assert.Contains(t, older, ids[0])
	assert.NotContains(t, older, ids[1])
	assert.NotContains(t, older, "Older</a>")
}

func TestRecordingsPage_InvalidBeforeIsBadRequest(t *testing.T) {
	h, _, _ := newRecordingHarness(t, recordedText)

	for _, query := range []string{"?before=x", "?before=..%2Fx", "?before=", "?before=" + unknownID + "&before=" + unknownID} {
		resp := h.get(recordingsPath + query)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "query %q", query)
		assert.Contains(t, errorMessage(t, resp), "before", "query %q", query)
	}
}

// keepFormOf returns a keep form padded with a second field to exactly size
// bytes.
func keepFormOf(t *testing.T, size int) string {
	t.Helper()

	form := "keep=true&pad="
	require.LessOrEqual(t, len(form), size)

	return form + strings.Repeat("p", size-len(form))
}

func TestRecordingKeep_FormAtTheLimitIsAccepted(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)

	resp := h.postUnfollowed(recordingsPath+"/"+ids[0]+"/keep", contentTypeForm, strings.NewReader(keepFormOf(t, maxKeepFormBytes)))

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.True(t, onlyRecording(t, store).Keep)
}

func TestRecordingKeep_FormOneByteOverIsBadRequest(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)

	resp := h.post(recordingsPath+"/"+ids[0]+"/keep", contentTypeForm, strings.NewReader(keepFormOf(t, maxKeepFormBytes+1)))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "invalid keep form")
	assert.False(t, onlyRecording(t, store).Keep)
}

// postUnfollowed posts body to path and returns the response itself, not
// where a redirect leads.
func (h *harness) postUnfollowed(path, contentType string, body io.Reader) *http.Response {
	h.t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.server.URL+path, body)
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { closeQuietly(h.t, resp.Body) })

	return resp
}

func skipUnderRoot(t *testing.T) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("root reads files whatever their mode")
	}
}

func TestRecordingsPage_UnreadableStoreIsServerError(t *testing.T) {
	skipUnderRoot(t)
	dir := t.TempDir()
	plantRecordings(t, dir, 1)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)
	require.NoError(t, os.Chmod(dir, 0))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, 0o700)) })

	resp := h.get(recordingsPath)

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
	assert.Equal(t, "could not list recordings", errorMessage(t, resp))
}

func TestRecordingAudio_UnreadableFileIsServerError(t *testing.T) {
	skipUnderRoot(t)
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)
	require.NoError(t, os.Chmod(filepath.Join(dir, ids[0]+".opus"), 0))

	resp := h.get(recordingsPath + "/" + ids[0] + "/audio")

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
	assert.Equal(t, "could not read the recording", errorMessage(t, resp))
}

// requestAs sends method to path with the Host header set to host and the
// given extra headers, and returns the response without following a
// redirect.
func (h *harness) requestAs(method, path, host, body string, headers map[string]string) *http.Response {
	h.t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(h.t.Context(), method, h.server.URL+path, strings.NewReader(body))
	require.NoError(h.t, err)
	req.Host = host
	if body != "" {
		req.Header.Set("Content-Type", contentTypeForm)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { closeQuietly(h.t, resp.Body) })

	return resp
}

// recordingRequest is one request to each recordings route.
type recordingRequest struct {
	method string
	path   string
	body   string
}

func recordingRequests(id string) []recordingRequest {
	return []recordingRequest{
		{http.MethodGet, recordingsPath, ""},
		{http.MethodGet, recordingsPath + "/" + id + "/audio", ""},
		{http.MethodPost, recordingsPath + "/" + id + "/keep", "keep=true"},
	}
}

func TestRecordingRoutes_ForeignHostIsForbidden(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessAllowing(t, dir, recordedText, []string{"voice.example", "proxy.example:8443"})

	for _, host := range []string{"evil.example", "evil.example:8920", "voice.example.evil.example", "proxy.example", "proxy.example:443", "127.0.0.2"} {
		for _, req := range recordingRequests(ids[0]) {
			resp := h.requestAs(req.method, req.path, host, req.body, map[string]string{"X-Forwarded-Host": "127.0.0.1"})

			assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s %s as %s", req.method, req.path, host)
			assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
			assert.NotEmpty(t, errorMessage(t, resp))
		}
	}
	assert.False(t, onlyRecording(t, store).Keep)
}

func TestRecordingRoutes_AllowedHostsAreServed(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, _ := newRecordingHarnessAllowing(t, dir, recordedText, []string{"Voice.Example", "proxy.example:8443"})

	for _, host := range []string{"127.0.0.1", "127.0.0.1:8920", "localhost", "localhost:8920", "[::1]", "[::1]:8920", "voice.example", "VOICE.example:443", "proxy.example:8443"} {
		page := h.requestAs(http.MethodGet, recordingsPath, host, "", nil)
		assert.Equal(t, http.StatusOK, page.StatusCode, "page as %s", host)

		audio := h.requestAs(http.MethodGet, recordingsPath+"/"+ids[0]+"/audio", host, "", nil)
		assert.Equal(t, http.StatusOK, audio.StatusCode, "audio as %s", host)

		keep := h.requestAs(http.MethodPost, recordingsPath+"/"+ids[0]+"/keep", host, "keep=true", nil)
		assert.Equal(t, http.StatusSeeOther, keep.StatusCode, "keep as %s", host)
	}
}

func TestRecordingKeep_CrossSiteIsForbidden(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	path := recordingsPath + "/" + ids[0] + "/keep"

	for name, headers := range map[string]map[string]string{
		"cross-site fetch": {"Sec-Fetch-Site": "cross-site"},
		"same-site fetch":  {"Sec-Fetch-Site": "same-site"},
		"foreign origin":   {"Origin": "http://evil.example"},
	} {
		resp := h.requestAs(http.MethodPost, path, "127.0.0.1", "keep=true", headers)

		assert.Equal(t, http.StatusForbidden, resp.StatusCode, name)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), name)
		assert.NotEmpty(t, errorMessage(t, resp), name)
	}
	assert.False(t, onlyRecording(t, store).Keep)

	sameOrigin := h.requestAs(http.MethodPost, path, "127.0.0.1", "keep=true", map[string]string{"Sec-Fetch-Site": "same-origin"})
	assert.Equal(t, http.StatusSeeOther, sameOrigin.StatusCode, "the page's own form")
}

func TestRecordingKeep_DuplicateValueIsBadRequest(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)

	for _, form := range []string{"keep=true&keep=true", "keep=true&keep=false", "keep=false&keep=true"} {
		resp := h.requestAs(http.MethodPost, recordingsPath+"/"+ids[0]+"/keep", "127.0.0.1", form, nil)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "form %q", form)
		assert.Contains(t, errorMessage(t, resp), "keep", "form %q", form)
	}
	assert.False(t, onlyRecording(t, store).Keep)
}

const (
	// longRecordingBytes outlasts any loopback socket buffer, so a slow
	// reader holds the handler's writes for as long as it takes.
	longRecordingBytes = 16 << 20
	slowReadChunk      = 64 << 10
	slowReadPause      = 8 * time.Millisecond
	shortControl       = 300 * time.Millisecond
)

// plantLongRecording writes one recording whose audio is size bytes of a
// repeating pattern and returns its id and audio.
func plantLongRecording(t *testing.T, dir string, size int) (id string, audio []byte) {
	t.Helper()

	ids := plantRecordings(t, dir, 1)
	audio = make([]byte, size)
	for i := range audio {
		audio[i] = byte(i % 251)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ids[0]+".opus"), audio, 0o600))

	return ids[0], audio
}

// readSlowly reads body a chunk at a time with a pause between chunks, as a
// browser playing the audio would.
func readSlowly(body io.Reader) ([]byte, error) {
	var got []byte
	chunk := make([]byte, slowReadChunk)
	for {
		n, err := body.Read(chunk)
		got = append(got, chunk[:n]...)
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		time.Sleep(slowReadPause)
	}
}

func TestRecordingAudio_OutlivesTheControlDeadline(t *testing.T) {
	dir := t.TempDir()
	id, want := plantLongRecording(t, dir, longRecordingBytes)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)
	h.service.controlTimeout = shortControl
	h.server.Config.Handler = h.service.Handler()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.server.URL+recordingsPath+"/"+id+"/audio", http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer closeQuietly(t, resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	began := time.Now()
	got, err := readSlowly(resp.Body)

	require.NoError(t, err, "after %d of %d bytes", len(got), len(want))
	assert.Greater(t, time.Since(began), shortControl, "the read outlasted the control deadline")
	assert.Equal(t, len(want), len(got))
	assert.True(t, bytes.Equal(want, got), "the audio arrived intact")
}

func TestRecordingAudio_ServesRangesAndIsNotCached(t *testing.T) {
	dir := t.TempDir()
	id, audio := plantLongRecording(t, dir, slowReadChunk)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)
	path := recordingsPath + "/" + id + "/audio"

	whole := h.requestAs(http.MethodGet, path, "127.0.0.1", "", nil)
	assert.Equal(t, http.StatusOK, whole.StatusCode)
	assert.Equal(t, "no-store", whole.Header.Get("Cache-Control"))
	assert.Equal(t, "audio/ogg", whole.Header.Get("Content-Type"))
	assert.Equal(t, "bytes", whole.Header.Get("Accept-Ranges"))

	part := h.requestAs(http.MethodGet, path, "127.0.0.1", "", map[string]string{"Range": "bytes=10-19"})
	assert.Equal(t, http.StatusPartialContent, part.StatusCode)
	assert.Equal(t, fmt.Sprintf("bytes 10-19/%d", len(audio)), part.Header.Get("Content-Range"))
	assert.Equal(t, "no-store", part.Header.Get("Cache-Control"))
	assert.Equal(t, audio[10:20], readBody(t, part))

	head := h.requestAs(http.MethodHead, path, "127.0.0.1", "", nil)
	assert.Equal(t, http.StatusOK, head.StatusCode)
	assert.Equal(t, strconv.Itoa(len(audio)), head.Header.Get("Content-Length"))
	assert.Empty(t, readBody(t, head))

	beyond := h.requestAs(http.MethodGet, path, "127.0.0.1", "", map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(audio)+1)})
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, beyond.StatusCode)
}
