package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
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
	// previewBytes is the transcript preview bound, written out so a changed
	// constant fails here.
	previewBytes = 2 << 10
	// transcriptLimitBytes is the upstream's bound on a transcript.
	transcriptLimitBytes = 1 << 20
	impossibleDateID     = "20261399T000000Z-00000000"
	recordingsPath       = "/recordings"
	contentTypeForm      = "application/x-www-form-urlencoded"
	recordingsPageSize   = 100
	unknownID            = "20261125T153000Z-00000000"
	recordedText         = "turn on the kitchen lights"
	scriptTranscript     = `<script>alert("x")</script> & more`
	escapedTranscript    = `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; more`
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
		assert.Equal(t, "no-store", audio.Header.Get("Cache-Control"), "audio %q", id)
		assert.NotEmpty(t, errorMessage(t, audio))

		keep := h.postForm(recordingsPath+"/"+id+"/keep", url.Values{"keep": {"true"}})
		assert.Equal(t, http.StatusNotFound, keep.StatusCode, "keep %q", id)
		assert.NotEmpty(t, errorMessage(t, keep))

		page := h.get(recordingsPath + "/" + id)
		assert.Equal(t, http.StatusNotFound, page.StatusCode, "page %q", id)
		assert.Equal(t, contentTypeJSON, page.Header.Get("Content-Type"), "page %q", id)
		assert.NotEmpty(t, errorMessage(t, page))

		edit := h.requestAs(http.MethodPost, recordingsPath+"/"+id, "127.0.0.1", "keep=true", nil)
		assert.Equal(t, http.StatusNotFound, edit.StatusCode, "edit %q", id)
		assert.NotEmpty(t, errorMessage(t, edit))
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

	for _, query := range []string{"?before=x", "?before=..%2Fx", "?before=", "?before=" + unknownID + "&before=" + unknownID, "?before=" + impossibleDateID} {
		resp := h.get(recordingsPath + query)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "query %q", query)
		assert.Contains(t, errorMessage(t, resp), "before", "query %q", query)
	}

	malformed := h.get(recordingsPath + "?before=%zz")
	assert.Equal(t, http.StatusBadRequest, malformed.StatusCode, "a malformed escape")
	assert.Equal(t, contentTypeJSON, malformed.Header.Get("Content-Type"))
	assert.Contains(t, errorMessage(t, malformed), "query")
}

func TestRecordingsPage_PastTheOldestSaysThePageIsEmpty(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 2)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	page := string(readBody(t, h.get(recordingsPath+"?before="+ids[0])))

	assert.Contains(t, page, "Showing 0 of 2 recordings.")
	assert.Contains(t, page, "No recordings on this page.")
	assert.NotContains(t, page, "No recordings yet.")
}

func TestRecordingsPage_EmptyStoreSaysNoRecordingsYet(t *testing.T) {
	h, _, _ := newRecordingHarness(t, recordedText)

	page := string(readBody(t, h.get(recordingsPath)))

	assert.Contains(t, page, "No recordings yet.")
}

func TestRecordingsPage_ExactlyOnePageHasNoOlderLink(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, recordingsPageSize)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	page := string(readBody(t, h.get(recordingsPath)))

	assert.Contains(t, page, "Showing 100 of 100 recordings.")
	assert.Contains(t, page, ids[0])
	assert.NotContains(t, page, "Older</a>")
}

func TestRecordingsPage_RefusesFraming(t *testing.T) {
	h, _, _ := newRecordingHarness(t, recordedText)

	resp := h.get(recordingsPath)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "frame-ancestors 'none'", resp.Header.Get("Content-Security-Policy"))
}

// plantTranscript writes one recording with transcript into dir and returns
// its id.
func plantTranscript(t *testing.T, dir, transcript string) string {
	t.Helper()

	id := clockStart.Format("20060102T150405Z") + "-0000abcd"
	meta, err := json.Marshal(map[string]any{
		"at": clockStart.Format(time.RFC3339Nano), "durationMs": 500, "source": "openai", "transcript": transcript, "keep": false,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+".json"), meta, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+".opus"), []byte("audio"), 0o600))

	return id
}

