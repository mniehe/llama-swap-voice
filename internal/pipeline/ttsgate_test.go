package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	arrivalGap   = 10 * time.Millisecond
	queueTimeout = 300 * time.Millisecond
)

func (g *ttsGate) queued() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return len(g.waiters)
}

func (g *ttsGate) available() int {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.free
}

func waitForQueue(t *testing.T, g *ttsGate, want int) {
	t.Helper()

	require.Eventually(t, func() bool { return g.queued() == want }, waitTimeout, time.Millisecond,
		"want %d queued waiters, have %d", want, g.queued())
}

func TestTTSGate_Acquire_LimitsHolders(t *testing.T) {
	g := newTTSGate(2)
	require.NoError(t, g.acquire(t.Context()))
	require.NoError(t, g.acquire(t.Context()))

	ctx, cancel := context.WithTimeout(t.Context(), arrivalGap)
	defer cancel()

	assert.ErrorIs(t, g.acquire(ctx), context.DeadlineExceeded, "a third holder got in past a size of 2")
}

func TestTTSGate_Acquire_AdmitsInArrivalOrder(t *testing.T) {
	const waiters = 3
	g := newTTSGate(1)
	require.NoError(t, g.acquire(t.Context()))

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	for id := range waiters {
		wg.Go(func() {
			if !assert.NoError(t, g.acquire(t.Context())) {
				return
			}
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
			g.release()
		})
		waitForQueue(t, g, id+1)
	}

	g.release()
	wg.Wait()

	assert.Equal(t, []int{0, 1, 2}, order)
	assert.Equal(t, 1, g.available(), "the slot did not come back after the last release")
}

func TestTTSGate_Acquire_ExpiredWaiterLeavesQueue(t *testing.T) {
	g := newTTSGate(1)
	require.NoError(t, g.acquire(t.Context()))

	ctx, cancel := context.WithTimeout(t.Context(), arrivalGap)
	defer cancel()

	assert.ErrorIs(t, g.acquire(ctx), context.DeadlineExceeded)
	assert.Zero(t, g.queued(), "an expired waiter stayed in the queue")

	g.release()
	assert.Equal(t, 1, g.available(), "the release went to the expired waiter")
	require.NoError(t, g.acquire(t.Context()))
}

func TestTTSGate_Abandon_HandedSlotPassesOn(t *testing.T) {
	g := newTTSGate(1)
	require.NoError(t, g.acquire(t.Context()))

	turn := make(chan struct{})
	g.mu.Lock()
	g.waiters = append(g.waiters, turn)
	g.mu.Unlock()

	g.release()
	select {
	case <-turn:
	case <-time.After(waitTimeout):
		require.Fail(t, "release did not hand the slot to the queued waiter")
	}

	errLate := errors.New("deadline won the race")
	assert.ErrorIs(t, g.abandon(turn, errLate), errLate)
	assert.Equal(t, 1, g.available(), "a slot handed to a waiter that gave up was lost")
}

type speechResult struct {
	pcm string
	err error
}

// admittedSpeech runs one speech request the way an entrypoint does: slot,
// deadline, synthesis, then the whole stream. It runs off the test goroutine,
// so it reports failures in its result.
func admittedSpeech(ctx context.Context, p *Pipeline, input string) speechResult {
	admitted, release, err := p.Admit(ctx, WorkSpeak, p.RequestDeadline(time.Now()))
	if err != nil {
		return speechResult{err: err}
	}
	defer release()

	job, err := p.PrepareSpeech("", input)
	if err != nil {
		return speechResult{err: err}
	}

	speech, err := p.Speak(admitted, job, normalSpeed)
	if err != nil {
		return speechResult{err: err}
	}

	pcm, err := io.ReadAll(speech.PCM)
	return speechResult{pcm: string(pcm), err: errors.Join(err, speech.PCM.Close())}
}

// servePaused sends the header and some audio, then holds the rest of the
// body until release closes.
func servePaused(t *testing.T, release <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, upstreamAudio)
		assert.NoError(t, http.NewResponseController(w).Flush())

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeOrFail(t, w, upstreamFrames)
	}
}

// waitInGate fails with the result of a speech request that ended instead of
// reaching its place in the gate.
func waitInGate(t *testing.T, placed func() bool, reply <-chan speechResult) {
	t.Helper()

	deadline := time.After(waitTimeout)
	for !placed() {
		select {
		case got := <-reply:
			require.FailNow(t, "a speech request ended before its gate turn", "error %v", got.err)
		case <-deadline:
			require.FailNow(t, "a speech request never reached its place in the gate")
		case <-time.After(time.Millisecond):
		}
	}
}

