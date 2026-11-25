package recordings

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	waitFor   = 2 * time.Second
	pollEvery = 5 * time.Millisecond
)

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
		store.Record(wavData, source, transcript)
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
	store.maxQueued = int64(2 * len(one))

	recordWithin(t, store, one, sourceOpenAI, "first")
	recordWithin(t, store, wav(t, halfSecond), sourceOpenAI, "second")
	recordWithin(t, store, wav(t, halfSecond), sourceWyoming, "third")

	dropped := logs.withMessage(t, "recording dropped: the save queue is full")
	require.Len(t, dropped, 1)
	assert.Equal(t, "WARN", dropped[0]["level"])
	assert.Equal(t, sourceWyoming, dropped[0]["source"])
	assert.InDelta(t, len(one), dropped[0]["bytes"], 0)
	runStore(t, store)
	waitForTranscripts(t, store, "second", "first")
}

func TestRecord_QueueAtExactlyTheLimitTakesTheRecording(t *testing.T) {
	store, logs := openLogged(t, t.TempDir(), steppingNow())
	one := wav(t, halfSecond)
	store.maxQueued = int64(len(one))

	recordWithin(t, store, one, sourceOpenAI, "fits")

	assert.Empty(t, logs.withMessage(t, "recording dropped: the save queue is full"))
	assert.Equal(t, 1, queuedCount(store))
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

func TestStore_ConcurrentRecordKeepAndCollectStayUnderTheCap(t *testing.T) {
	dir := t.TempDir()
	store, _ := openLogged(t, dir, steppingNow())
	seed := save(t, store, "seed")
	size := recordingBytes(t, dir, seed)
	store.maxBytes = 5 * size
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
				store.Record(wav(t, halfSecond), sourceOpenAI, "concurrent")
				if (w+i)%3 == 0 {
					list, _, err := store.List("", 1)
					if err == nil && len(list) == 1 {
						assert.NoError(t, store.SetKeep(list[0].ID, true))
					}
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
