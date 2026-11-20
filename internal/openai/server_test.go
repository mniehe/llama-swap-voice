package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	rawSpeechRequest  = "POST /v1/audio/speech HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: "
	rawStalledHealth  = "GET /health HTTP/1.1\r\nHost: x\r\nContent-Length: 100\r\n\r\n"
	shortDeadline     = 300 * time.Millisecond
	deadlineSlack     = 700 * time.Millisecond
	stalledBodyLength = "100\r\n\r\n"
	transcriptionPath = "/v1/audio/transcriptions"
)

func readUntilClosed(t *testing.T, conn net.Conn) (string, time.Duration) {
	t.Helper()

	start := time.Now()
	require.NoError(t, conn.SetReadDeadline(start.Add(stuckTimeout)))
	data, err := io.ReadAll(conn)

	var netErr net.Error
	require.False(t, errors.As(err, &netErr) && netErr.Timeout(), "server kept the connection open for %s", stuckTimeout)

	return string(data), time.Since(start)
}

func dialServer(t *testing.T, h *harness) *net.TCPConn {
	t.Helper()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", h.server.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { closeQuietly(t, conn) })

	tcp, ok := conn.(*net.TCPConn)
	require.True(t, ok, "connection is %T, not TCP", conn)

	return tcp
}

// postStatus runs off the test goroutine, so it reports failures without
// stopping the test.
func postStatus(t *testing.T, url, body string) int {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if !assert.NoError(t, err) {
		return 0
	}

	resp, err := http.DefaultClient.Do(req)
	if !assert.NoError(t, err) {
		return 0
	}
	closeQuietly(t, resp.Body)

	return resp.StatusCode
}

func TestServer_Limited_MaxConcurrentTTSIs503(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-time.After(stuckTimeout):
		}
		serveWAV(t)(w, r)
	}, func(cfg *Config) { cfg.MaxConcurrentTTS = 1 })

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- postStatus(t, h.server.URL+speechPath, `{"input":"first","response_format":"wav"}`)
	}()

	select {
	case <-started:
	case <-time.After(stuckTimeout):
		require.Fail(t, "first request never reached upstream")
	}

	resp := h.speech(speechPath, map[string]any{"input": "second", "response_format": "wav"})
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, errBusy.Error(), errorMessage(t, resp))

	close(release)
	assert.Equal(t, http.StatusOK, <-firstDone)
}

func requireBusy(t *testing.T, resp *http.Response) {
	t.Helper()

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, errBusy.Error(), errorMessage(t, resp))
	assert.True(t, resp.Close, "want Connection: close on a rejected request")
}