func inputsOf(calls []upstreamCall) []string {
	inputs := make([]string, 0, len(calls))
	for _, call := range calls {
		inputs = append(inputs, call.fields["input"])
	}

	return inputs
}

func TestPipeline_Admit_QueuedSpeechLeavesTranscriptionSlots(t *testing.T) {
	const speechSlots = 8
	release := make(chan struct{})
	paused := servePaused(t, release)
	transcript := serveTranscript(t, transcriptText)
	p, fake := newPipeline(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == asrRoute {
			transcript(w, r)
			return
		}
		paused(w, r)
	}, func(cfg *Config) { cfg.MaxConcurrentTTS = speechSlots })

	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	releaseAll := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseAll)

	gate := p.ttsGate
	inputs := make([]string, speechSlots)
	replies := make([]chan speechResult, speechSlots)
	for i := range speechSlots {
		inputs[i] = fmt.Sprintf("sentence %d", i)
		replies[i] = make(chan speechResult, 1)
		wg.Go(func() { replies[i] <- admittedSpeech(t.Context(), p, inputs[i]) })

		placed := func() bool { return gate.queued() == i }
		if i == 0 {
			placed = func() bool { return len(fake.callsTo(ttsRoute)) == 1 }
		}
		waitInGate(t, placed, replies[i])
	}

	ctx, releaseTranscription, err := p.Admit(t.Context(), WorkTranscribe, p.RequestDeadline(time.Now()))
	require.NoError(t, err, "a transcription was refused while speech waited")
	text, err := p.Transcribe(ctx, strings.NewReader(streamedWAV), SourceOpenAI)
	releaseTranscription()
	require.NoError(t, err)
	assert.Contains(t, text, transcriptText)

	_, _, err = p.Admit(t.Context(), WorkSpeak, p.RequestDeadline(time.Now()))
	requireKind(t, err, KindBusy, busyMessage)
	assert.Equal(t, speechSlots-1, gate.queued(), "the rejected speech request joined the gate")
	assert.Len(t, fake.callsTo(ttsRoute), 1, "the rejected speech request reached the upstream")

	releaseAll()
	for i, reply := range replies {
		got := <-reply
		require.NoError(t, got.err, inputs[i])
		assert.NotEmpty(t, got.pcm, inputs[i])
	}
	assert.Equal(t, inputs, inputsOf(fake.callsTo(ttsRoute)))
	assert.Equal(t, 1, gate.available(), "a finished speech request kept its TTS slot")
}

func TestPipeline_Speak_TimeoutWhileQueuedIsTimeout(t *testing.T) {
	p, fake := newPipeline(t, serveWAV(t), func(cfg *Config) { cfg.RequestTimeout = queueTimeout })
	require.NoError(t, p.ttsGate.acquire(t.Context()))

	start := time.Now()
	got := admittedSpeech(t.Context(), p, "queued")
	elapsed := time.Since(start)

	requireKind(t, got.err, KindTimeout, "")
	assert.Contains(t, got.err.Error(), "waiting for a free TTS slot")
	assert.Less(t, elapsed, queueTimeout+deadlineSlack)
	assert.Zero(t, fake.callCount(), "the queued request reached upstream")
	assert.Zero(t, p.ttsGate.queued(), "the timed-out request stayed queued")

	p.ttsGate.release()
	after := admittedSpeech(t.Context(), p, "after")
	require.NoError(t, after.err, "the gate leaked a slot")
	assert.Equal(t, upstreamFrames, after.pcm)
}

func TestPipeline_Speak_SlowUpstreamTimeoutReleasesGate(t *testing.T) {
	stallAfterHeaders := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		assert.NoError(t, http.NewResponseController(w).Flush())
		<-r.Context().Done()
	}

	tests := map[string]http.HandlerFunc{
		"no answer":          stall,
		"stream before data": stallAfterHeaders,
	}

	for name, respond := range tests {
		t.Run(name, func(t *testing.T) {
			p, _ := newPipeline(t, respond, func(cfg *Config) { cfg.RequestTimeout = shortDeadline })

			start := time.Now()
			got := admittedSpeech(t.Context(), p, "slow")
			elapsed := time.Since(start)

			requireKind(t, got.err, KindTimeout, "")
			assert.Less(t, elapsed, shortDeadline+deadlineSlack)
			assert.Equal(t, 1, p.ttsGate.available(), "the timed-out request kept its TTS slot")
		})
	}
}
