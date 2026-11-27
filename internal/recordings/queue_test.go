package recordings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
)

const (
	waitFor   = 2 * time.Second
	pollEvery = 5 * time.Millisecond
	// itemOverhead is what the queue charges each recording besides its
	// bytes, written out so a changed constant fails here.
	itemOverhead = 512
	// longestUploadSeconds is the transcription limit, 20 minutes
	// (internal/pipeline).
	longestUploadSeconds = 20 * 60
	droppedMessage       = "recording dropped: the save queue is full"
	testRequestID        = "req-42"
)

// chargeOf is what the queue charges for one recording: the WAV's whole
// buffer, the transcript and the overhead.
func chargeOf(wavData []byte, transcript string) int64 {
	return int64(cap(wavData)+len(transcript)) + itemOverhead
}

// steppingNow moves a millisecond on every reading, so recordings saved one
// after the other keep their order.
func steppingNow() func() time.Time {
	var steps atomic.Int64
	return func() time.Time {
		return start.Add(time.Duration(steps.Add(1)) * time.Millisecond)
	}
}

// runStore runs the store's background work until the test ends.
func runStore(t *testing.T, store *Store) {
	t.Helper()

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
}

// recordWithin calls Record and fails the test unless it returns at once.
func recordWithin(t *testing.T, store *Store, wavData []byte, source, transcript string) {
	t.Helper()

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		store.Record(wavData, source, transcript, testRequestID)
	}()

	select {
	case <-returned:
	case <-time.After(waitFor):
		require.FailNow(t, "Record waited on the save")
	}
}

// holdStore takes the store's mutex, as a slow disk under a save would, and
// releases it when the returned function is called or the test ends.
func holdStore(t *testing.T, store *Store) (release func()) {
	t.Helper()

	store.mu.Lock()
	var once sync.Once
	release = func() { once.Do(store.mu.Unlock) }
	t.Cleanup(release)

	return release
}

func queuedCount(store *Store) int {
	store.queueMu.Lock()
	defer store.queueMu.Unlock()

	return len(store.queue)
}

func transcripts(t *testing.T, store *Store) []string {
	t.Helper()

	list, _, err := store.List("", maxIDAttempts*maxIDAttempts)
	require.NoError(t, err)
	found := make([]string, 0, len(list))
	for i := range list {
		found = append(found, list[i].Transcript)
	}

	return found
}

func waitForTranscripts(t *testing.T, store *Store, want ...string) {
	t.Helper()

	require.Eventually(t, func() bool {
		return assert.ObjectsAreEqual(want, transcripts(t, store))
	}, waitFor, pollEvery, "want %v", want)
}

func TestRecord_ReturnsWhileASaveIsStuck(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	release := holdStore(t, store)

	recordWithin(t, store, wav(t, halfSecond), sourceOpenAI, "queued")

	release()
	runStore(t, store)
	waitForTranscripts(t, store, "queued")
}

func TestRecord_DropsWhenTheQueueIsFull(t *testing.T) {
	store, logs := openLogged(t, t.TempDir(), steppingNow())
	one := wav(t, halfSecond)
	store.maxQueued = chargeOf(one, "first") + chargeOf(one, "second")

	recordWithin(t, store, one, sourceOpenAI, "first")
	recordWithin(t, store, wav(t, halfSecond), sourceOpenAI, "second")
	recordWithin(t, store, wav(t, halfSecond), sourceWyoming, "third")

	dropped := logs.withMessage(t, droppedMessage)
	require.Len(t, dropped, 1)
	assert.Equal(t, "WARN", dropped[0]["level"])
	assert.Equal(t, sourceWyoming, dropped[0]["source"])
	assert.InDelta(t, chargeOf(one, "third"), dropped[0]["bytes"], 0)
	assert.Equal(t, testRequestID, dropped[0]["request_id"])
	runStore(t, store)
	waitForTranscripts(t, store, "second", "first")
}

