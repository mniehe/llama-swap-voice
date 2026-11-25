package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
)

const (
	busyMessage = "too many concurrent requests, retry shortly"
	qwenPrefix  = `^language \w+<asr_text>`
)

func expiredContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)

	return ctx
}

func TestPipeline_Admit_EveryTTSSlotHeldIsBusy(t *testing.T) {
	const slots = 2
	p, _ := newPipeline(t, serveWAV(t), func(cfg *Config) { cfg.MaxConcurrentTTS = slots })
	deadline := p.RequestDeadline(time.Now())

	for range slots {
		_, release, err := p.Admit(t.Context(), WorkSpeak, deadline)
		require.NoError(t, err)
		t.Cleanup(release)
	}

	ctx, release, err := p.Admit(t.Context(), WorkSpeak, deadline)

	requireKind(t, err, KindBusy, busyMessage)
	assert.Nil(t, ctx)
	assert.Nil(t, release)
}

func TestPipeline_Admit_OneTTSSlotTwoCallers(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t), func(cfg *Config) { cfg.MaxConcurrentTTS = 1 })
	deadline := p.RequestDeadline(time.Now())

	_, releaseFirst, err := p.Admit(t.Context(), WorkSpeak, deadline)
	require.NoError(t, err)

	_, _, err = p.Admit(t.Context(), WorkSpeak, deadline)
	requireKind(t, err, KindBusy, busyMessage)

	releaseFirst()

	_, releaseSecond, err := p.Admit(t.Context(), WorkSpeak, deadline)
	require.NoError(t, err, "the slot did not come back after release")
	releaseSecond()
}

func TestPipeline_Admit_WorkKindsUseSeparateSlots(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t), func(cfg *Config) {
		cfg.MaxConcurrent = 1
		cfg.MaxConcurrentTTS = 1
	})
	deadline := p.RequestDeadline(time.Now())

	_, releaseSpeech, err := p.Admit(t.Context(), WorkSpeak, deadline)
	require.NoError(t, err)
	t.Cleanup(releaseSpeech)

	_, releaseTranscription, err := p.Admit(t.Context(), WorkTranscribe, deadline)
	require.NoError(t, err, "a held speech slot refused a transcription")
	t.Cleanup(releaseTranscription)

	_, _, err = p.Admit(t.Context(), WorkTranscribe, deadline)
	requireKind(t, err, KindBusy, busyMessage)
}

func TestPipeline_RequestDeadline_AddsRequestTimeout(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))
	now := time.Now()

	assert.Equal(t, now.Add(harnessTimeout), p.RequestDeadline(now))
}

func TestPipeline_Admit_AppliesDeadline(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))
	deadline := time.Now().Add(shortDeadline)

	ctx, release, err := p.Admit(t.Context(), WorkTranscribe, deadline)
	require.NoError(t, err)

	got, ok := ctx.Deadline()
	require.True(t, ok, "the admitted context has no deadline")
	assert.Equal(t, deadline, got)

	release()
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "release left the admitted context running")
}

func TestPipeline_Enter_AfterDrainIsFalse(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))
	require.True(t, p.Enter())
	p.Leave()

	require.NoError(t, p.Drain(t.Context()), "nothing in flight, so the drain is immediate")

	assert.False(t, p.Enter(), "Enter admitted a request after the drain began")
	requireKind(t, ErrShuttingDown, KindShuttingDown, "server is shutting down")
}

func TestPipeline_Drain_WaitsForLeave(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))
	require.True(t, p.Enter())

	ctx, cancel := context.WithTimeout(t.Context(), shortDeadline)
	defer cancel()
	require.ErrorIs(t, p.Drain(ctx), context.DeadlineExceeded)

	p.Leave()
	require.NoError(t, p.Drain(t.Context()))
}

func TestPipeline_Limits_ReportsSharedBounds(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))

	assert.Equal(t, Limits{RequestTimeout: harnessTimeout, MaxInputChars: harnessMaxChars, MaxUploadBytes: 25 << 20}, p.Limits())
}

