package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	gateHold        = 100 * time.Millisecond
	arrivalGap      = 10 * time.Millisecond
	queueTimeout    = 300 * time.Millisecond
	concurrencyBody = `{"error":{"message":"too many requests","code":"concurrency_limit"}}`
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

// overlapUpstream answers like llama-swap with concurrencyLimit 1: a call
// that arrives while another is running gets a 429.
type overlapUpstream struct {
	mu     sync.Mutex
	active int
	events []string
	hold   time.Duration
}

func (o *overlapUpstream) enter(input string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.active > 0 {
		o.events = append(o.events, "429 "+input)
		return false
	}

	o.active++
	o.events = append(o.events, "start "+input)
	return true
}

func (o *overlapUpstream) leave(input string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.active--
	o.events = append(o.events, "end "+input)
}

func (o *overlapUpstream) log() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return slices.Clone(o.events)
}

func (o *overlapUpstream) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input := r.FormValue("input")
		if !o.enter(input) {
			w.Header().Set("Content-Type", contentTypeJSON)
			w.WriteHeader(http.StatusTooManyRequests)
			writeOrFail(t, w, concurrencyBody)
			return
		}
		defer o.leave(input)

		select {
		case <-time.After(o.hold):
		case <-r.Context().Done():
			return
		}
		serveWAV(t)(w, r)
	}
}

type speechResult struct {
	status int
	body   string
	err    error
}

func sendSpeech(t *testing.T, h *harness, input, format string) speechResult {
	payload := fmt.Sprintf(`{"input":%q,"response_format":%q}`, input, format)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+speechPath, strings.NewReader(payload))
	if err != nil {
		return speechResult{err: err}
	}
	req.Header.Set("Content-Type", contentTypeJSON)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return speechResult{err: err}
	}
	defer closeQuietly(t, resp.Body)

	body, err := io.ReadAll(resp.Body)
	return speechResult{status: resp.StatusCode, body: string(body), err: err}
}

func requireTimeoutBody(t *testing.T, got speechResult) {
	t.Helper()

	require.NoError(t, got.err, "want the full 504 body, not a dropped connection")
	require.Equal(t, http.StatusGatewayTimeout, got.status, got.body)

	var body errorBody
	require.NoError(t, json.Unmarshal([]byte(got.body), &body), "504 body %q is not complete JSON", got.body)
	assert.NotEmpty(t, body.Error.Message)
}

func TestServer_HandleSpeech_ConcurrentRequestsNeverOverlapUpstream(t *testing.T) {
	const requests = 3

	tests := map[string]string{"wav": upstreamAudio, "mp3": mp3FrameSync}
	for format, prefix := range tests {
		t.Run(format, func(t *testing.T) {
			fake := &overlapUpstream{hold: gateHold}
			h := newHarness(t, fake.serve(t))

			results := make([]speechResult, requests)
			var wg sync.WaitGroup
			for i := range requests {
				wg.Go(func() { results[i] = sendSpeech(t, h, fmt.Sprintf("sentence %d", i), format) })
			}
			wg.Wait()

			for i, got := range results {
				require.NoError(t, got.err, "request %d", i)
				assert.Equal(t, http.StatusOK, got.status, "request %d: %s", i, got.body)
				assert.True(t, strings.HasPrefix(got.body, prefix), "request %d: body %q", i, got.body)
			}
			assert.NotContains(t, strings.Join(fake.log(), "\n"), "429", "upstream saw overlapping calls: %v", fake.log())
		})
	}
}

func TestServer_HandleSpeech_QueuedRequestsReachUpstreamInArrivalOrder(t *testing.T) {
	fake := &overlapUpstream{hold: gateHold}
	h := newHarness(t, fake.serve(t))

	var wg sync.WaitGroup
	for _, input := range []string{"A", "B", "C"} {
		wg.Go(func() {
			got := sendSpeech(t, h, input, "wav")
			assert.NoError(t, got.err, input)
			assert.Equal(t, http.StatusOK, got.status, "%s: %s", input, got.body)
		})
		time.Sleep(arrivalGap)
	}
	wg.Wait()

	assert.Equal(t, []string{"start A", "end A", "start B", "end B", "start C", "end C"}, fake.log())
}

func TestServer_HandleSpeech_TimeoutWhileQueuedIs504(t *testing.T) {
	h := newHarness(t, serveWAV(t), func(cfg *Config) { cfg.RequestTimeout = queueTimeout })
	require.NoError(t, h.service.ttsGate.acquire(t.Context()))

	start := time.Now()
	got := sendSpeech(t, h, "queued", "wav")
	elapsed := time.Since(start)

	requireTimeoutBody(t, got)
	assert.Contains(t, got.body, "waiting for a free TTS slot")
	assert.Less(t, elapsed, queueTimeout+deadlineSlack)
	assert.Zero(t, h.upstream.callCount(), "the queued request reached upstream")
	assert.Zero(t, h.service.ttsGate.queued(), "the timed-out request stayed queued")

	h.service.ttsGate.release()
	after := sendSpeech(t, h, "after", "wav")
	require.NoError(t, after.err)
	assert.Equal(t, http.StatusOK, after.status, "the gate leaked a slot: %s", after.body)
}

func TestServer_HandleSpeech_SlowUpstreamTimeoutIs504(t *testing.T) {
	stall := func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}
	stallAfterHeaders := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		assert.NoError(t, http.NewResponseController(w).Flush())
		<-r.Context().Done()
	}

	tests := map[string]struct {
		respond http.HandlerFunc
		format  string
	}{
		"no answer":          {stall, "wav"},
		"stream before data": {stallAfterHeaders, "mp3"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tt.respond, func(cfg *Config) { cfg.RequestTimeout = shortDeadline })

			start := time.Now()
			got := sendSpeech(t, h, "slow", tt.format)
			elapsed := time.Since(start)

			requireTimeoutBody(t, got)
			assert.Less(t, elapsed, shortDeadline+deadlineSlack)
			assert.Equal(t, 1, h.service.ttsGate.available(), "the timed-out request kept its TTS slot")
		})
	}
}