func TestRecord_QueueAtExactlyTheLimitTakesTheRecording(t *testing.T) {
	store, logs := openLogged(t, t.TempDir(), steppingNow())
	one := wav(t, halfSecond)
	store.maxQueued = chargeOf(one, "fits")

	recordWithin(t, store, one, sourceOpenAI, "fits")

	assert.Empty(t, logs.withMessage(t, droppedMessage))
	assert.Equal(t, 1, queuedCount(store))
}

func TestRecord_ChargesTheTranscriptAndOverhead(t *testing.T) {
	tiny := wav(t, 1)
	require.Len(t, tiny, 46)
	transcript := strings.Repeat("t", maxTranscriptBytes)
	charge := chargeOf(tiny, transcript)

	atLimit, atLimitLogs := openLogged(t, t.TempDir(), steppingNow())
	atLimit.maxQueued = charge
	recordWithin(t, atLimit, tiny, sourceOpenAI, transcript)
	assert.Equal(t, 1, queuedCount(atLimit))
	assert.Empty(t, atLimitLogs.withMessage(t, droppedMessage))

	oneUnder, oneUnderLogs := openLogged(t, t.TempDir(), steppingNow())
	oneUnder.maxQueued = charge - 1
	recordWithin(t, oneUnder, tiny, sourceOpenAI, transcript)
	assert.Zero(t, queuedCount(oneUnder))
	dropped := oneUnderLogs.withMessage(t, droppedMessage)
	require.Len(t, dropped, 1)
	assert.InDelta(t, charge, dropped[0]["bytes"], 0)
}

func TestRecord_AfterTheWorkerSavesTheQueueTakesAnotherAtTheLimit(t *testing.T) {
	store, logs := openLogged(t, t.TempDir(), steppingNow())
	first := wav(t, halfSecond)
	store.maxQueued = chargeOf(first, "first")
	store.Record(first, sourceOpenAI, "first", testRequestID)
	require.Equal(t, 1, queuedCount(store))

	store.saveQueued(t.Context(), nil)

	second := wav(t, halfSecond)
	require.Equal(t, chargeOf(first, "first"), chargeOf(second, "again"), "both recordings charge the same")
	store.Record(second, sourceOpenAI, "again", testRequestID)
	assert.Empty(t, logs.withMessage(t, droppedMessage), "the save released the whole charge of the first")
	assert.Equal(t, 1, queuedCount(store))
}

func TestRecord_ChargesTheWAVBufferNotItsLength(t *testing.T) {
	short := wav(t, halfSecond)
	spare := make([]byte, len(short), len(short)+4096)
	copy(spare, short)
	charge := int64(cap(spare)+len("spare")) + itemOverhead

	atLimit, atLimitLogs := openLogged(t, t.TempDir(), steppingNow())
	atLimit.maxQueued = charge
	atLimit.Record(spare, sourceOpenAI, "spare", testRequestID)
	assert.Equal(t, 1, queuedCount(atLimit))
	assert.Empty(t, atLimitLogs.withMessage(t, droppedMessage))

	oneUnder, oneUnderLogs := openLogged(t, t.TempDir(), steppingNow())
	oneUnder.maxQueued = charge - 1
	oneUnder.Record(spare, sourceOpenAI, "spare", testRequestID)
	assert.Zero(t, queuedCount(oneUnder))
	dropped := oneUnderLogs.withMessage(t, droppedMessage)
	require.Len(t, dropped, 1)
	assert.InDelta(t, charge, dropped[0]["bytes"], 0, "the spare capacity is charged")
}

func TestRecord_KeepsACopyOfTheTranscript(t *testing.T) {
	store, _ := openStore(t, t.TempDir())
	upstreamText := strings.Repeat("t", maxTranscriptBytes)
	transcript := upstreamText[:len("turn on the lights")]

	store.Record(wav(t, halfSecond), sourceOpenAI, transcript, testRequestID)

	store.queueMu.Lock()
	defer store.queueMu.Unlock()
	require.Len(t, store.queue, 1)
	queuedText := store.queue[0].transcript
	assert.Equal(t, transcript, queuedText)
	assert.NotSame(t, unsafe.StringData(upstreamText), unsafe.StringData(queuedText), "the queue does not hold the upstream's whole reply")
	assert.Equal(t, int64(cap(store.queue[0].wav)+len(transcript))+itemOverhead, store.queuedBytes)
}

