package recordings

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// skippedOldID is a planted pair dated weeks before start, so expiry
	// would reach it if the store took it for a recording nobody kept.
	skippedOldID = "20261101T000000Z-0000000e"
	unusableMeta = "{not json"
)

// skippedBytesOf reads what the store counts for the skipped pair id, and
// whether it still protects it.
func skippedBytesOf(store *Store, id string) (int64, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()

	size, ok := store.skipped[id]
	return size, ok
}

func strayOf(store *Store) map[string]int64 {
	store.mu.Lock()
	defer store.mu.Unlock()

	stray := make(map[string]int64, len(store.stray))
	for name, size := range store.stray {
		stray[name] = size
	}

	return stray
}

func fileContent(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}

func TestSave_FullStoreReportsKeptAndUnevictableBytesApart(t *testing.T) {
	dir := t.TempDir()
	skippedBytes := plantPair(t, dir, plantedCorruptID, []byte(unusableMeta))
	store, _ := openLogged(t, dir, (&clock{now: start}).Now)
	kept := save(t, store, "kept")
	require.NoError(t, store.SetKeep(kept.ID, true))
	keptBytes := recordingBytes(t, dir, kept)
	store.maxBytes = skippedBytes + keptBytes

	err := store.Save(wav(t, halfSecond), sourceOpenAI, "refused")

	require.ErrorIs(t, err, ErrFull)
	assert.Contains(t, err.Error(), "store is full")
	assert.Contains(t, err.Error(), strconv.FormatInt(keptBytes, 10)+" bytes kept")
	assert.Contains(t, err.Error(), strconv.FormatInt(skippedBytes, 10)+" bytes that no eviction can reclaim")
	assert.NotContains(t, err.Error(), strconv.FormatInt(keptBytes+skippedBytes, 10)+" bytes kept", "unusable bytes are not blamed on kept recordings")
}

func TestSave_NeverReusesTheIDOfASkippedPair(t *testing.T) {
	dir := t.TempDir()
	plantPair(t, dir, collidingID, []byte(unusableMeta))
	store, _ := openStore(t, dir)
	issued := []string{collidingID, nextID}
	store.newID = func(time.Time) (string, error) {
		id := issued[0]
		issued = issued[1:]
		return id, nil
	}

	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, "fresh"))

	assert.Equal(t, []string{nextID}, ids(t, store))
	assert.Equal(t, unusableMeta, fileContent(t, filepath.Join(dir, collidingID+jsonSuffix)), "the skipped pair's metadata is untouched")
	assert.Equal(t, "audio", fileContent(t, filepath.Join(dir, collidingID+opusSuffix)), "the skipped pair's audio is untouched")
}

func TestSave_UnevictableBytesAloneRefuseWithoutEvicting(t *testing.T) {
	dir := t.TempDir()
	skippedBytes := plantPair(t, dir, plantedCorruptID, []byte(unusableMeta))
	store, _ := openLogged(t, dir, (&clock{now: start}).Now)
	unkept := save(t, store, "unkept")
	size := recordingBytes(t, dir, unkept)
	// Nobody kept anything, yet the skipped pair leaves less than one
	// recording of room: evicting the unkept one cannot make the save fit.
	store.maxBytes = skippedBytes + size - 1
	before := names(t, dir)

	err := store.Save(wav(t, halfSecond), sourceOpenAI, "refused")

	require.ErrorIs(t, err, ErrFull)
	assert.Equal(t, before, names(t, dir), "a refused save deletes and writes nothing")
	assert.Equal(t, []string{unkept.ID}, ids(t, store))
}

func TestCollect_RecountsASkippedPairFromTheScan(t *testing.T) {
	dir := t.TempDir()
	skippedBytes := plantPair(t, dir, plantedCorruptID, []byte(unusableMeta))
	store, _ := openStore(t, dir)
	counted, ok := skippedBytesOf(store, plantedCorruptID)
	require.True(t, ok)
	require.Equal(t, skippedBytes, counted)
	grown := strings.Repeat("a", 100)
	require.NoError(t, os.WriteFile(filepath.Join(dir, plantedCorruptID+opusSuffix), []byte(grown), 0o600))

	require.NoError(t, store.Collect())

	counted, ok = skippedBytesOf(store, plantedCorruptID)
	require.True(t, ok)
	assert.Equal(t, int64(len(grown)+len(unusableMeta)), counted)
}

