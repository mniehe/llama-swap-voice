package recordings

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
)

const (
	sourceOpenAI  = "openai"
	sourceWyoming = "wyoming"
	halfSecond    = audio.RecordingRate / 2
	day           = 24 * time.Hour
	privateDir    = 0o700
	readOnlyDir   = 0o500
)

var start = time.Date(2026, time.November, 25, 15, 30, 0, 0, time.UTC)

// clock is a test clock that moves only when told to.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// wav is a 16 kHz mono WAV of samples of a quiet ramp, as
// audio.NormaliseUpload returns it.
func wav(t *testing.T, samples int) []byte {
	t.Helper()

	pcm := make([]byte, 0, samples*2)
	for i := range samples {
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(i%1000))
	}
	header, err := audio.WAVHeader(audio.RecordingRate, 2, 1, len(pcm))
	require.NoError(t, err)

	return append(header, pcm...)
}

func openStore(t *testing.T, dir string) (*Store, *clock) {
	t.Helper()

	c := &clock{now: start}
	store, err := Open(dir, c.Now, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	return store, c
}

func names(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	found := make([]string, 0, len(entries))
	for _, entry := range entries {
		found = append(found, entry.Name())
	}
	slices.Sort(found)

	return found
}

type fileFields struct {
	At         string `json:"at"`
	DurationMs int64  `json:"durationMs"`
	Source     string `json:"source"`
	Transcript string `json:"transcript"`
	Keep       bool   `json:"keep"`
}

func readMetadata(t *testing.T, path string) fileFields {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var meta fileFields
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&meta))

	return meta
}

func ids(t *testing.T, store *Store) []string {
	t.Helper()

	list, _, err := store.List("", math.MaxInt)
	require.NoError(t, err)

	found := make([]string, 0, len(list))
	for i := range list {
		found = append(found, list[i].ID)
	}

	return found
}

// save saves a recording and returns it, telling it apart from others saved
// in the same second by its id.
func save(t *testing.T, store *Store, transcript string) *Recording {
	t.Helper()

	before := ids(t, store)
	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, transcript))
	list, _, err := store.List("", math.MaxInt)
	require.NoError(t, err)

	for i := range list {
		if !slices.Contains(before, list[i].ID) {
			return &list[i]
		}
	}
	require.FailNow(t, "the saved recording is not listed")

	return nil
}

func TestOpen_CreatesPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recordings")

	openStore(t, dir)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(privateDir), info.Mode().Perm())
}

func TestOpen_UnwritableDirectoryFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, readOnlyDir))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, privateDir)) })

	_, err := Open(dir, time.Now, slog.New(slog.DiscardHandler))

	require.Error(t, err)
	assert.Contains(t, err.Error(), dir)
}

func TestSave_WritesOpusAndMetadata(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)

	require.NoError(t, store.Save(wav(t, halfSecond), sourceWyoming, "turn on the lights"))

	found := names(t, dir)
	require.Len(t, found, 2)
	id := strings.TrimSuffix(found[0], jsonSuffix)
	assert.Equal(t, []string{id + jsonSuffix, id + opusSuffix}, found)
	assert.True(t, ValidID(id), "id %q", id)
	assert.True(t, strings.HasPrefix(id, "20261125T153000Z-"), "id %q", id)
	assert.Equal(t, fileFields{
		At:         "2026-11-25T15:30:00Z",
		DurationMs: 500,
		Source:     sourceWyoming,
		Transcript: "turn on the lights",
		Keep:       false,
	}, readMetadata(t, filepath.Join(dir, id+jsonSuffix)))

	opus, err := os.ReadFile(filepath.Join(dir, id+opusSuffix))
	require.NoError(t, err)
	decoded, err := audio.NormaliseUpload(t.Context(), bytes.NewReader(opus), audio.Limits{MaxDuration: time.Minute})
	require.NoError(t, err)
	assert.InDelta(t, halfSecond*2, len(decoded)-len(wav(t, 0)), float64(audio.RecordingRate/50*2), "decoded PCM bytes")
}

func TestSave_RejectsAudioThatIsNotTheNormalisedWAV(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	header, err := audio.WAVHeader(24000, 2, 1, 4)
	require.NoError(t, err)

	require.Error(t, store.Save(append(header, 1, 2, 3, 4), sourceOpenAI, "x"))
	require.Error(t, store.Save([]byte("RIFF"), sourceOpenAI, "x"))

	assert.Empty(t, names(t, dir))
}

func TestList_NewestFirst(t *testing.T) {
	store, c := openStore(t, t.TempDir())
	save(t, store, "first")
	c.advance(time.Minute)
	save(t, store, "second")

	list, _, err := store.List("", math.MaxInt)

	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "second", list[0].Transcript)
	assert.Equal(t, "first", list[1].Transcript)
	assert.Equal(t, start.Add(time.Minute), list[0].At)
	assert.Equal(t, 500*time.Millisecond, list[0].Duration)
	assert.Equal(t, sourceOpenAI, list[0].Source)
}

func TestSetKeep_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, "keep me")

	require.NoError(t, store.SetKeep(rec.ID, true))

	reopened, _ := openStore(t, dir)
	list, _, err := reopened.List("", math.MaxInt)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].Keep)
	assert.True(t, readMetadata(t, filepath.Join(dir, rec.ID+jsonSuffix)).Keep)

	require.NoError(t, reopened.SetKeep(rec.ID, false))
	assert.False(t, readMetadata(t, filepath.Join(dir, rec.ID+jsonSuffix)).Keep)
}