func TestRecord_DefaultQueueHoldsTheLongestUpload(t *testing.T) {
	store, logs := openLogged(t, t.TempDir(), steppingNow())
	require.Equal(t, int64(maxQueuedBytes), store.maxQueued)
	pcmLen := longestUploadSeconds * audio.RecordingRate * pcmBytes
	header, err := audio.WAVHeader(audio.RecordingRate, pcmBytes, 1, pcmLen)
	require.NoError(t, err)
	longest := slices.Concat(header, make([]byte, pcmLen))
	require.Len(t, longest, 38_400_044)

	recordWithin(t, store, longest, sourceOpenAI, strings.Repeat("t", maxTranscriptBytes))

	assert.Equal(t, 1, queuedCount(store))
	assert.Empty(t, logs.withMessage(t, droppedMessage))
}

func TestRecord_TakesTheTimeWhenCalled(t *testing.T) {
	store, c := openStore(t, t.TempDir())
	release := holdStore(t, store)
	recorded := c.Now()

	recordWithin(t, store, wav(t, halfSecond), sourceOpenAI, "queued")
	c.advance(time.Hour)
	release()
	runStore(t, store)

	waitForTranscripts(t, store, "queued")
	list, _, err := store.List("", 1)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, recorded, list[0].At, "the time Record was called, not when the worker saved")
	assert.True(t, strings.HasPrefix(list[0].ID, recorded.Format(idTime)), "id %s", list[0].ID)
}

func TestRun_CollectsBetweenSaves(t *testing.T) {
	dir := t.TempDir()
	store, c := openStore(t, dir)
	stale := save(t, store, "stale")
	c.advance(Retention + time.Hour)
	ticks := make(chan time.Time, 1)
	store.ticks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	// write runs on the worker under the store's mutex, which the List in
	// waitForTranscripts takes after it, so these need no lock of their own.
	write := store.write
	audioWrites, staleAtLastSave := 0, false
	store.write = func(name string, data []byte) error {
		if strings.HasSuffix(name, opusSuffix) {
			audioWrites++
			switch audioWrites {
			case 1:
				ticks <- c.Now()
			case 3:
				_, err := os.Stat(filepath.Join(dir, stale.ID+jsonSuffix))
				staleAtLastSave = err == nil
			}
		}
		return write(name, data)
	}
	for _, transcript := range []string{"a", "b", "c"} {
		c.advance(time.Second)
		recordWithin(t, store, wav(t, halfSecond), sourceOpenAI, transcript)
	}

	runStore(t, store)

	waitForTranscripts(t, store, "c", "b", "a")
	assert.False(t, staleAtLastSave, "the collection due after the first save ran before the third")
}

func TestRun_SavesInOrder(t *testing.T) {
	store, _ := openLogged(t, t.TempDir(), steppingNow())
	release := holdStore(t, store)
	for _, transcript := range []string{"a", "b", "c"} {
		recordWithin(t, store, wav(t, halfSecond), sourceOpenAI, transcript)
	}

	release()
	runStore(t, store)

	waitForTranscripts(t, store, "c", "b", "a")
}

func TestRun_DrainsTheQueueAfterItsContextEnds(t *testing.T) {
	store, _ := openLogged(t, t.TempDir(), steppingNow())
	release := holdStore(t, store)
	for _, transcript := range []string{"a", "b", "c"} {
		recordWithin(t, store, wav(t, halfSecond), sourceOpenAI, transcript)
	}
	release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	store.Run(ctx, t.Context())

	assert.Equal(t, []string{"c", "b", "a"}, transcripts(t, store), "Run returns only once the queue is saved")
}

