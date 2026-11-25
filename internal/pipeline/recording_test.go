package pipeline

import (
	"errors"
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

// fakeRecorder records each Save and fails with err when it is set.
type fakeRecorder struct {
	mu    sync.Mutex
	saved []savedRecording
	err   error
}

func (f *fakeRecorder) Save(wav []byte, source, transcript string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.saved = append(f.saved, savedRecording{wav: wav, source: source, transcript: transcript})
	return f.err
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

func TestPipeline_Transcribe_SaveFailureKeepsTranscriptAndWarns(t *testing.T) {
	logs := &logLines{}
	recorder := &fakeRecorder{err: errors.New("disk full")}
	p, _ := newLoggedPipeline(t, slog.New(slog.NewJSONHandler(logs, nil)), serveTranscript(t, transcriptText), withRecorder(recorder))

	text, err := p.Transcribe(WithRequestID(t.Context(), "req-1"), strings.NewReader(streamedWAV), SourceOpenAI)

	require.NoError(t, err)
	assert.Equal(t, transcriptText, text)
	require.Len(t, recorder.all(), 1)
	warned := false
	for _, line := range logs.all(t) {
		if line["msg"] == "could not save recording" {
			warned = true
			assert.Equal(t, "WARN", line["level"])
			assert.Equal(t, "req-1", line["request_id"])
			assert.Equal(t, "disk full", line["error"])
		}
	}
	assert.True(t, warned, "no warning for the failed save")
}

func TestPipeline_Transcribe_UnwritableStoreKeepsTranscript(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := t.TempDir()
	store, err := recordings.Open(dir, time.Now, discardLogger())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, readOnlyDir))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, privateDir)) })
	p, _ := newPipeline(t, serveTranscript(t, transcriptText), withRecorder(store))

	text, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceWyoming)

	require.NoError(t, err)
	assert.Equal(t, transcriptText, text)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestPipeline_Transcribe_RealStoreSavesOnePair(t *testing.T) {
	dir := t.TempDir()
	store, err := recordings.Open(dir, time.Now, discardLogger())
	require.NoError(t, err)
	p, _ := newPipeline(t, serveTranscript(t, transcriptText), withRecorder(store))

	_, err = p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceOpenAI)

	require.NoError(t, err)
	list, err := store.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "openai", list[0].Source)
	assert.Equal(t, transcriptText, list[0].Transcript)
}
