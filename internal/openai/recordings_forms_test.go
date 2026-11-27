package openai

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/recordings"
)

const (
	// recordingFormLimit and profileFormLimit are the form bounds that
	// docs/api.md states, written out so a changed constant fails here.
	recordingFormLimit = 100928
	profileFormLimit   = 1 << 10
	// storeCapBytes is the store's cap. A skipped pair this large fills the
	// store at no cost on disk, because the audio is a sparse file.
	storeCapBytes = 1 << 30
	// skippedID names a pair whose metadata does not decode, which the store
	// leaves on disk and counts toward the cap.
	skippedID       = "20261125T120000Z-0000fff0"
	plantedProfile  = "0123456789abcdef"
	plantedProfiles = `{"profiles":[{"id":"` + plantedProfile + `","name":"Anna"}]}`
	formType        = "application/x-www-form-urlencoded"
	textareaOpen    = `<textarea name="correctText" rows="4" cols="80">`
)

// postTyped posts body to path with the Content-Type contentType, or with
// none when contentType is empty, and returns the response unfollowed.
func postTyped(t *testing.T, h *harness, path, contentType, body string) *http.Response {
	t.Helper()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { closeQuietly(t, resp.Body) })

	return resp
}

// formPost is one post to a recordings form route.
type formPost struct {
	path string
	body string
}

func formPosts(recordingID, profileID string) map[string]formPost {
	return map[string]formPost{
		"review":         {recordingsPath + "/" + recordingID, "keep=true"},
		"keep":           {recordingsPath + "/" + recordingID + "/keep", "keep=true"},
		"profile add":    {profilesPath, "name=Ben"},
		"profile rename": {profilesPath + "/" + profileID + "/rename", "name=Ben"},
		"profile delete": {profilesPath + "/" + profileID + "/delete", ""},
	}
}

func TestRecordingPosts_AnythingButAFormIsUnsupported(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	anna := addProfile(t, store, "Anna")
	require.NoError(t, store.Annotate(ids[0], recordings.Annotation{TranscriptionError: true, CorrectText: spokenWords, Profiles: []string{anna.ID}}))
	reviewed, err := store.Recording(ids[0])
	require.NoError(t, err)

	for route, post := range formPosts(ids[0], anna.ID) {
		for _, contentType := range []string{"", "text/plain", contentTypeJSON, "multipart/form-data; boundary=x", "application/x-www-form-urlencoded-x", "not a type"} {
			for _, body := range []string{post.body, ""} {
				resp := postTyped(t, h, post.path, contentType, body)

				assert.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode, "%s as %q with %q", route, contentType, body)
				assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), "%s as %q", route, contentType)
				assert.Contains(t, errorMessage(t, resp), formType, "%s as %q", route, contentType)
			}
		}
	}

	got, err := store.Recording(ids[0])
	require.NoError(t, err)
	assert.Equal(t, reviewed, got, "the review is unchanged")
	assert.Equal(t, []recordings.Profile{anna}, store.Profiles(), "no profile is added, renamed or deleted")
}

func TestRecordingPosts_AFormWithParametersOrAnyCaseIsAccepted(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)

	for i, contentType := range []string{formType + "; charset=UTF-8", "Application/X-WWW-Form-Urlencoded"} {
		review := postTyped(t, h, recordingsPath+"/"+ids[0], contentType, "keep=true")
		assert.Equal(t, http.StatusSeeOther, review.StatusCode, contentType)

		add := postTyped(t, h, profilesPath, contentType, fmt.Sprintf("name=person+%d", i))
		assert.Equal(t, http.StatusSeeOther, add.StatusCode, contentType)
	}
	assert.True(t, onlyRecording(t, store).Keep)
	assert.Len(t, store.Profiles(), 2)
}

// fillStore plants, beside the recordings in dir, a pair the store cannot
// read whose sparse audio takes the whole cap, and a profiles file with one
// profile.
func fillStore(t *testing.T, dir string) {
	t.Helper()

	audio := filepath.Join(dir, skippedID+".opus")
	require.NoError(t, os.WriteFile(audio, nil, 0o600))
	require.NoError(t, os.Truncate(audio, storeCapBytes))
	require.NoError(t, os.WriteFile(filepath.Join(dir, skippedID+".json"), []byte("{"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "profiles.json"), []byte(plantedProfiles), 0o600))
}