func TestRun_DropsWhatIsLeftWhenTheDrainEnds(t *testing.T) {
	store, logs := openLogged(t, t.TempDir(), steppingNow())
	release := holdStore(t, store)
	for _, transcript := range []string{"a", "b", "c"} {
		recordWithin(t, store, wav(t, halfSecond), sourceOpenAI, transcript)
	}
	ctx, cancel := context.WithCancel(t.Context())
	drain, endDrain := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Run(ctx, drain)
	}()
	require.Eventually(t, func() bool { return queuedCount(store) == 2 }, waitFor, pollEvery, "the worker takes the first recording")

	cancel()
	endDrain()
	release()

	select {
	case <-done:
	case <-time.After(waitFor):
		require.FailNow(t, "Run did not return after its drain ended")
	}
	assert.Equal(t, []string{"a"}, transcripts(t, store), "the save under way finishes")
	dropped := logs.withMessage(t, "recordings dropped at shutdown")
	require.Len(t, dropped, 1)
	assert.Equal(t, "WARN", dropped[0]["level"])
	assert.InDelta(t, 2, dropped[0]["count"], 0)
	assert.Zero(t, queuedCount(store))
}

func TestRun_SaveFailureIsLoggedAtWarn(t *testing.T) {
	dir := t.TempDir()
	store, logs := openLogged(t, dir, steppingNow())
	runStore(t, store)

	recordWithin(t, store, []byte("RIFF"), sourceWyoming, "lost")

	require.Eventually(t, func() bool {
		return len(logs.withMessage(t, "could not save recording")) == 1
	}, waitFor, pollEvery)
	warning := logs.withMessage(t, "could not save recording")[0]
	assert.Equal(t, "WARN", warning["level"])
	assert.Equal(t, sourceWyoming, warning["source"])
	assert.Equal(t, testRequestID, warning["request_id"])
	assert.Contains(t, warning["error"], "shorter than a WAV header")
	assert.Empty(t, names(t, dir))
}

// diskBytes sums the sizes of every file in dir.
func diskBytes(t *testing.T, dir string) int64 {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var total int64
	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		total += info.Size()
	}

	return total
}

// keepNewest keeps the newest recording. A save can evict it between the
// List and the SetKeep, so not found is an answer too.
func keepNewest(t *testing.T, store *Store) {
	t.Helper()

	list, _, err := store.List("", 1)
	if err != nil || len(list) != 1 {
		return
	}

	if keepErr := store.SetKeep(list[0].ID, true); !errors.Is(keepErr, ErrNotFound) {
		assert.NoError(t, keepErr)
	}
}

func TestStore_ConcurrentRecordKeepAndCollectStayUnderTheCap(t *testing.T) {
	dir := t.TempDir()
	store, _ := openLogged(t, dir, steppingNow())
	seed := save(t, store, "seed")
	size := recordingBytes(t, dir, seed)
	store.maxBytes = 5 * size
	// The kept seed is never evicted or expired, so keeping it again must
	// succeed however the other calls interleave.
	require.NoError(t, store.SetKeep(seed.ID, true))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.Run(ctx, ctx)
	}()

	const writers, rounds = 4, 15
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range rounds {
				store.Record(wav(t, halfSecond), sourceOpenAI, "concurrent", testRequestID)
				if (w+i)%3 == 0 {
					assert.NoError(t, store.SetKeep(seed.ID, true))
					keepNewest(t, store)
				}
				if i%5 == 0 {
					assert.NoError(t, store.Collect())
				}
			}
		})
	}
	wg.Wait()
	require.Eventually(t, func() bool { return queuedCount(store) == 0 }, 10*waitFor, pollEvery)
	cancel()
	<-done

	assert.LessOrEqual(t, diskBytes(t, dir), store.maxBytes)
	store.mu.Lock()
	var indexedBytes int64
	for _, rec := range store.order {
		indexedBytes += rec.bytes()
	}
	store.mu.Unlock()
	assert.Equal(t, diskBytes(t, dir), indexedBytes, "the index counts what is on disk")
}
