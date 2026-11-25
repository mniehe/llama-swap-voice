package pipeline

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/recordings"
)

const (
	privateDir  = 0o700
	readOnlyDir = 0o500
)

type savedRecording struct {
	wav        []byte
	source     string
	transcript string
}

// fakeRecorder records each recording it is handed.
type fakeRecorder struct {
	mu    sync.Mutex
	saved []savedRecording
}

func (f *fakeRecorder) Record(wav []byte, source, transcript string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.saved = append(f.saved, savedRecording{wav: wav, source: source, transcript: transcript})
}

func (f *fakeRecorder) all() []savedRecording {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]savedRecording(nil), f.saved...)
}

func withRecorder(recorder Recorder) func(*Config) {
	return func(cfg *Config) {
		cfg.Recorder = recorder
		cfg.ASRPrefixRegex = qwenPrefix
	}
}

// runStore runs the store's background work until the test ends.
func runStore(t *testing.T, store *recordings.Store) {
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

func storedCount(t *testing.T, store *recordings.Store) int {
	t.Helper()

	_, total, err := store.List("", 1)
	require.NoError(t, err)

	return total
}

func TestPipeline_Transcribe_SavesStrippedTranscriptWithSource(t *testing.T) {
	recorder := &fakeRecorder{}
	p, fake := newPipeline(t, serveTranscript(t, "language English<asr_text> "+transcriptText), withRecorder(recorder))

	for _, source := range []Source{SourceOpenAI, SourceWyoming} {
		text, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), source)
		require.NoError(t, err)
		assert.Equal(t, transcriptText, text)
	}

	saved := recorder.all()
	require.Len(t, saved, 2)
	sent := fake.callsTo(asrRoute)
	require.Len(t, sent, 2)
	assert.Equal(t, sent[0].files["file"], saved[0].wav, "the recording is the WAV sent upstream")
	assert.Equal(t, savedRecording{wav: saved[0].wav, source: "openai", transcript: transcriptText}, saved[0])
	assert.Equal(t, "wyoming", saved[1].source)
}

func TestPipeline_Transcribe_FailedTranscriptionSavesNothing(t *testing.T) {
	recorder := &fakeRecorder{}
	p, _ := newPipeline(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "asr unavailable", http.StatusServiceUnavailable)
	}, withRecorder(recorder))

	_, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceOpenAI)

	require.Error(t, err)
	assert.Empty(t, recorder.all())
}

func TestPipeline_Transcribe_DoesNotWaitForTheSave(t *testing.T) {
	store, err := recordings.Open(t.TempDir(), time.Now, discardLogger())
	require.NoError(t, err)
	p, _ := newPipeline(t, serveTranscript(t, transcriptText), withRecorder(store))

	text, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceOpenAI)

	require.NoError(t, err)
	assert.Equal(t, transcriptText, text)
	assert.Zero(t, storedCount(t, store), "nothing is saved until the worker runs")
	runStore(t, store)
	assert.Eventually(t, func() bool { return storedCount(t, store) == 1 }, time.Second, 5*time.Millisecond)
}

func TestPipeline_Transcribe_SaveFailureKeepsTranscriptAndWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := t.TempDir()
	logs := &logLines{}
	store, err := recordings.Open(dir, time.Now, slog.New(slog.NewJSONHandler(logs, nil)))
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, readOnlyDir))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, privateDir)) })
	runStore(t, store)
	p, _ := newPipeline(t, serveTranscript(t, transcriptText), withRecorder(store))

	text, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceWyoming)

	require.NoError(t, err)
	assert.Equal(t, transcriptText, text)
	var warning map[string]any
	require.Eventually(t, func() bool {
		for _, line := range logs.all(t) {
			if line["msg"] == "could not save recording" {
				warning = line
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "no warning for the failed save")
	assert.Equal(t, "WARN", warning["level"])
	assert.Equal(t, "wyoming", warning["source"])
	assert.Contains(t, warning["error"], "permission denied")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestPipeline_Transcribe_RealStoreSavesOnePair(t *testing.T) {
	dir := t.TempDir()
	store, err := recordings.Open(dir, time.Now, discardLogger())
	require.NoError(t, err)
	runStore(t, store)
	p, _ := newPipeline(t, serveTranscript(t, transcriptText), withRecorder(store))

	_, err = p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceOpenAI)

	require.NoError(t, err)
	require.Eventually(t, func() bool { return storedCount(t, store) == 1 }, time.Second, 5*time.Millisecond)
	list, _, err := store.List("", 1)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "openai", list[0].Source)
	assert.Equal(t, transcriptText, list[0].Transcript)
}