func TestPipeline_Catalogue_ListsModelsAndSortedVoices(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))

	got := p.Catalogue()

	assert.Equal(t, Catalogue{
		ASRModel:     asrModel,
		TTSModel:     ttsModel,
		Voices:       []string{"alice", "narrator"},
		DefaultVoice: "alice",
		Lang:         "en",
	}, got)
}

func TestPipeline_PrepareSpeech_UnknownVoiceIsInvalid(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))

	_, err := p.PrepareSpeech("bob", "hi")

	requireKind(t, err, KindInvalid, `unknown voice "bob"; available voices: alice, narrator`)
}

func TestPipeline_PrepareSpeech_RejectsInput(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))

	tests := map[string]struct {
		input   string
		message string
	}{
		"empty":      {"", "input is required"},
		"blank":      {" \n\t ", "input is required"},
		"over limit": {strings.Repeat("é", harnessMaxChars+1), fmt.Sprintf("input exceeds %d characters", harnessMaxChars)},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := p.PrepareSpeech("alice", tt.input)
			requireKind(t, err, KindInvalid, tt.message)
		})
	}
}

func TestPipeline_PrepareSpeech_TrimsInputAndDefaultsVoice(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))

	job, err := p.PrepareSpeech("", "  hello  ")

	require.NoError(t, err)
	assert.Equal(t, "alice", job.Voice)
	assert.Equal(t, "hello", job.input)
	assert.Equal(t, p.cfg.Voices["alice"], job.voicePath)
}

func TestPipeline_ResolveSpeed(t *testing.T) {
	speed := func(v float64) *float64 { return &v }

	got, err := ResolveSpeed(nil)
	require.NoError(t, err)
	assert.InDelta(t, normalSpeed, got, 0)

	got, err = ResolveSpeed(speed(1.5))
	require.NoError(t, err)
	assert.InDelta(t, 1.5, got, 0)

	for _, outside := range []float64{0.1, 4.5} {
		_, err := ResolveSpeed(speed(outside))
		requireKind(t, err, KindInvalid, "speed must be between 0.25 and 4")
	}
}

func TestPipeline_Speak_ReturnsPCMWithoutHeader(t *testing.T) {
	p, fake := newPipeline(t, serveWAV(t))

	speech, err := p.Speak(t.Context(), prepare(t, p, "narrator", "hello"), normalSpeed)

	require.NoError(t, err)
	require.NotNil(t, speech)
	assert.Equal(t, upstreamFrames, readSpeech(t, speech))
	assert.Equal(t, "narrator", speech.Voice)
	assert.Equal(t, http.StatusOK, speech.UpstreamStatus)
	assert.False(t, speech.Retried)

	calls := fake.callsTo(ttsRoute)
	require.Len(t, calls, 1)
	assert.Equal(t, "hello", calls[0].fields["input"])
	assert.Equal(t, "en", calls[0].fields["lang"])
	assert.Equal(t, "wav", calls[0].fields["response_format"])
	assert.Equal(t, []byte("narrator.wav reference audio"), calls[0].files["speaker_ref"])
	assert.Equal(t, 1, p.ttsGate.available(), "a finished speech kept its gate slot")
}

func TestPipeline_Speak_NonWAVUpstreamIsUpstreamError(t *testing.T) {
	p, _ := newPipeline(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		writeOrFail(t, w, `{"error":"not audio"}`)
	})
	job := prepare(t, p, "alice", "hi")

	_, err := p.Speak(t.Context(), job, normalSpeed)

	requireKind(t, err, KindUpstream, "")
	failed := pipelineError(t, err)
	assert.True(t, strings.HasPrefix(failed.Message, "upstream audio: "), failed.Message)
	assert.Contains(t, failed.Message, "not a WAV stream")
	assert.Equal(t, http.StatusOK, failed.UpstreamStatus)
	assert.Nil(t, failed.Upstream)

	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	_, err = p.Speak(ctx, job, normalSpeed)
	requireKind(t, err, KindUpstream, failed.Message)
	assert.Equal(t, 1, p.ttsGate.available(), "a failed speech kept its gate slot")
}

