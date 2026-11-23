package openai

import (
	"encoding/json"
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

// holdTTSGate takes the only TTS slot through the pipeline, on a reply the
// upstream holds open, and returns the stream whose Close frees the slot.
func holdTTSGate(t *testing.T, h *harness) io.Closer {
	t.Helper()

	job, err := h.pipeline.PrepareSpeech("", "holder")
	require.NoError(t, err)

	speech, err := h.pipeline.Speak(t.Context(), job, normalSpeed)
	require.NoError(t, err)
	t.Cleanup(func() { closeQuietly(t, speech.PCM) })

	return speech.PCM
}

func TestServer_HandleSpeech_TimeoutWhileQueuedIs504(t *testing.T) {
	release := make(chan struct{})
	releaseHolder := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseHolder)
	paused := servePaused(t, streamedSpeech(t, fixtureSamples(t)), release)
	h := newHarness(t, paused, func(cfg *Config) { cfg.RequestTimeout = queueTimeout })
	holder := holdTTSGate(t, h)

	start := time.Now()
	got := sendSpeech(t, h, "queued", "wav")
	elapsed := time.Since(start)

	requireTimeoutBody(t, got)
	assert.Contains(t, got.body, "waiting for a free TTS slot")
	assert.Less(t, elapsed, queueTimeout+deadlineSlack)
	assert.Len(t, h.upstream.callsTo(ttsRoute), 1, "the queued request reached upstream")

	releaseHolder()
	require.NoError(t, holder.Close())
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
		})
	}
}
