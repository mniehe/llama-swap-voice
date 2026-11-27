package recordings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// correctTextLimit is the correct text bound, written out so a changed
	// constant fails here.
	correctTextLimit = 16 << 10
	spokenText       = "turn off the kitchen lights"
	heardText        = "turn of the kitten lights"
	// preAnnotationMetadata is a metadata file as the store wrote it before
	// recordings could be annotated.
	preAnnotationMetadata = `{"at":"2026-11-25T12:00:00Z","durationMs":500,"source":"openai","transcript":"old","keep":false}`
)

func metadataPath(dir, id string) string {
	return filepath.Join(dir, id+jsonSuffix)
}

func fileBytes(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return data
}

// transcriptOf returns the transcript field of the metadata file at path as
// the raw JSON bytes it was written with.
func transcriptOf(t *testing.T, path string) json.RawMessage {
	t.Helper()

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fileBytes(t, path), &fields))

	return fields["transcript"]
}

func TestAnnotate_RoundTripsThroughTheFileAndAReopen(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)
	transcriptBefore := transcriptOf(t, metadataPath(dir, rec.ID))

	require.NoError(t, store.Annotate(rec.ID, Annotation{TranscriptionError: true, CorrectText: spokenText}))

	meta := readMetadata(t, metadataPath(dir, rec.ID))
	assert.True(t, meta.TranscriptionError)
	assert.Equal(t, spokenText, meta.CorrectText)
	assert.Equal(t, heardText, meta.Transcript)
	assert.Equal(t, transcriptBefore, transcriptOf(t, metadataPath(dir, rec.ID)), "the transcript bytes are unchanged")

	reopened, _ := openStore(t, dir)
	got, err := reopened.Recording(rec.ID)
	require.NoError(t, err)
	assert.Equal(t, rec.ID, got.ID)
	assert.Equal(t, heardText, got.Transcript)
	assert.True(t, got.TranscriptionError)
	assert.Equal(t, spokenText, got.CorrectText)
	assert.True(t, got.Keep)
}

func TestSave_WritesTheAnnotationFieldsExplicitly(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)

	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fileBytes(t, metadataPath(dir, rec.ID)), &fields))

	assert.JSONEq(t, `false`, string(fields["transcriptionError"]))
	assert.JSONEq(t, `""`, string(fields["correctText"]))
}

func TestOpen_LoadsMetadataWrittenBeforeAnnotations(t *testing.T) {
	dir := t.TempDir()
	plantPair(t, dir, plantedCorruptID, []byte(preAnnotationMetadata))

	store, logs := openLogged(t, dir, (&clock{now: start}).Now)

	assert.Equal(t, []string{plantedCorruptID}, ids(t, store))
	assert.Empty(t, logs.withMessage(t, "recording left on disk: its metadata is unusable"))
	got, err := store.Recording(plantedCorruptID)
	require.NoError(t, err)
	assert.Equal(t, plantedCorruptID, got.ID)
	assert.Equal(t, "old", got.Transcript)
	assert.False(t, got.TranscriptionError)
	assert.Empty(t, got.CorrectText)
	assert.False(t, got.Keep)
}

func TestAnnotate_ErrorForcesKeep(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)

	require.NoError(t, store.Annotate(rec.ID, Annotation{Keep: false, TranscriptionError: true}))

	assert.True(t, readMetadata(t, metadataPath(dir, rec.ID)).Keep)
	got, err := store.Recording(rec.ID)
	require.NoError(t, err)
	assert.True(t, got.Keep)
	assert.Empty(t, got.CorrectText, "an error without correct text means text from non-speech")
}

func TestAnnotate_KeepAloneIsSaved(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)

	require.NoError(t, store.Annotate(rec.ID, Annotation{Keep: true}))
	assert.True(t, readMetadata(t, metadataPath(dir, rec.ID)).Keep)

	require.NoError(t, store.Annotate(rec.ID, Annotation{Keep: false}))
	assert.False(t, readMetadata(t, metadataPath(dir, rec.ID)).Keep)
}