// waitInGate fails with the reply of a speech request that ended instead of
// reaching its place in the gate.
func waitInGate(t *testing.T, placed func() bool, reply <-chan speechResult) {
	t.Helper()

	deadline := time.After(waitTimeout)
	for !placed() {
		select {
		case got := <-reply:
			require.FailNow(t, "a speech request ended before its gate turn", "status %d: %s", got.status, got.body)
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

func TestServer_Limited_QueuedSpeechLeavesTranscriptionSlots(t *testing.T) {
	const speechSlots = 8
	release := make(chan struct{})
	paused := servePaused(t, streamedSpeech(t, fixtureSamples(t)), release)
	transcript := serveTranscript(t, transcriptText)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
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

	gate := h.service.ttsGate
	inputs := make([]string, speechSlots)
	replies := make([]chan speechResult, speechSlots)
	for i := range speechSlots {
		inputs[i] = fmt.Sprintf("sentence %d", i)
		replies[i] = make(chan speechResult, 1)
		wg.Go(func() { replies[i] <- sendSpeech(t, h, inputs[i], "wav") })

		placed := func() bool { return gate.queued() == i }
		if i == 0 {
			placed = func() bool { return len(h.upstream.callsTo(ttsRoute)) == 1 }
		}
		waitInGate(t, placed, replies[i])
	}

	resp := h.transcribe(nil, []byte(streamedWAV))
	body := string(readBody(t, resp))
	require.Equal(t, http.StatusOK, resp.StatusCode, "a transcription was refused while speech waited: %s", body)
	assert.Contains(t, body, transcriptText)

	ninth := h.speech(speechPath, map[string]any{"input": "sentence 8", "response_format": "wav"})
	requireBusy(t, ninth)
	assert.Equal(t, speechSlots-1, gate.queued(), "the rejected speech request joined the gate")
	assert.Len(t, h.upstream.callsTo(ttsRoute), 1, "the rejected speech request reached the upstream")

	releaseAll()
	for i, reply := range replies {
		got := <-reply
		require.NoError(t, got.err, inputs[i])
		assert.Equal(t, http.StatusOK, got.status, "%s: %s", inputs[i], got.body)
	}
	assert.Equal(t, inputs, inputsOf(h.upstream.callsTo(ttsRoute)))
	assert.Equal(t, 1, gate.available(), "a finished speech request kept its TTS slot")
}

// transcriptionStatus runs off the test goroutine, so it reports failures
// without stopping the test.
func transcriptionStatus(t *testing.T, h *harness, contentType string, body []byte) int {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+transcriptionPath, bytes.NewReader(body))
	if !assert.NoError(t, err) {
		return 0
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if !assert.NoError(t, err) {
		return 0
	}
	closeQuietly(t, resp.Body)

	return resp.StatusCode
}

func TestServer_Limited_HeldTranscriptionsLeaveSpeechSlots(t *testing.T) {
	const transcriptionSlots = 4
	release := make(chan struct{})
	transcript := serveTranscript(t, transcriptText)
	speech := serveWAV(t)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != asrRoute {
			speech(w, r)
			return
		}

		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		transcript(w, r)
	}, func(cfg *Config) { cfg.MaxConcurrent = transcriptionSlots })

	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	releaseAll := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseAll)

	contentType, form := transcriptionForm(t, nil, []byte(streamedWAV))
	statuses := make(chan int, transcriptionSlots)
	for range transcriptionSlots {
		wg.Go(func() { statuses <- transcriptionStatus(t, h, contentType, form) })
	}
	require.Eventually(t, func() bool { return len(h.upstream.callsTo(asrRoute)) == transcriptionSlots }, waitTimeout, time.Millisecond,
		"the transcriptions never all reached the upstream")

	resp := h.speech(speechPath, map[string]any{"input": "hello", "response_format": "wav"})
	require.Equal(t, http.StatusOK, resp.StatusCode, "a speech request was refused while transcriptions ran: %s", readBody(t, resp))

	fifth := h.post(transcriptionPath, contentType, bytes.NewReader(form))
	requireBusy(t, fifth)
	assert.Len(t, h.upstream.callsTo(asrRoute), transcriptionSlots, "the rejected transcription reached the upstream")

	releaseAll()
	wg.Wait()
	close(statuses)

	var got []int
	for status := range statuses {
		got = append(got, status)
	}
	assert.Equal(t, slices.Repeat([]int{http.StatusOK}, transcriptionSlots), got)
}

func TestServer_Limited_StalledBodyReleasesSlot(t *testing.T) {
	h := newHarness(t, serveWAV(t), func(cfg *Config) {
		cfg.MaxConcurrentTTS = 1
		cfg.RequestTimeout = 200 * time.Millisecond
	})

	conn := dialServer(t, h)
	writeOrFail(t, conn, rawSpeechRequest+"100\r\n\r\n")
	time.Sleep(500 * time.Millisecond)

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "wav"})
	assert.Equal(t, http.StatusOK, resp.StatusCode, "want 200 once the stalled body timed out")
}

func TestServer_Limited_NonReadingClientReleasesSlot(t *testing.T) {
	const (
		streamChunks = 4096
		chunkBytes   = 64 * 1024
		readBuffer   = 4096
	)
	chunk := make([]byte, chunkBytes)
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, upstreamHeader)
		for range streamChunks {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}, func(cfg *Config) {
		cfg.MaxConcurrentTTS = 1
		cfg.RequestTimeout = 300 * time.Millisecond
	})

	conn := dialServer(t, h)
	require.NoError(t, conn.SetReadBuffer(readBuffer))

	body := `{"input":"first","response_format":"wav"}`
	writeOrFail(t, conn, rawSpeechRequest+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	time.Sleep(h.cfg.RequestTimeout + responseWriteSlack + deadlineSlack)

	resp := h.speech(speechPath, map[string]any{"input": "second", "response_format": "wav"})
	assert.Equal(t, http.StatusOK, resp.StatusCode, "want 200 while the first client still reads nothing")
}