func TestRecordingsPage_ShowsABoundedTranscriptPreview(t *testing.T) {
	dir := t.TempDir()
	// The 'a' puts the preview bound inside a two-byte character.
	transcript := "a" + strings.Repeat("é", transcriptLimitBytes)
	plantTranscript(t, dir, transcript)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	page := string(readBody(t, h.get(recordingsPath)))

	want := transcript[:previewBytes-1] + "…"
	assert.Contains(t, page, want)
	assert.NotContains(t, page, transcript[:previewBytes+1])
	assert.Less(t, len(page), 4*previewBytes, "the page stays small")
}

func TestNewRecordingRow_ShowsThePreviewTheStoreReturns(t *testing.T) {
	rec := recordings.Recording{Transcript: strings.Repeat("x", 2*previewBytes)}

	row := newRecordingRow(&rec)

	assert.Equal(t, rec.Transcript, row.Transcript, "the store bounds the preview; the page cuts nothing more")
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
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "could not read the recording", errorMessage(t, resp))
}

func TestRecordingAudio_ForeignHostIsNotCached(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	resp := h.requestAs(http.MethodGet, recordingsPath+"/"+ids[0]+"/audio", "evil.example", "", nil)

	requireJSONError(t, resp, http.StatusForbidden)
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
		{http.MethodGet, recordingsPath + "/" + id, ""},
		{http.MethodPost, recordingsPath + "/" + id, "keep=true"},
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

		one := h.requestAs(http.MethodGet, recordingsPath+"/"+ids[0], host, "", nil)
		assert.Equal(t, http.StatusOK, one.StatusCode, "recording page as %s", host)

		edit := h.requestAs(http.MethodPost, recordingsPath+"/"+ids[0], host, "keep=true", nil)
		assert.Equal(t, http.StatusSeeOther, edit.StatusCode, "recording form as %s", host)
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

}

func requireJSONError(t *testing.T, resp *http.Response, status int) {
	t.Helper()

	assert.Equal(t, status, resp.StatusCode)
	assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.NotEmpty(t, errorMessage(t, resp))
}

func TestRecordingAudio_UnsatisfiableRangeIsAJSONError(t *testing.T) {
	dir := t.TempDir()
	id, audio := plantLongRecording(t, dir, slowReadChunk)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	resp := h.requestAs(http.MethodGet, recordingsPath+"/"+id+"/audio", "127.0.0.1", "", map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(audio)+1)})

	requireJSONError(t, resp, http.StatusRequestedRangeNotSatisfiable)
	assert.Equal(t, fmt.Sprintf("bytes */%d", len(audio)), resp.Header.Get("Content-Range"))
}

func TestRecordingAudio_MalformedRangeIsAJSONErrorWithoutContentRange(t *testing.T) {
	dir := t.TempDir()
	id, _ := plantLongRecording(t, dir, slowReadChunk)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	resp := h.requestAs(http.MethodGet, recordingsPath+"/"+id+"/audio", "127.0.0.1", "", map[string]string{"Range": "bytes=x-y"})

	requireJSONError(t, resp, http.StatusRequestedRangeNotSatisfiable)
	assert.Empty(t, resp.Header.Get("Content-Range"))
}

func TestRecordingAudio_FailedPreconditionIsAJSONError(t *testing.T) {
	dir := t.TempDir()
	id, _ := plantLongRecording(t, dir, slowReadChunk)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	resp := h.requestAs(http.MethodGet, recordingsPath+"/"+id+"/audio", "127.0.0.1", "", map[string]string{"If-Match": `"x"`})

	requireJSONError(t, resp, http.StatusPreconditionFailed)
	assert.Empty(t, resp.Header.Get("Content-Range"))
}

func TestRecordingAudio_UnchangedIsNotModified(t *testing.T) {
	dir := t.TempDir()
	id, _ := plantLongRecording(t, dir, slowReadChunk)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)
	path := recordingsPath + "/" + id + "/audio"
	whole := h.requestAs(http.MethodGet, path, "127.0.0.1", "", nil)
	modified := whole.Header.Get("Last-Modified")
	require.NotEmpty(t, modified)

	resp := h.requestAs(http.MethodGet, path, "127.0.0.1", "", map[string]string{"If-Modified-Since": modified})

	assert.Equal(t, http.StatusNotModified, resp.StatusCode)
	assert.Empty(t, readBody(t, resp))
}

// failingSeeker fails every Seek, which makes http.ServeContent answer 500.
type failingSeeker struct{ io.Reader }

func (failingSeeker) Seek(int64, int) (int64, error) {
	return 0, errors.New("disk gone")
}

