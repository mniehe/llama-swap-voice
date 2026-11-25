package recordings

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
)

const (
	// opusFrame is one 20 ms Opus frame at the recording rate.
	opusFrame        = audio.RecordingRate / 50
	minCorrelation   = 0.9
	chirpAmplitude   = 0.4
	chirpStartHz     = 200
	chirpEndHz       = 2000
	chirpSeconds     = 2
	fullScale        = math.MaxInt16
	collidingID      = "20261125T153000Z-0000000a"
	nextID           = "20261125T153000Z-0000000b"
	plantedOrphanID  = "20261125T120000Z-0000000d"
	plantedCorruptID = "20261125T120000Z-0000000e"
	// maxTranscriptBytes is the upstream client's bound on a transcript
	// (internal/upstream), written out so a lowered metadata bound fails here.
	maxTranscriptBytes = 1 << 20
	// jsonEscapedByte is a character JSON escapes to six bytes, the worst
	// case for the metadata bound.
	jsonEscapedByte = "<"
)

// logBuffer collects JSON log lines from a store.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *logBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()

	l.mu.Lock()
	defer l.mu.Unlock()

	var lines []map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(l.buf.Bytes()), []byte("\n")) {
		if len(raw) == 0 {
			continue
		}
		var line map[string]any
		require.NoError(t, json.Unmarshal(raw, &line))
		lines = append(lines, line)
	}

	return lines
}

func (l *logBuffer) withMessage(t *testing.T, msg string) []map[string]any {
	t.Helper()

	var found []map[string]any
	for _, line := range l.lines(t) {
		if line["msg"] == msg {
			found = append(found, line)
		}
	}

	return found
}

func openLogged(t *testing.T, dir string, now func() time.Time) (*Store, *logBuffer) {
	t.Helper()

	logs := &logBuffer{}
	store, err := Open(dir, now, slog.New(slog.NewJSONHandler(logs, nil)))
	require.NoError(t, err)

	return store, logs
}

// chirpSamples sweeps from chirpStartHz to chirpEndHz, so no shifted copy of
// it correlates with itself the way a pure tone does.
func chirpSamples(seconds int) []int16 {
	count := audio.RecordingRate * seconds
	samples := make([]int16, count)
	duration := float64(seconds)
	for i := range samples {
		at := float64(i) / audio.RecordingRate
		phase := 2 * math.Pi * (chirpStartHz*at + (chirpEndHz-chirpStartHz)*at*at/(2*duration))
		samples[i] = int16(chirpAmplitude * fullScale * math.Sin(phase))
	}

	return samples
}

func wavOf(t *testing.T, samples []int16) []byte {
	t.Helper()

	pcm, err := binary.Append(nil, binary.LittleEndian, samples)
	require.NoError(t, err)
	header, err := audio.WAVHeader(audio.RecordingRate, pcmBytes, 1, len(pcm))
	require.NoError(t, err)

	return append(header, pcm...)
}

func samplesOf(t *testing.T, wavData []byte) []int16 {
	t.Helper()

	headerBytes := len(wav(t, 0))
	require.GreaterOrEqual(t, len(wavData), headerBytes)
	pcm := wavData[headerBytes:]
	samples := make([]int16, len(pcm)/pcmBytes)
	_, err := binary.Decode(pcm, binary.LittleEndian, samples)
	require.NoError(t, err)

	return samples
}

// bestCorrelation is the highest normalised cross-correlation of got against
// want over shifts of up to maxLag samples either way.
func bestCorrelation(want, got []int16, maxLag int) float64 {
	best := 0.0
	for lag := -maxLag; lag <= maxLag; lag++ {
		var dot, wantEnergy, gotEnergy float64
		for i, w := range want {
			j := i + lag
			if j < 0 || j >= len(got) {
				continue
			}
			wf, gf := float64(w), float64(got[j])
			dot += wf * gf
			wantEnergy += wf * wf
			gotEnergy += gf * gf
		}
		if wantEnergy == 0 || gotEnergy == 0 {
			continue
		}
		best = max(best, dot/math.Sqrt(wantEnergy*gotEnergy))
	}

	return best
}