func TestCollect_ForgetsASkippedPairWhoseFilesAreGone(t *testing.T) {
	dir := t.TempDir()
	plantPair(t, dir, plantedCorruptID, []byte(unusableMeta))
	store, _ := openStore(t, dir)
	for _, suffix := range []string{opusSuffix, jsonSuffix} {
		require.NoError(t, os.Remove(filepath.Join(dir, plantedCorruptID+suffix)))
	}

	require.NoError(t, store.Collect())

	_, ok := skippedBytesOf(store, plantedCorruptID)
	assert.False(t, ok, "a pair the operator deleted no longer counts")
}

func TestCollect_KeepsAHalfDeletedSkippedPairCountedAtItsSurvivor(t *testing.T) {
	dir := t.TempDir()
	plantPair(t, dir, plantedCorruptID, []byte(unusableMeta))
	store, _ := openStore(t, dir)
	require.NoError(t, os.Remove(filepath.Join(dir, plantedCorruptID+opusSuffix)))

	require.NoError(t, store.Collect())

	counted, ok := skippedBytesOf(store, plantedCorruptID)
	require.True(t, ok, "the surviving half stays protected")
	assert.Equal(t, int64(len(unusableMeta)), counted)
	assert.Equal(t, unusableMeta, fileContent(t, filepath.Join(dir, plantedCorruptID+jsonSuffix)))
}

func TestCollect_RebuildsTheStrayFilesFromTheScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes files from a read-only directory")
	}
	dir := t.TempDir()
	store, _ := openLogged(t, dir, steppingNow())
	saved := []*Recording{save(t, store, "rec0"), save(t, store, "rec1")}
	size := recordingBytes(t, dir, saved[1])
	blockAudio(t, dir, saved[0])
	store.maxBytes = 2*size + 1
	require.Error(t, store.Save(wav(t, halfSecond), sourceOpenAI, "rec2"))
	require.Contains(t, strayOf(store), saved[0].ID+opusSuffix)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, saved[0].ID+opusSuffix)))
	orphan := plantedOrphanID + opusSuffix
	require.NoError(t, os.WriteFile(filepath.Join(dir, orphan), []byte("orphan"), 0o600))
	require.NoError(t, os.Chmod(dir, readOnlyDir))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, privateDir)) })

	require.Error(t, store.Collect(), "the orphan cannot be deleted from a read-only directory")

	assert.Equal(t, map[string]int64{orphan: int64(len("orphan"))}, strayOf(store), "only what this sweep could not delete counts")
}

func TestOpen_KeepsMetadataWithoutAnExplicitKeepOrTime(t *testing.T) {
	cases := map[string]string{
		"empty object": `{}`,
		"null":         `null`,
		"null keep":    `{"at":"2026-11-01T00:00:00Z","durationMs":500,"source":"openai","transcript":"x","keep":null}`,
		"missing keep": `{"at":"2026-11-01T00:00:00Z","durationMs":500,"source":"openai","transcript":"x"}`,
		"missing at":   `{"durationMs":500,"source":"openai","transcript":"x","keep":false}`,
		"zero at":      `{"at":"0001-01-01T00:00:00Z","durationMs":500,"source":"openai","transcript":"x","keep":false}`,
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			plantPair(t, dir, skippedOldID, []byte(meta))

			store, logs := openLogged(t, dir, (&clock{now: start}).Now)
			require.NoError(t, store.Collect())

			assert.Empty(t, ids(t, store))
			requireLeftOnDisk(t, logs, dir, skippedOldID)
			_, ok := skippedBytesOf(store, skippedOldID)
			assert.True(t, ok, "the pair counts against the cap")
		})
	}
}

// failStat makes the store's directory scan fail to stat the names given.
func failStat(store *Store, names ...string) {
	store.mu.Lock()
	defer store.mu.Unlock()

	store.entryInfo = func(entry fs.DirEntry) (fs.FileInfo, error) {
		for _, name := range names {
			if entry.Name() == name {
				return nil, errors.New("stat failed")
			}
		}
		return entry.Info()
	}
}