func TestServeRecording_ServerErrorIsLoggedJSON(t *testing.T) {
	logs := &logBuffer{}
	s := &Server{log: slog.New(slog.NewJSONHandler(logs, nil))}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, recordingsPath+"/"+unknownID+"/audio", http.NoBody)

	s.serveRecording(recorder, req, time.Time{}, failingSeeker{strings.NewReader("audio")})

	resp := recorder.Result()
	defer closeQuietly(t, resp.Body)
	requireJSONError(t, resp, http.StatusInternalServerError)
	assert.Contains(t, logs.text(), "recording request failed")
	assert.Contains(t, logs.text(), "seek")
}

func (b *logBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

const (
	// correctTextBytes is the store's correct text bound, written out so a
	// changed constant fails here.
	correctTextBytes = 16 << 10
	spokenWords      = "turn off the kitchen lights"
	scriptCorrection = `<b>turn</b> & off`
	escapedScript    = `&lt;b&gt;turn&lt;/b&gt; &amp; off`
	flaggedMark      = "Transcription error"
)

// fetchRecordingPage fetches the page of the recording id and returns it, after
// checking the headers every page carries.
func fetchRecordingPage(t *testing.T, h *harness, id string) string {
	t.Helper()

	resp := h.get(recordingsPath + "/" + id)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, contentTypeHTML, resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	assert.Equal(t, "frame-ancestors 'none'", resp.Header.Get("Content-Security-Policy"))

	return string(readBody(t, resp))
}

func TestRecordingPage_ShowsTheWholeTranscriptAndTheFormState(t *testing.T) {
	dir := t.TempDir()
	transcript := strings.Repeat("x", 2*previewBytes) + scriptTranscript
	id := plantTranscript(t, dir, transcript)
	h, store := newRecordingHarnessIn(t, dir, recordedText)

	plain := fetchRecordingPage(t, h, id)

	assert.Contains(t, plain, strings.Repeat("x", 2*previewBytes)+escapedTranscript, "the whole transcript, escaped")
	assert.NotContains(t, plain, "<script>")
	assert.Contains(t, plain, `<audio controls preload="none" src="/recordings/`+id+`/audio">`)
	assert.Contains(t, plain, `<form method="post" action="/recordings/`+id+`">`)
	assert.Contains(t, plain, `<input type="checkbox" name="keep" value="true">`)
	assert.Contains(t, plain, `<input type="checkbox" name="transcriptionError" value="true">`)
	assert.Contains(t, plain, `<textarea name="correctText" rows="4" cols="80"></textarea>`)

	require.NoError(t, store.Annotate(id, recordings.Annotation{TranscriptionError: true, CorrectText: scriptCorrection}))
	flagged := fetchRecordingPage(t, h, id)

	assert.Contains(t, flagged, `<input type="checkbox" name="keep" value="true" checked>`)
	assert.Contains(t, flagged, `<input type="checkbox" name="transcriptionError" value="true" checked>`)
	assert.Contains(t, flagged, `<textarea name="correctText" rows="4" cols="80">`+escapedScript+`</textarea>`)
}

// postRecording posts form to the page of the recording id without
// following the redirect.
func postRecording(t *testing.T, h *harness, id, form string) *http.Response {
	t.Helper()

	return h.requestAs(http.MethodPost, recordingsPath+"/"+id, "127.0.0.1", form, nil)
}

func TestRecordingForm_SavesEachFieldAndRedirectsToThePage(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	id := ids[0]

	steps := []struct {
		form string
		want recordings.Recording
	}{
		{"keep=true", recordings.Recording{Keep: true}},
		{"", recordings.Recording{}},
		{"transcriptionError=true&correctText=" + url.QueryEscape(spokenWords), recordings.Recording{Keep: true, TranscriptionError: true, CorrectText: spokenWords}},
		{"transcriptionError=true&correctText=", recordings.Recording{Keep: true, TranscriptionError: true}},
		{"keep=true&correctText=" + url.QueryEscape(spokenWords), recordings.Recording{Keep: true}},
	}
	for _, step := range steps {
		resp := postRecording(t, h, id, step.form)

		assert.Equal(t, http.StatusSeeOther, resp.StatusCode, "form %q", step.form)
		assert.Equal(t, recordingsPath+"/"+id, resp.Header.Get("Location"), "form %q", step.form)
		got, err := store.Recording(id)
		require.NoError(t, err)
		assert.Equal(t, step.want.Keep, got.Keep, "keep after %q", step.form)
		assert.Equal(t, step.want.TranscriptionError, got.TranscriptionError, "error after %q", step.form)
		assert.Equal(t, step.want.CorrectText, got.CorrectText, "correct text after %q", step.form)
		assert.Equal(t, "planted 0", got.Transcript, "the transcript is never edited")
	}
}

// recordingFormOf returns a recording form padded with a second field to
// exactly size bytes.
func recordingFormOf(t *testing.T, size int) string {
	t.Helper()

	form := "keep=true&pad="
	require.LessOrEqual(t, len(form), size)

	return form + strings.Repeat("p", size-len(form))
}

func TestRecordingForm_AtTheLimitIsAccepted(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)

	resp := postRecording(t, h, ids[0], recordingFormOf(t, maxRecordingFormBytes))

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.True(t, onlyRecording(t, store).Keep)
}