func TestSave_StoredAudioMatchesTheInput(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	input := chirpSamples(chirpSeconds)

	require.NoError(t, store.Save(wavOf(t, input), sourceOpenAI, "chirp"))

	found := names(t, dir)
	require.Len(t, found, 2)
	opus, err := os.ReadFile(filepath.Join(dir, strings.TrimSuffix(found[0], jsonSuffix)+opusSuffix))
	require.NoError(t, err)
	decoded, err := audio.NormaliseUpload(t.Context(), bytes.NewReader(opus), audio.Limits{MaxDuration: time.Minute})
	require.NoError(t, err)
	got := samplesOf(t, decoded)

	assert.InDelta(t, len(input), len(got), opusFrame, "decoded length within one Opus frame")
	correlation := bestCorrelation(input, got, opusFrame)
	t.Logf("best normalised cross-correlation %.4f", correlation)
	assert.GreaterOrEqual(t, correlation, minCorrelation)
}

func TestSave_JSONWriteFailureRemovesTheOpus(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	errDiskFull := errors.New("disk full")
	write := store.write
	store.write = func(name string, data []byte) error {
		if strings.HasSuffix(name, jsonSuffix) {
			return errDiskFull
		}
		return write(name, data)
	}

	err := store.Save(wav(t, halfSecond), sourceOpenAI, "lost")

	require.ErrorIs(t, err, errDiskFull)
	assert.Empty(t, names(t, dir), "no audio without its metadata")
	store.mu.Lock()
	assert.Empty(t, store.order, "a failed save is not indexed")
	assert.Empty(t, store.byID, "a failed save is not indexed")
	store.mu.Unlock()

	store.write = write
	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, "saved"))
	assert.Equal(t, []string{"saved"}, transcripts(t, store))
}

func TestSave_RegeneratesACollidingID(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	issued := []string{collidingID, collidingID, nextID}
	store.newID = func(time.Time) (string, error) {
		id := issued[0]
		issued = issued[1:]
		return id, nil
	}

	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, "first"))
	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, "second"))

	assert.Equal(t, []string{
		collidingID + jsonSuffix, collidingID + opusSuffix,
		nextID + jsonSuffix, nextID + opusSuffix,
	}, names(t, dir))
	assert.Equal(t, "first", readMetadata(t, filepath.Join(dir, collidingID+jsonSuffix)).Transcript, "the first pair is never replaced")
	assert.Equal(t, "second", readMetadata(t, filepath.Join(dir, nextID+jsonSuffix)).Transcript)
}

func TestSave_KeepsTheFullPrecisionTime(t *testing.T) {
	dir := t.TempDir()
	at := start.Add(123456789 * time.Nanosecond)
	store, err := Open(dir, func() time.Time { return at }, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, "precise"))

	found := names(t, dir)
	require.Len(t, found, 2)
	assert.Equal(t, "2026-11-25T15:30:00.123456789Z", readMetadata(t, filepath.Join(dir, found[0])).At)
}

func TestOpen_KeepsTmpFilesItDidNotCreate(t *testing.T) {
	dir := t.TempDir()
	foreign := []string{"foo.tmp", "probe.tmp", "probe-x.tmp", "20261125T120000Z-0000000a.tmp", "20261125T120000Z-0000000a.wav.1.tmp", "x.opus.1.tmp"}
	for _, name := range foreign {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("mine"), 0o600))
	}

	openStore(t, dir)

	for _, name := range foreign {
		assert.FileExists(t, filepath.Join(dir, name))
	}
}

// plantPair writes a recording's two files with the given metadata and
// returns their total size.
func plantPair(t *testing.T, dir, id string, meta []byte) int64 {
	t.Helper()

	audioBytes := []byte("audio")
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+opusSuffix), audioBytes, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, id+jsonSuffix), meta, 0o600))

	return int64(len(audioBytes) + len(meta))
}

// paddedMetadata is valid metadata padded with trailing spaces to exactly
// size bytes.
func paddedMetadata(t *testing.T, size int) []byte {
	t.Helper()

	meta := []byte(`{"at":"2026-11-25T12:00:00Z","durationMs":500,"source":"openai","transcript":"padded","keep":true}`)
	require.LessOrEqual(t, len(meta), size)

	return append(meta, bytes.Repeat([]byte(" "), size-len(meta))...)
}