func TestRecordingPosts_FullStoreIsInsufficientStorage(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	fillStore(t, dir)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	metadata := filepath.Join(dir, ids[0]+".json")
	before, err := os.ReadFile(metadata)
	require.NoError(t, err)

	for route, post := range map[string]formPost{
		"review":      {recordingsPath + "/" + ids[0], "transcriptionError=true&correctText=" + url.QueryEscape(spokenWords)},
		"profile add": {profilesPath, "name=Ben"},
	} {
		resp := postRecordingForm(t, h, post.path, post.body)

		assert.Equal(t, http.StatusInsufficientStorage, resp.StatusCode, route)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), route)
		assert.Contains(t, errorMessage(t, resp), "full", route)
	}
	after, err := os.ReadFile(metadata)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the review is not saved")
	assert.Equal(t, []string{"Anna"}, profileNamesOf(store))

	deleted := postRecordingForm(t, h, profilesPath+"/"+plantedProfile+"/delete", "")
	assert.Equal(t, http.StatusSeeOther, deleted.StatusCode, "a delete is always allowed")
	assert.Empty(t, store.Profiles())
}

// postRecordingForm posts form to path as a form, unfollowed.
func postRecordingForm(t *testing.T, h *harness, path, form string) *http.Response {
	t.Helper()

	return postTyped(t, h, path, formType, form)
}

func TestProfileChoices_TicksAProfileByIDWhateverItsName(t *testing.T) {
	all := []recordings.Profile{{ID: plantedProfile, Name: "Annabel"}, {ID: "fedcba9876543210", Name: "Ben"}}
	assigned := []recordings.Profile{{ID: plantedProfile, Name: "Anna"}}

	choices := profileChoices(all, assigned)

	assert.Equal(t, []profileChoice{
		{ID: plantedProfile, Name: "Annabel", Checked: true},
		{ID: "fedcba9876543210", Name: "Ben"},
	}, choices)
}

func TestRecordingPage_KeepsALeadingNewlineOfTheCorrectText(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	require.NoError(t, store.Annotate(ids[0], recordings.Annotation{TranscriptionError: true, CorrectText: "\n" + spokenWords}))

	page := fetchRecordingPage(t, h, ids[0])

	// HTML drops one newline directly after <textarea>; the page writes one
	// of its own, so the text's own newline is what the browser shows.
	assert.Contains(t, page, textareaOpen+"\n\n"+spokenWords+"</textarea>")
}

func TestRecordingForm_LineBreaksAreSavedAsLF(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)

	for name, text := range map[string]string{
		"the most line breaks": strings.Repeat("\n", correctTextBytes),
		"lines at the limit":   strings.Repeat("abc\n", correctTextBytes/4),
		"a short text":         "turn\noff\nthe lights",
	} {
		require.LessOrEqual(t, len(text), correctTextBytes, name)
		crlf := strings.ReplaceAll(text, "\n", "\r\n")
		body := "transcriptionError=true&correctText=" + url.QueryEscape(crlf)
		require.LessOrEqual(t, len(body), recordingFormLimit, name)

		resp := postRecordingForm(t, h, recordingsPath+"/"+ids[0], body)

		require.Equal(t, http.StatusSeeOther, resp.StatusCode, name)
		got, err := store.Recording(ids[0])
		require.NoError(t, err)
		assert.Equal(t, text, got.CorrectText, name)
	}

	resp := postRecordingForm(t, h, recordingsPath+"/"+ids[0], "transcriptionError=true&correctText="+url.QueryEscape("turn\roff\r\n"))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	got, err := store.Recording(ids[0])
	require.NoError(t, err)
	assert.Equal(t, "turn\noff\n", got.CorrectText, "a lone CR is a line break too")
}

func TestRecordingPage_ShowsAKeptRecordingWithoutAnError(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	require.NoError(t, store.SetKeep(ids[0], true))

	page := fetchRecordingPage(t, h, ids[0])

	assert.Contains(t, page, `<input type="checkbox" name="keep" value="true" checked>`)
	assert.Contains(t, page, `<input type="checkbox" name="transcriptionError" value="true">`)
}