func TestRecordingForm_RefusalsAreBadRequestAndSaveNothing(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)

	for name, form := range map[string]string{
		"form one byte over":     recordingFormOf(t, maxRecordingFormBytes+1),
		"correct text over":      "keep=true&transcriptionError=true&correctText=" + strings.Repeat("x", correctTextBytes+1),
		"correct text not UTF-8": "keep=true&transcriptionError=true&correctText=%FF",
		"keep not true":          "keep=yes",
		"error not true":         "keep=true&transcriptionError=on",
		"keep twice":             "keep=true&keep=true",
		"correct text twice":     "keep=true&transcriptionError=true&correctText=a&correctText=b",
		"malformed escape":       "keep=true&correctText=%zz",
	} {
		resp := postRecording(t, h, ids[0], form)

		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, name)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), name)
		assert.NotEmpty(t, errorMessage(t, resp), name)
	}
	got, err := store.Recording(ids[0])
	require.NoError(t, err)
	assert.False(t, got.Keep)
	assert.False(t, got.TranscriptionError)
}

func TestRecordingForm_CorrectTextAtTheLimitIsAccepted(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	text := strings.Repeat("x", correctTextBytes)

	resp := postRecording(t, h, ids[0], "transcriptionError=true&correctText="+text)

	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	got, err := store.Recording(ids[0])
	require.NoError(t, err)
	assert.Equal(t, text, got.CorrectText)
}

func TestRecordingForm_CrossSiteIsForbidden(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	path := recordingsPath + "/" + ids[0]

	for name, headers := range map[string]map[string]string{
		"cross-site fetch": {"Sec-Fetch-Site": "cross-site"},
		"foreign origin":   {"Origin": "http://evil.example"},
	} {
		resp := h.requestAs(http.MethodPost, path, "127.0.0.1", "transcriptionError=true", headers)

		assert.Equal(t, http.StatusForbidden, resp.StatusCode, name)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), name)
		assert.NotEmpty(t, errorMessage(t, resp), name)
	}
	assert.False(t, onlyRecording(t, store).TranscriptionError)
}

func TestRecordingKeep_ReleasingAFlaggedRecordingIsConflict(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	require.NoError(t, store.Annotate(ids[0], recordings.Annotation{TranscriptionError: true}))

	resp := h.requestAs(http.MethodPost, recordingsPath+"/"+ids[0]+"/keep", "127.0.0.1", "keep=false", nil)

	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
	assert.Contains(t, errorMessage(t, resp), "clear the transcription error first")
	assert.True(t, onlyRecording(t, store).Keep)
}

func TestRecordingsPage_LinksRowsAndMarksFlaggedOnesWithoutRelease(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 2)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	flagged, plain := ids[0], ids[1]
	require.NoError(t, store.Annotate(flagged, recordings.Annotation{TranscriptionError: true}))
	require.NoError(t, store.SetKeep(plain, true))

	page := string(readBody(t, h.get(recordingsPath)))

	for _, id := range ids {
		assert.Contains(t, page, `<a href="/recordings/`+id+`">`, "row %s links to its page", id)
	}
	assert.Equal(t, 1, strings.Count(page, flaggedMark), "only the flagged row is marked")
	assert.Contains(t, page, `action="/recordings/`+plain+`/keep"`, "a kept row can be released")
	assert.NotContains(t, page, `action="/recordings/`+flagged+`/keep"`, "a flagged row cannot be released")
}