func TestCollect_AnIncompleteScanKeepsASkippedPair(t *testing.T) {
	dir := t.TempDir()
	skippedBytes := plantPair(t, dir, skippedOldID, []byte(unusableMeta))
	store, _ := openStore(t, dir)
	failStat(store, skippedOldID+opusSuffix, skippedOldID+jsonSuffix)

	require.NoError(t, store.Collect())

	counted, ok := skippedBytesOf(store, skippedOldID)
	require.True(t, ok, "names the scan listed but could not stat are not gone")
	assert.Equal(t, skippedBytes, counted, "the last size it read still counts")

	failStat(store)
	require.NoError(t, store.Collect())
	assert.Equal(t, unusableMeta, fileContent(t, filepath.Join(dir, skippedOldID+jsonSuffix)))
	assert.Equal(t, "audio", fileContent(t, filepath.Join(dir, skippedOldID+opusSuffix)))
}

func TestCollect_AnIncompleteScanKeepsASkippedPairsLastSize(t *testing.T) {
	dir := t.TempDir()
	skippedBytes := plantPair(t, dir, skippedOldID, []byte(unusableMeta))
	store, _ := openStore(t, dir)
	failStat(store, skippedOldID+opusSuffix)

	require.NoError(t, store.Collect())

	counted, ok := skippedBytesOf(store, skippedOldID)
	require.True(t, ok)
	assert.Equal(t, skippedBytes, counted, "a half the scan could not stat is not counted as empty")
}

// requireUnreadableWarning checks that the store warned once that it left
// the pair id on disk because its files could not be read.
func requireUnreadableWarning(t *testing.T, logs *logBuffer, id string) {
	t.Helper()

	warnings := logs.withMessage(t, "recording left on disk: its files cannot be read")
	require.Len(t, warnings, 1)
	assert.Equal(t, "WARN", warnings[0]["level"])
	assert.Equal(t, id, warnings[0]["id"])
}

func TestOpen_AnUnstattablePairIsLeftOnDisk(t *testing.T) {
	dir := t.TempDir()
	store, logs := openLogged(t, dir, (&clock{now: start}).Now)
	planted := plantPair(t, dir, skippedOldID, []byte(`{"at":"2026-11-01T00:00:00Z","durationMs":500,"source":"openai","transcript":"x","keep":true}`))
	failStat(store, skippedOldID+jsonSuffix)

	require.NoError(t, store.load())
	require.NoError(t, store.Collect())

	assert.FileExists(t, filepath.Join(dir, skippedOldID+opusSuffix), "the audio of a pair whose metadata could not be stat'd is kept")
	assert.FileExists(t, filepath.Join(dir, skippedOldID+jsonSuffix))
	counted, ok := skippedBytesOf(store, skippedOldID)
	require.True(t, ok)
	assert.Equal(t, planted-int64(len(`{"at":"2026-11-01T00:00:00Z","durationMs":500,"source":"openai","transcript":"x","keep":true}`)), counted, "what could be stat'd counts")
	requireUnreadableWarning(t, logs, skippedOldID)
}

func TestOpen_APairWithUnstattableAudioIsLeftOnDisk(t *testing.T) {
	dir := t.TempDir()
	store, logs := openLogged(t, dir, (&clock{now: start}).Now)
	meta := []byte(`{"at":"2026-11-01T00:00:00Z","durationMs":500,"source":"openai","transcript":"x","keep":false}`)
	plantPair(t, dir, skippedOldID, meta)
	failStat(store, skippedOldID+opusSuffix)

	require.NoError(t, store.load())
	require.NoError(t, store.Collect())

	assert.FileExists(t, filepath.Join(dir, skippedOldID+opusSuffix), "audio that could not be stat'd is kept")
	assert.FileExists(t, filepath.Join(dir, skippedOldID+jsonSuffix), "expired metadata beside it is kept too")
	assert.NotContains(t, ids(t, store), skippedOldID)
	counted, ok := skippedBytesOf(store, skippedOldID)
	require.True(t, ok, "the pair is counted as skipped")
	assert.Equal(t, int64(len(meta)), counted, "what could be stat'd counts")
	requireUnreadableWarning(t, logs, skippedOldID)
}

func TestCollect_NeverDeletesAFileItCouldNotStat(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	orphan := plantedOrphanID + opusSuffix
	require.NoError(t, os.WriteFile(filepath.Join(dir, orphan), []byte("orphan"), 0o600))
	failStat(store, orphan)

	require.NoError(t, store.Collect())

	assert.FileExists(t, filepath.Join(dir, orphan))
}