func requireLeftOnDisk(t *testing.T, logs *logBuffer, dir, id string) {
	t.Helper()

	assert.FileExists(t, filepath.Join(dir, id+opusSuffix))
	assert.FileExists(t, filepath.Join(dir, id+jsonSuffix))
	warnings := logs.withMessage(t, "recording left on disk: its metadata is unusable")
	require.Len(t, warnings, 1)
	assert.Equal(t, "WARN", warnings[0]["level"])
	assert.Equal(t, id, warnings[0]["id"])
	assert.NotEmpty(t, warnings[0]["error"])
}

func TestOpen_KeepsARecordingWithAMaximalTranscript(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	transcript := strings.Repeat(jsonEscapedByte, maxTranscriptBytes)
	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, transcript))

	reopened, logs := openLogged(t, dir, (&clock{now: start}).Now)

	list, total, err := reopened.List("", 1)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, list, 1)
	assert.Equal(t, transcript, list[0].Transcript)
	assert.Empty(t, logs.withMessage(t, "recording left on disk: its metadata is unusable"))
}

func TestOpen_LoadsMetadataOfExactlyTheLimit(t *testing.T) {
	dir := t.TempDir()
	plantPair(t, dir, plantedCorruptID, paddedMetadata(t, maxMetadataBytes))

	store, logs := openLogged(t, dir, (&clock{now: start}).Now)

	assert.Equal(t, []string{plantedCorruptID}, ids(t, store))
	assert.Empty(t, logs.withMessage(t, "recording left on disk: its metadata is unusable"))
}

func TestOpen_LeavesMetadataOneByteOverOnDisk(t *testing.T) {
	dir := t.TempDir()
	plantPair(t, dir, plantedCorruptID, paddedMetadata(t, maxMetadataBytes+1))

	store, logs := openLogged(t, dir, (&clock{now: start}).Now)

	assert.Empty(t, ids(t, store))
	requireLeftOnDisk(t, logs, dir, plantedCorruptID)
}

func TestOpen_LeavesUndecodableMetadataOnDiskAndCountsIt(t *testing.T) {
	dir := t.TempDir()
	skippedBytes := plantPair(t, dir, plantedCorruptID, []byte("{not json"))

	store, logs := openLogged(t, dir, (&clock{now: start}).Now)

	assert.Empty(t, ids(t, store))
	requireLeftOnDisk(t, logs, dir, plantedCorruptID)
	require.NoError(t, store.Collect())
	requireLeftOnDisk(t, logs, dir, plantedCorruptID)
	_, err := store.Audio(plantedCorruptID)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, store.SetKeep(plantedCorruptID, true), ErrNotFound)

	first := save(t, store, "first")
	size := recordingBytes(t, dir, first)
	// Room for the skipped pair and one recording: a second evicts the first
	// only because the skipped pair counts.
	store.maxBytes = skippedBytes + 2*size - 1
	second := save(t, store, "second")

	assert.Equal(t, []string{second.ID}, ids(t, store))
	requireLeftOnDisk(t, logs, dir, plantedCorruptID)
}

func TestOpen_UnopenableMetadataFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads files whatever their mode")
	}
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	rec := save(t, store, "locked")
	metaPath := filepath.Join(dir, rec.ID+jsonSuffix)
	require.NoError(t, os.Chmod(metaPath, 0))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(metaPath, 0o600)) })

	_, err := Open(dir, (&clock{now: start}).Now, slog.New(slog.DiscardHandler))

	require.ErrorIs(t, err, os.ErrPermission)
	assert.FileExists(t, filepath.Join(dir, rec.ID+opusSuffix))
	assert.FileExists(t, metaPath)
}

// blockAudio replaces the audio of rec with a directory that is not empty,
// which os.Remove refuses to delete.
func blockAudio(t *testing.T, dir string, rec *Recording) {
	t.Helper()

	audioPath := filepath.Join(dir, rec.ID+opusSuffix)
	require.NoError(t, os.Remove(audioPath))
	require.NoError(t, os.Mkdir(audioPath, privateDir))
	require.NoError(t, os.WriteFile(filepath.Join(audioPath, "blocker"), []byte("x"), 0o600))
}

func TestCollect_UndeletableAudioLeavesTheIndex(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	stuck := save(t, store, "stuck")
	c.advance(time.Minute)
	other := save(t, store, "other")
	blockAudio(t, dir, stuck)
	c.advance(Retention + time.Hour)

	err := store.Collect()

	require.Error(t, err)
	assert.Contains(t, err.Error(), stuck.ID)
	list, total, listErr := store.List("", 10)
	require.NoError(t, listErr, "the page still lists")
	assert.Empty(t, list)
	assert.Zero(t, total)
	assert.NoFileExists(t, filepath.Join(dir, stuck.ID+jsonSuffix))
	assert.NoFileExists(t, filepath.Join(dir, other.ID+jsonSuffix), "the other expired recording is still deleted")
	assert.NoFileExists(t, filepath.Join(dir, other.ID+opusSuffix))
}