func TestSetKeep_UnknownIDIsNotFound(t *testing.T) {
	store, _ := openStore(t, t.TempDir())

	require.ErrorIs(t, store.SetKeep("20261125T153000Z-00000000", true), ErrNotFound)
}

func TestInvalidID_TouchesNoFile(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	planted := map[string]string{"x.opus": "audio", "x.json": `{"keep":false}`}
	for name, content := range planted {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}

	for _, id := range []string{"x", "../x", "", "20261125T153000Z-0000000", "20261125T153000Z-0000000G", "20261125T153000Z-00000000/../x"} {
		_, err := store.Audio(id)
		require.ErrorIs(t, err, ErrNotFound, "audio %q", id)
		require.ErrorIs(t, store.SetKeep(id, true), ErrNotFound, "keep %q", id)
	}

	for name, content := range planted {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		assert.Equal(t, content, string(data))
	}
}

func TestAudio_OpensTheOpusFile(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, "listen")

	file, err := store.Audio(rec.ID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, file.Close()) }()

	want, err := os.ReadFile(filepath.Join(dir, rec.ID+opusSuffix))
	require.NoError(t, err)
	got, err := io.ReadAll(file)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestCollect_DeletesExpiredUnkeptAndKeepsKept(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	old := save(t, store, "old")
	kept := save(t, store, "kept")
	require.NoError(t, store.SetKeep(kept.ID, true))
	c.advance(day)
	recent := save(t, store, "recent")

	c.advance(Retention - day + time.Second)
	require.NoError(t, store.Collect())

	assert.ElementsMatch(t, []string{kept.ID, recent.ID}, ids(t, store))
	assert.NoFileExists(t, filepath.Join(dir, old.ID+opusSuffix))
	assert.NoFileExists(t, filepath.Join(dir, old.ID+jsonSuffix))
}

func TestCollect_KeepsARecordingAtExactlyTheRetention(t *testing.T) {
	store, c := openStore(t, t.TempDir())
	save(t, store, "edge")

	c.advance(Retention)
	require.NoError(t, store.Collect())

	list, _, err := store.List("", math.MaxInt)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestOpen_CollectsOnce(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	save(t, store, "stale")

	c := &clock{now: start.Add(Retention + time.Hour)}
	reopened, err := Open(dir, c.Now, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	list, _, err := reopened.List("", math.MaxInt)
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.Empty(t, names(t, dir))
}

// recordingBytes is the size on disk of one saved recording.
func recordingBytes(t *testing.T, dir string, rec *Recording) int64 {
	t.Helper()

	var total int64
	for _, suffix := range []string{opusSuffix, jsonSuffix} {
		info, err := os.Stat(filepath.Join(dir, rec.ID+suffix))
		require.NoError(t, err)
		total += info.Size()
	}

	return total
}

func TestSave_CapEvictsOldestUnkeptFirst(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	first := save(t, store, "first")
	c.advance(time.Minute)
	kept := save(t, store, "kept")
	require.NoError(t, store.SetKeep(kept.ID, true))
	c.advance(time.Minute)
	second := save(t, store, "second")
	c.advance(time.Minute)
	third := save(t, store, "third")
	size := recordingBytes(t, dir, first)
	// Room for four recordings and a little slack: the fifth evicts one.
	store.maxBytes = 4*size + size/2

	c.advance(time.Minute)
	fifth := save(t, store, "fifth")

	assert.Equal(t, []string{fifth.ID, third.ID, second.ID, kept.ID}, ids(t, store))

	c.advance(time.Minute)
	sixth := save(t, store, "sixth")
	assert.Equal(t, []string{sixth.ID, fifth.ID, third.ID, kept.ID}, ids(t, store), "the oldest unkept goes next")
}

func TestSave_CapFullOfKeptRecordingsRefuses(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	kept := save(t, store, "kept")
	require.NoError(t, store.SetKeep(kept.ID, true))
	c.advance(time.Minute)
	unkept := save(t, store, "unkept")
	store.maxBytes = recordingBytes(t, dir, kept) + recordingBytes(t, dir, unkept)/2
	before := names(t, dir)

	c.advance(time.Minute)
	err := store.Save(wav(t, halfSecond), sourceOpenAI, "refused")

	require.ErrorIs(t, err, ErrFull)
	assert.Equal(t, before, names(t, dir), "a refused save deletes and writes nothing")
}

func TestOpen_RemovesTemporaryAndOrphanFiles(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	whole := save(t, store, "whole")
	orphans := []string{
		"20261125T120000Z-0000000a" + opusSuffix,
		"20261125T120000Z-0000000b" + jsonSuffix,
		"20261125T120000Z-0000000c" + opusSuffix + ".123" + tmpSuffix,
		"probe-1" + tmpSuffix,
	}
	for _, name := range orphans {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600))
	}
	unrelated := "notes.txt"
	require.NoError(t, os.WriteFile(filepath.Join(dir, unrelated), []byte("mine"), 0o600))

	openStore(t, dir)

	assert.Equal(t, []string{whole.ID + jsonSuffix, whole.ID + opusSuffix, unrelated}, names(t, dir))
}

func TestRun_StopsWhenContextEnds(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		store.Run(ctx, ctx)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "Run did not return after its context ended")
	}
}

func TestRun_CollectsEveryInterval(t *testing.T) {
	store, c := openStore(t, t.TempDir())
	save(t, store, "stale")
	ticks := make(chan time.Time)
	store.ticks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		store.Run(ctx, ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()

	c.advance(Retention + time.Hour)
	ticks <- c.Now()

	assert.Eventually(t, func() bool {
		list, _, err := store.List("", math.MaxInt)
		return err == nil && len(list) == 0
	}, time.Second, 10*time.Millisecond)
}