func TestPipeline_Speak_HeaderOnlyUpstreamHasNoAudio(t *testing.T) {
	p, _ := newPipeline(t, serveBody(t, upstreamHeader))

	_, err := p.Speak(t.Context(), prepare(t, p, "alice", "hi"), normalSpeed)

	requireKind(t, err, KindUpstream, "upstream sent no audio")
	failed := pipelineError(t, err)
	assert.Equal(t, http.StatusOK, failed.UpstreamStatus)
	assert.Equal(t, 1, p.ttsGate.available())
}

func TestPipeline_Speak_StretcherFailureIsInternal(t *testing.T) {
	p, _ := newPipeline(t, serveWAV(t))

	_, err := p.Speak(t.Context(), prepare(t, p, "alice", "hi"), 0)

	requireKind(t, err, KindInternal, "speed must be between 0.25 and 4, got 0")
	failed := pipelineError(t, err)
	assert.Equal(t, http.StatusOK, failed.UpstreamStatus)
	assert.Equal(t, 1, p.ttsGate.available())
}

func answerStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(code), code)
	}
}

// inTurn answers the nth upstream call with the nth handler.
func inTurn(handlers ...http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	next := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handler := handlers[min(next, len(handlers)-1)]
		next++
		mu.Unlock()
		handler(w, r)
	}
}

func TestPipeline_Speak_ReportsRetry(t *testing.T) {
	p, _ := newPipeline(t, inTurn(answerStatus(http.StatusBadGateway), serveWAV(t)))

	speech, err := p.Speak(t.Context(), prepare(t, p, "alice", "hi"), normalSpeed)

	require.NoError(t, err)
	require.NotNil(t, speech)
	assert.True(t, speech.Retried)
	assert.Equal(t, upstreamFrames, readSpeech(t, speech))
}

func TestPipeline_Speak_ReportsRetryOnFailureAfterResponse(t *testing.T) {
	p, _ := newPipeline(t, inTurn(answerStatus(http.StatusBadGateway), serveBody(t, upstreamHeader)))

	_, err := p.Speak(t.Context(), prepare(t, p, "alice", "hi"), normalSpeed)

	requireKind(t, err, KindUpstream, "upstream sent no audio")
	failed := pipelineError(t, err)
	assert.True(t, failed.Retried)
	assert.Equal(t, http.StatusOK, failed.UpstreamStatus)
}

func TestPipeline_Speak_UpstreamStatusErrorIsUpstream(t *testing.T) {
	p, _ := newPipeline(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "queue full", http.StatusTooManyRequests)
	})

	_, err := p.Speak(t.Context(), prepare(t, p, "alice", "hi"), normalSpeed)

	requireKind(t, err, KindUpstream, "upstream returned 429: queue full")
	failed := pipelineError(t, err)
	require.NotNil(t, failed.Upstream)
	assert.Equal(t, http.StatusTooManyRequests, failed.Upstream.Code)
	assert.Equal(t, "7", failed.Upstream.RetryAfter)
	assert.Equal(t, http.StatusTooManyRequests, failed.UpstreamStatus)
	assert.Equal(t, 1, p.ttsGate.available())
}

func TestPipeline_Speak_UpstreamEndedContextIsTimeout(t *testing.T) {
	p, _ := newPipeline(t, stall)
	ctx, cancel := context.WithTimeout(t.Context(), shortDeadline)
	defer cancel()

	_, err := p.Speak(ctx, prepare(t, p, "alice", "hi"), normalSpeed)

	requireKind(t, err, KindTimeout, "")
	failed := pipelineError(t, err)
	assert.Contains(t, failed.Message, context.DeadlineExceeded.Error())
	assert.Nil(t, failed.Upstream)
	assert.Zero(t, failed.UpstreamStatus)
}

func TestPipeline_Speak_UpstreamTransportErrorIsUpstream(t *testing.T) {
	p, _ := newPipeline(t, func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})

	_, err := p.Speak(t.Context(), prepare(t, p, "alice", "hi"), normalSpeed)

	requireKind(t, err, KindUpstream, "")
	failed := pipelineError(t, err)
	assert.Contains(t, failed.Message, "retried once")
	assert.Equal(t, failed.Err.Error(), failed.Message)
	assert.Nil(t, failed.Upstream)
	assert.Zero(t, failed.UpstreamStatus)
}