func TestSave_UndeletableAudioStillEvictsTheRestAndFails(t *testing.T) {
	dir := t.TempDir()
	store, logs := openLogged(t, dir, steppingNow())
	saved := make([]*Recording, 0, 3)
	for _, transcript := range []string{"rec0", "rec1", "rec2"} {
		saved = append(saved, save(t, store, transcript))
	}
	size := recordingBytes(t, dir, saved[2])
	require.NoError(t, store.SetKeep(saved[2].ID, true))
	blockAudio(t, dir, saved[0])
	// Room for two recordings: the next save evicts both unkept ones.
	store.maxBytes = 2*size + 1

	err := store.Save(wav(t, halfSecond), sourceOpenAI, "rec3")

	require.Error(t, err)
	assert.Equal(t, []string{saved[2].ID}, ids(t, store))
	assert.NoFileExists(t, filepath.Join(dir, saved[1].ID+opusSuffix), "the eviction after the failed one still runs")
	store.mu.Lock()
	assert.Contains(t, store.stray, saved[0].ID+opusSuffix, "the undeleted audio counts against the cap")
	store.mu.Unlock()
	warnings := logs.withMessage(t, "could not evict recording")
	require.Len(t, warnings, 1)
	assert.Equal(t, saved[0].ID, warnings[0]["id"])
}

// sameSizeRecordings saves count recordings a minute apart whose two files
// are the same size, and returns that size.
func sameSizeRecordings(t *testing.T, store *Store, c *clock, dir string, count int) (saved []*Recording, size int64) {
	t.Helper()

	saved = make([]*Recording, 0, count)
	for i := range count {
		if i > 0 {
			c.advance(time.Minute)
		}
		saved = append(saved, save(t, store, "rec"))
	}

	size = recordingBytes(t, dir, saved[0])
	for _, rec := range saved {
		require.Equal(t, size, recordingBytes(t, dir, rec))
	}

	return saved, size
}

func TestSave_CapAtExactlyTheLimitEvictsNothing(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	saved, size := sameSizeRecordings(t, store, c, dir, 3)
	store.maxBytes = 4 * size

	c.advance(time.Minute)
	fourth := save(t, store, "rec")

	assert.Equal(t, []string{fourth.ID, saved[2].ID, saved[1].ID, saved[0].ID}, ids(t, store))
}

func TestSave_CapOneByteOverEvictsExactlyOne(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	saved, size := sameSizeRecordings(t, store, c, dir, 3)
	store.maxBytes = 4*size - 1

	c.advance(time.Minute)
	fourth := save(t, store, "rec")

	assert.Equal(t, []string{fourth.ID, saved[2].ID, saved[1].ID}, ids(t, store))
}

func TestSave_KeptAtExactlyTheLimitStillSaves(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	saved, _ := sameSizeRecordings(t, store, c, dir, 1)
	require.NoError(t, store.SetKeep(saved[0].ID, true))
	size := recordingBytes(t, dir, saved[0])
	store.maxBytes = 2*size + 1 // the new recording's metadata says false, one byte longer than true

	c.advance(time.Minute)
	second := save(t, store, "rec")

	assert.Equal(t, []string{second.ID, saved[0].ID}, ids(t, store))
}

func TestSave_KeptOneByteOverRefuses(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	saved, _ := sameSizeRecordings(t, store, c, dir, 1)
	require.NoError(t, store.SetKeep(saved[0].ID, true))
	size := recordingBytes(t, dir, saved[0])
	store.maxBytes = 2 * size
	before := names(t, dir)

	c.advance(time.Minute)
	err := store.Save(wav(t, halfSecond), sourceOpenAI, "rec")

	require.ErrorIs(t, err, ErrFull)
	assert.Equal(t, before, names(t, dir))
}

func listIDs(t *testing.T, store *Store, before string, limit int) (found []string, total int) {
	t.Helper()

	list, total, err := store.List(before, limit)
	require.NoError(t, err)
	for i := range list {
		found = append(found, list[i].ID)
	}

	return found, total
}

