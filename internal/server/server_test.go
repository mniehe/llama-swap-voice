package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const rawSpeechRequest = "POST /v1/audio/speech HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: "

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

func TestServer_Limited_MaxConcurrentIs503(t *testing.T) {
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
	}, func(cfg *Config) { cfg.MaxConcurrent = 1 })

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

func TestServer_Limited_StalledBodyReleasesSlot(t *testing.T) {
	h := newHarness(t, serveWAV(t), func(cfg *Config) {
		cfg.MaxConcurrent = 1
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
		writeOrFail(t, w, upstreamWAVs)
		for range streamChunks {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}, func(cfg *Config) {
		cfg.MaxConcurrent = 1
		cfg.RequestTimeout = 300 * time.Millisecond
	})

	conn := dialServer(t, h)
	require.NoError(t, conn.SetReadBuffer(readBuffer))

	body := `{"input":"first","response_format":"wav"}`
	writeOrFail(t, conn, rawSpeechRequest+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	time.Sleep(time.Second)

	resp := h.speech(speechPath, map[string]any{"input": "second", "response_format": "wav"})
	assert.Equal(t, http.StatusOK, resp.StatusCode, "want 200 while the first client still reads nothing")
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

func TestNew_InvalidPrefixRegex_ReturnsError(t *testing.T) {
	_, err := New(&Config{ASRPrefixRegex: "(", MaxConcurrent: 1}, nil, discardLogger())

	require.Error(t, err)
}

func TestElapsedMillis_ZeroEnd_IsNegative(t *testing.T) {
	start := time.Now()

	assert.Equal(t, int64(-1), elapsedMillis(start, time.Time{}))
	assert.Equal(t, int64(250), elapsedMillis(start, start.Add(250*time.Millisecond)))
}