func TestPipeline_Speak_QueueWaitEndedIsTimeout(t *testing.T) {
	p, fake := newPipeline(t, serveWAV(t))
	require.NoError(t, p.ttsGate.acquire(t.Context()))
	t.Cleanup(p.ttsGate.release)

	_, err := p.Speak(expiredContext(t), prepare(t, p, "alice", "hi"), normalSpeed)

	requireKind(t, err, KindTimeout, "waiting for a free TTS slot: context deadline exceeded")
	assert.Zero(t, fake.callCount())
}

func TestPipeline_SpeechPCM_EndedRequestIsTimeout(t *testing.T) {
	ctx := expiredContext(t)

	_, err := speechPCM(ctx, strings.NewReader("not a wav stream at all, just text"), normalSpeed)
	requireKind(t, err, KindTimeout, "")
	failed := pipelineError(t, err)
	assert.True(t, strings.HasPrefix(failed.Message, "upstream audio: "), failed.Message)

	_, err = speechPCM(ctx, strings.NewReader(upstreamAudio), 0)
	requireKind(t, err, KindTimeout, "speed must be between 0.25 and 4, got 0")
}

func TestPipeline_FirstPCM_NoAudio(t *testing.T) {
	_, err := firstPCM(t.Context(), strings.NewReader(""))
	requireKind(t, err, KindUpstream, "upstream sent no audio")

	_, err = firstPCM(expiredContext(t), strings.NewReader(""))
	requireKind(t, err, KindTimeout, "upstream sent no audio")

	_, err = firstPCM(t.Context(), strings.NewReader("\x01"))
	requireKind(t, err, KindUpstream, "unexpected EOF")

	first, err := firstPCM(t.Context(), strings.NewReader("\x01\x02\x03"))
	require.NoError(t, err)
	assert.Equal(t, "\x01\x02\x03", string(first))
}

func TestSpeechPCM_SpeedOutsideStretcherRangeIsInternalError(t *testing.T) {
	pcm, err := speechPCM(t.Context(), strings.NewReader(upstreamAudio), 0)

	require.Error(t, err)
	assert.Nil(t, pcm)
	requireKind(t, err, KindInternal, "")
	assert.Contains(t, err.Error(), "between 0.25 and 4")
}

func TestPipeline_Transcribe_SendsOneUploadAndStripsPrefix(t *testing.T) {
	p, fake := newPipeline(t, serveTranscript(t, "language English<asr_text> "+transcriptText), func(cfg *Config) {
		cfg.ASRPrefixRegex = qwenPrefix
	})

	text, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceOpenAI)

	require.NoError(t, err)
	assert.Equal(t, transcriptText, text)
	calls := fake.callsTo(asrRoute)
	require.Len(t, calls, 1)
	assert.Equal(t, 1, fake.callCount())
	assert.True(t, strings.HasPrefix(string(calls[0].files["file"]), "RIFF"), "the upload sent upstream is not a WAV")
}

func TestPipeline_Transcribe_KeepsTextWithoutPrefixMatch(t *testing.T) {
	p, _ := newPipeline(t, serveTranscript(t, " plain words "), func(cfg *Config) {
		cfg.ASRPrefixRegex = qwenPrefix
	})

	text, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceOpenAI)

	require.NoError(t, err)
	assert.Equal(t, "plain words", text)
}

func TestPipeline_Transcribe_UpstreamStatusErrorIsUpstream(t *testing.T) {
	p, _ := newPipeline(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "asr unavailable", http.StatusServiceUnavailable)
	})

	_, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceOpenAI)

	requireKind(t, err, KindUpstream, "upstream returned 503: asr unavailable")
	failed := pipelineError(t, err)
	require.NotNil(t, failed.Upstream)
	assert.Equal(t, http.StatusServiceUnavailable, failed.UpstreamStatus)
}

func TestPipeline_Transcribe_MalformedTranscriptKeepsUpstreamStatusZero(t *testing.T) {
	p, _ := newPipeline(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		writeOrFail(t, w, "{not json")
	})

	_, err := p.Transcribe(t.Context(), strings.NewReader(streamedWAV), SourceOpenAI)

	requireKind(t, err, KindUpstream, "")
	failed := pipelineError(t, err)
	assert.Zero(t, failed.UpstreamStatus)
	assert.Nil(t, failed.Upstream)
}