func TestList_PagesNewestFirstFromACursor(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	saved, _ := sameSizeRecordings(t, store, c, dir, 5)

	page, total := listIDs(t, store, "", 2)
	assert.Equal(t, []string{saved[4].ID, saved[3].ID}, page)
	assert.Equal(t, 5, total)

	page, total = listIDs(t, store, saved[3].ID, 2)
	assert.Equal(t, []string{saved[2].ID, saved[1].ID}, page)
	assert.Equal(t, 5, total)

	page, _ = listIDs(t, store, saved[1].ID, 2)
	assert.Equal(t, []string{saved[0].ID}, page)

	page, _ = listIDs(t, store, saved[0].ID, 2)
	assert.Empty(t, page)
}

func TestList_ReadsTheTranscriptOfEachReturnedRow(t *testing.T) {
	store, c := openStore(t, t.TempDir())
	save(t, store, "older")
	c.advance(time.Minute)
	save(t, store, "newer")

	list, total, err := store.List("", 1)

	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, 2, total)
	assert.Equal(t, "newer", list[0].Transcript)
}

func TestList_CursorOfADeletedRecordingStillPages(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	saved, _ := sameSizeRecordings(t, store, c, dir, 3)
	store.mu.Lock()
	require.NoError(t, store.remove(saved[1].ID))
	store.mu.Unlock()

	page, total := listIDs(t, store, saved[1].ID, 10)

	assert.Equal(t, []string{saved[0].ID}, page)
	assert.Equal(t, 2, total)
}

func TestList_CursorOfADeletedRecordingKeepsEarlierOnesInItsSecond(t *testing.T) {
	dir := t.TempDir()
	c := &clock{now: start.Add(100 * time.Millisecond)}
	store, err := Open(dir, c.Now, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	earlier := save(t, store, "earlier")
	require.NoError(t, store.SetKeep(earlier.ID, true))
	c.advance(400 * time.Millisecond)
	cursor := save(t, store, "cursor")
	c.advance(400 * time.Millisecond)
	later := save(t, store, "later")
	page, _ := listIDs(t, store, "", 2)
	require.Equal(t, []string{later.ID, cursor.ID}, page)
	store.mu.Lock()
	require.NoError(t, store.remove(cursor.ID))
	store.mu.Unlock()

	page, total := listIDs(t, store, cursor.ID, 10)

	assert.Contains(t, page, earlier.ID, "a recording saved earlier in the cursor's second is on the next page")
	assert.Equal(t, 2, total)
}

func TestList_OrdersBySaveTimeBeforeID(t *testing.T) {
	dir := t.TempDir()
	c := &clock{now: start.Add(100 * time.Millisecond)}
	store, err := Open(dir, c.Now, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	issued := []string{"20261125T153000Z-ffffffff", "20261125T153000Z-00000000"}
	store.newID = func(time.Time) (string, error) {
		id := issued[0]
		issued = issued[1:]
		return id, nil
	}

	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, "first"))
	c.advance(500 * time.Millisecond)
	require.NoError(t, store.Save(wav(t, halfSecond), sourceOpenAI, "second"))

	page, _ := listIDs(t, store, "", 10)
	assert.Equal(t, []string{"20261125T153000Z-00000000", "20261125T153000Z-ffffffff"}, page, "the later save first, though its id sorts lower")

	reopened, err := Open(dir, c.Now, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	page, _ = listIDs(t, reopened, "", 10)
	assert.Equal(t, []string{"20261125T153000Z-00000000", "20261125T153000Z-ffffffff"}, page, "the order survives a reopen")
}

func TestCollect_DeletesOrphansAndStoreTempFiles(t *testing.T) {
	dir := t.TempDir()
	store, _ := openStore(t, dir)
	whole := save(t, store, "whole")
	leftovers := []string{
		plantedOrphanID + opusSuffix,
		"20261125T120000Z-0000000f" + jsonSuffix,
		whole.ID + opusSuffix + ".42" + tmpSuffix,
		"probe-7" + tmpSuffix,
	}
	for _, name := range leftovers {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "foo.tmp"), []byte("mine"), 0o600))

	require.NoError(t, store.Collect())

	assert.Equal(t, []string{whole.ID + jsonSuffix, whole.ID + opusSuffix, "foo.tmp"}, names(t, dir))
}