func TestServer_Limited_BusyStalledBodyEndsAtDeadline(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-r.Context().Done()
	}, func(cfg *Config) {
		cfg.MaxConcurrentTTS = 1
		cfg.RequestTimeout = shortDeadline
	})

	body := `{"input":"first","response_format":"wav"}`
	writeOrFail(t, dialServer(t, h), rawSpeechRequest+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	<-started

	conn := dialServer(t, h)
	writeOrFail(t, conn, rawSpeechRequest+stalledBodyLength)
	reply, elapsed := readUntilClosed(t, conn)

	assert.Contains(t, reply, "503 Service Unavailable")
	assert.Contains(t, reply, "Connection: close")
	assert.Less(t, elapsed, shortDeadline+deadlineSlack)
}

func TestServer_Bounded_StalledHealthBodyEndsAtDeadline(t *testing.T) {
	h := newHarness(t, serveWAV(t))
	h.service.controlTimeout = shortDeadline
	h.server = httptest.NewServer(h.service.Handler())
	t.Cleanup(h.server.Close)

	conn := dialServer(t, h)
	writeOrFail(t, conn, rawStalledHealth)
	_, elapsed := readUntilClosed(t, conn)

	assert.Less(t, elapsed, shortDeadline+deadlineSlack)
}

func TestServer_Drain_RejectsLaterRequests(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	require.NoError(t, h.service.Drain(t.Context()), "nothing in flight, so the drain is immediate")

	resp := h.get("/health")
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.True(t, resp.Close, "want Connection: close on a rejected request")
	assert.Equal(t, errShuttingDown.Error(), errorMessage(t, resp))
}

func TestServer_Drain_WaitsForRunningHandler(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		serveWAV(t)(w, r)
	})

	finished := make(chan int, 1)
	go func() {
		finished <- postStatus(t, h.server.URL+speechPath, `{"input":"hi","response_format":"wav"}`)
	}()
	<-started

	ctx, cancel := context.WithTimeout(t.Context(), shortDeadline)
	defer cancel()
	require.ErrorIs(t, h.service.Drain(ctx), context.DeadlineExceeded)

	close(release)
	require.NoError(t, h.service.Drain(t.Context()))
	assert.Equal(t, http.StatusOK, <-finished)
}

func TestServer_Handler_ReadinessFailsWhileShuttingDown(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	ready := h.get("/internal/readiness")
	assert.Equal(t, http.StatusOK, ready.StatusCode)
	assert.Equal(t, probeOK, string(readBody(t, ready)))

	h.service.SetShuttingDown()

	closing := h.get("/internal/readiness")
	assert.Equal(t, http.StatusServiceUnavailable, closing.StatusCode)
	assert.Equal(t, probeClosed, string(readBody(t, closing)))

	alive := h.get("/internal/liveness")
	assert.Equal(t, http.StatusOK, alive.StatusCode)
	assert.Equal(t, probeOK, string(readBody(t, alive)))
}

func TestServer_Handler_UnknownPathIsJSON404(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	for _, path := range []string{"/nope", "/v1/audio/speak", "/v1/models/extra"} {
		resp := h.get(path)

		assert.Equal(t, http.StatusNotFound, resp.StatusCode, path)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), path)
		assert.Contains(t, errorMessage(t, resp), path, path)
	}
}

func TestServer_Handler_WrongMethodIsJSON405(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	tests := []struct {
		method string
		path   string
		allow  string
	}{
		{http.MethodGet, speechPath, "POST"},
		{http.MethodGet, "/audio/transcriptions", "POST"},
		{http.MethodPost, "/v1/models", "GET, HEAD"},
		{http.MethodDelete, "/health", "GET, HEAD"},
	}

	for _, tt := range tests {
		req, err := http.NewRequestWithContext(t.Context(), tt.method, h.server.URL+tt.path, http.NoBody)
		require.NoError(t, err)

		resp := h.do(req)

		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "%s %s", tt.method, tt.path)
		assert.Equal(t, tt.allow, resp.Header.Get("Allow"), "%s %s", tt.method, tt.path)
		assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"), "%s %s", tt.method, tt.path)
		assert.Contains(t, errorMessage(t, resp), tt.method, "%s %s", tt.method, tt.path)
	}
}

func TestNew_InvalidPrefixRegex_ReturnsError(t *testing.T) {
	_, err := New(&Config{ASRPrefixRegex: "(", MaxConcurrent: 1}, nil, discardLogger())

	require.Error(t, err)
}

func TestElapsedMillis_ZeroEnd_IsNegative(t *testing.T) {
	start := time.Now()

	assert.Equal(t, int64(-1), elapsedMillis(start, time.Time{}))
	assert.Equal(t, int64(250), elapsedMillis(start, start.Add(250*time.Millisecond)))
}