func TestFormBounds_MatchTheDocumentedValues(t *testing.T) {
	assert.Equal(t, recordingFormLimit, maxRecordingFormBytes)
	assert.Equal(t, profileFormLimit, maxProfileFormBytes)
}

func TestRecordingRoutes_UnreadableMetadataIsServerError(t *testing.T) {
	skipUnderRoot(t)
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, store := newRecordingHarnessIn(t, dir, recordedText)
	logs := &logBuffer{}
	h.service = New(h.pipeline, slog.New(slog.NewJSONHandler(logs, nil)), WithRecordings(store, nil))
	h.server.Config.Handler = h.service.Handler()
	metadata := filepath.Join(dir, ids[0]+".json")
	require.NoError(t, os.Chmod(metadata, 0))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(metadata, 0o600)) })

	page := h.get(recordingsPath + "/" + ids[0])
	edit := postRecordingForm(t, h, recordingsPath+"/"+ids[0], "keep=true")

	for name, resp := range map[string]*http.Response{"page": page, "form": edit} {
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, name)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), name)
		assert.Equal(t, "could not read the recording", errorMessage(t, resp), name)
	}
	assert.Equal(t, 2, strings.Count(logs.text(), "recording request failed"))
}

func TestProfileDelete_FormAtTheBoundIsAcceptedAndOneByteOverIsBadRequest(t *testing.T) {
	h, store, _ := newRecordingHarness(t, recordedText)
	anna := addProfile(t, store, "Anna")
	path := profilesPath + "/" + anna.ID + "/delete"
	pad := func(size int) string { return "pad=" + strings.Repeat("p", size-len("pad=")) }

	over := postRecordingForm(t, h, path, pad(profileFormLimit+1))
	assert.Equal(t, http.StatusBadRequest, over.StatusCode)
	assert.Contains(t, errorMessage(t, over), "invalid profile form")
	assert.Equal(t, []string{"Anna"}, profileNamesOf(store))

	at := postRecordingForm(t, h, path, pad(profileFormLimit))
	assert.Equal(t, http.StatusSeeOther, at.StatusCode)
	assert.Empty(t, store.Profiles())
}

func TestProfileRoutes_AStoreThatCannotWriteIsServerError(t *testing.T) {
	skipUnderRoot(t)
	h, store, dir := newRecordingHarness(t, recordedText)
	anna := addProfile(t, store, "Anna")
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, 0o700)) })

	for route, post := range map[string]formPost{
		"add":    {profilesPath, "name=Ben"},
		"rename": {profilesPath + "/" + anna.ID + "/rename", "name=Annabel"},
		"delete": {profilesPath + "/" + anna.ID + "/delete", ""},
	} {
		resp := postRecordingForm(t, h, post.path, post.body)

		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, route)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), route)
		assert.NotEmpty(t, errorMessage(t, resp), route)
	}
	assert.Equal(t, []recordings.Profile{anna}, store.Profiles())
}

func TestPages_WithoutProfilesShowTheirEmptyStatesAndTheLimit(t *testing.T) {
	dir := t.TempDir()
	ids := plantRecordings(t, dir, 1)
	h, _ := newRecordingHarnessIn(t, dir, recordedText)

	profiles := string(readBody(t, h.get(profilesPath)))
	recording := fetchRecordingPage(t, h, ids[0])

	assert.Contains(t, profiles, "No profiles yet.")
	assert.Contains(t, profiles, fmt.Sprintf("There can be at most %d profiles.", profileLimit))
	assert.NotContains(t, profiles, "<table>")
	assert.Contains(t, recording, "No speaker profiles yet.")
	assert.NotContains(t, recording, `name="profile"`)
}

func TestProfilesPage_TakesTheNameLengthFromTheView(t *testing.T) {
	const nameBytes = 7
	view := profilesView{
		Profiles:     []recordings.Profile{{ID: plantedProfile, Name: "Anna"}},
		MaxProfiles:  profileLimit,
		MaxNameBytes: nameBytes,
	}

	var page bytes.Buffer
	require.NoError(t, profilesPage.Execute(&page, &view))

	assert.Equal(t, 2, strings.Count(page.String(), fmt.Sprintf(`maxlength="%d"`, nameBytes)), "the rename and the add field")
	assert.NotContains(t, page.String(), fmt.Sprintf(`maxlength="%d"`, profileNameLimit))
}