func TestPipeline_Transcribe_EndedRequestIsTimeout(t *testing.T) {
	p, fake := newPipeline(t, serveTranscript(t, transcriptText))

	_, err := p.Transcribe(expiredContext(t), strings.NewReader(streamedWAV), SourceOpenAI)

	requireKind(t, err, KindTimeout, "decoding audio: context deadline exceeded")
	assert.Zero(t, fake.callCount())
}

func TestPipeline_DecodeFailure_MapsDecoderErrors(t *testing.T) {
	p, _ := newPipeline(t, serveTranscript(t, transcriptText))

	tests := map[string]struct {
		err     error
		kind    Kind
		message string
	}{
		"too long":    {audio.ErrTooLong, KindInvalid, "audio is longer than 20 minutes"},
		"unsupported": {fmt.Errorf("m4a: %w", audio.ErrUnsupported), KindInvalid, "could not decode audio: unsupported audio format"},
		"corrupt":     {fmt.Errorf("wav: %w", audio.ErrCorrupt), KindInvalid, "could not decode audio: corrupted audio"},
		"read":        {errors.New("disk on fire"), KindInternal, "internal server error"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			failed := p.decodeFailure(t.Context(), tt.err)

			require.NotNil(t, failed)
			requireKind(t, failed, tt.kind, tt.message)
			assert.ErrorIs(t, failed, tt.err)
		})
	}
}

func TestPipeline_DecodeFailure_ExpiredContextIsTimeout(t *testing.T) {
	p, _ := newPipeline(t, serveTranscript(t, transcriptText))
	ctx := expiredContext(t)

	for _, err := range []error{audio.ErrCorrupt, audio.ErrUnsupported, audio.ErrTooLong, context.DeadlineExceeded} {
		failed := p.decodeFailure(ctx, err)

		require.NotNil(t, failed, "expired context with %v", err)
		assert.Equal(t, KindTimeout, failed.Kind, "expired context with %v", err)
		assert.Equal(t, "decoding audio: context deadline exceeded", failed.Message, "expired context with %v", err)
		assert.ErrorIs(t, failed, context.DeadlineExceeded, "expired context with %v", err)
	}
}

func TestKind_ZeroValueIsNoKind(t *testing.T) {
	for _, kind := range []Kind{KindInvalid, KindBusy, KindTimeout, KindUpstream, KindShuttingDown, KindInternal} {
		assert.NotZero(t, kind, "an Error built without a kind would read as kind %d", kind)
	}
}

type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.buf.Write(p)
}

func (l *logLines) all(t *testing.T) []map[string]any {
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

func TestPipeline_Logs_CarryRequestID(t *testing.T) {
	const requestID = "req-123"
	logs := &logLines{}
	p, _ := newLoggedPipeline(t, slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})), stall)
	ctx := WithRequestID(t.Context(), requestID)

	corrupt := p.decodeFailure(ctx, fmt.Errorf("wav: %w", audio.ErrCorrupt))
	unreadable := p.decodeFailure(ctx, errors.New("disk on fire"))
	require.NotNil(t, corrupt)
	require.NotNil(t, unreadable)
	timed, cancel := context.WithTimeout(ctx, shortDeadline)
	defer cancel()
	_, err := p.Speak(timed, prepare(t, p, "alice", "hi"), normalSpeed)
	require.Error(t, err)

	lines := logs.all(t)
	messages := make([]string, 0, len(lines))
	for _, line := range lines {
		messages = append(messages, fmt.Sprint(line["msg"]))
		assert.Equal(t, requestID, line["request_id"], "log line %v", line)
	}
	assert.Equal(t, []string{"could not decode audio", "could not read upload", "request ended before upstream answered"}, messages)
}

func TestNew_InvalidPrefixRegex_ReturnsError(t *testing.T) {
	_, err := New(&Config{ASRPrefixRegex: "(", MaxConcurrent: 1}, nil, discardLogger())

	require.Error(t, err)
}