func TestSetKeep_ReleasingAFlaggedRecordingIsRefused(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)
	require.NoError(t, store.Annotate(rec.ID, Annotation{TranscriptionError: true, CorrectText: spokenText}))
	before := fileBytes(t, metadataPath(dir, rec.ID))

	err := store.SetKeep(rec.ID, false)

	require.ErrorIs(t, err, ErrFlaggedKept)
	assert.Equal(t, before, fileBytes(t, metadataPath(dir, rec.ID)), "the file is unchanged")
	got, getErr := store.Recording(rec.ID)
	require.NoError(t, getErr)
	assert.True(t, got.Keep)
	require.NoError(t, store.SetKeep(rec.ID, true), "keeping a flagged recording is no change")
}

func TestCollect_KeepsAFlaggedRecordingPastTheRetention(t *testing.T) {
	store, c := openStore(t, t.TempDir())
	flagged := save(t, store, heardText)
	plain := save(t, store, "plain")
	require.NoError(t, store.Annotate(flagged.ID, Annotation{TranscriptionError: true}))

	c.advance(Retention + time.Hour)
	require.NoError(t, store.Collect())

	assert.Equal(t, []string{flagged.ID}, ids(t, store), "the unflagged one expired")
	assert.NotEqual(t, flagged.ID, plain.ID)
}

func TestSave_CapNeverEvictsAFlaggedRecording(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	flagged := save(t, store, "flagged")
	require.NoError(t, store.Annotate(flagged.ID, Annotation{TranscriptionError: true}))
	c.advance(time.Minute)
	second := save(t, store, "second")
	size := recordingBytes(t, dir, second)
	store.maxBytes = recordingBytes(t, dir, flagged) + size + size/2

	c.advance(time.Minute)
	third := save(t, store, "third")

	assert.Equal(t, []string{third.ID, flagged.ID}, ids(t, store), "the unflagged one is evicted")
}

func TestAnnotate_ErrorOffClearsCorrectText(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)
	require.NoError(t, store.Annotate(rec.ID, Annotation{TranscriptionError: true, CorrectText: spokenText}))

	require.NoError(t, store.Annotate(rec.ID, Annotation{Keep: true, TranscriptionError: false, CorrectText: spokenText}))

	meta := readMetadata(t, metadataPath(dir, rec.ID))
	assert.False(t, meta.TranscriptionError)
	assert.Empty(t, meta.CorrectText)
	assert.True(t, meta.Keep)
}

func TestAnnotate_CorrectTextAtTheLimitIsAccepted(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)
	text := strings.Repeat("é", correctTextLimit/2)
	require.Len(t, text, correctTextLimit)

	require.NoError(t, store.Annotate(rec.ID, Annotation{TranscriptionError: true, CorrectText: text}))

	assert.Equal(t, text, readMetadata(t, metadataPath(dir, rec.ID)).CorrectText)
}

func TestAnnotate_RefusesBadCorrectTextAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, heardText)
	before := fileBytes(t, metadataPath(dir, rec.ID))

	for name, text := range map[string]string{
		"one byte over": strings.Repeat("x", correctTextLimit+1),
		"invalid UTF-8": "turn \xff off",
	} {
		err := store.Annotate(rec.ID, Annotation{TranscriptionError: true, CorrectText: text})

		require.ErrorIs(t, err, ErrInvalid, name)
		assert.Equal(t, before, fileBytes(t, metadataPath(dir, rec.ID)), name)
	}
}

func TestAnnotate_UnknownOrMalformedIDIsNotFound(t *testing.T) {
	store, _ := openStore(t, t.TempDir())

	for _, id := range []string{"20261125T153000Z-00000000", "x", "../x", impossibleDateID} {
		require.ErrorIs(t, store.Annotate(id, Annotation{Keep: true}), ErrNotFound, "annotate %q", id)
		_, err := store.Recording(id)
		require.ErrorIs(t, err, ErrNotFound, "recording %q", id)
	}
}

func TestRecording_ReturnsTheWholeTranscript(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	transcript := strings.Repeat("t", 2*maxPreviewBytes)
	rec := save(t, store, transcript)

	got, err := store.Recording(rec.ID)

	require.NoError(t, err)
	assert.Equal(t, transcript, got.Transcript)
	assert.Equal(t, 500*time.Millisecond, got.Duration)
	assert.Equal(t, sourceOpenAI, got.Source)
}
