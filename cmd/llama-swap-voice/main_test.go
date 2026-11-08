package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/fakeffmpeg"
	"github.com/mniehe/llama-swap-voice/internal/server"
	"github.com/mniehe/llama-swap-voice/internal/transcode"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	probeTimeout = 3 * time.Second
	probeEvery   = 10 * time.Millisecond
	slowAudio    = "RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xffslow audio"

	forcedTotal   = 200 * time.Millisecond
	shutdownSlack = 500 * time.Millisecond
)

func TestMain(m *testing.M) {
	fakeffmpeg.RunIfRequested()
	os.Exit(m.Run())
}

func freePort(t *testing.T) int {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	require.NoError(t, listener.Close())

	return addr.Port
}

func newService(t *testing.T, respond http.HandlerFunc) *server.Server {
	t.Helper()

	fake := httptest.NewServer(respond)
	t.Cleanup(fake.Close)

	client, err := upstream.New(fake.URL, "tts", "asr", &http.Client{})
	require.NoError(t, err)

	cfg := validConfig(t)
	serverCfg := cfg.serverConfig(map[string]string{"alice": filepath.Join(cfg.Voices.Dir, "alice.mp3")})
	service, err := server.New(&serverCfg, client, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	return service
}

func get(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}

	return resp.StatusCode, resp.Body.Close()
}

func waitReady(t *testing.T, base string) {
	t.Helper()

	assert.Eventually(t, func() bool {
		status, err := get(t.Context(), base+"/internal/readiness")
		return err == nil && status == http.StatusOK
	}, probeTimeout, probeEvery)
}

func startServe(t *testing.T, cfg *ServiceConfig, service *server.Server) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- serve(ctx, cfg, service, slog.New(slog.DiscardHandler))
	}()

	return cancel, result
}

func TestServe_Signal_ShutsDownCleanly(t *testing.T) {
	cfg := validConfig(t)
	cfg.Port = freePort(t)
	base := "http://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	cancel, done := startServe(t, &cfg, newService(t, http.NotFound))
	waitReady(t, base)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(probeTimeout):
		require.Fail(t, "serve did not return after the signal")
	}
}

func TestServe_Signal_DrainsInFlightRequest(t *testing.T) {
	cfg := validConfig(t)
	cfg.Port = freePort(t)
	base := "http://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	reached := make(chan struct{})
	release := make(chan struct{})
	service := newService(t, func(w http.ResponseWriter, _ *http.Request) {
		close(reached)
		<-release
		_, err := io.WriteString(w, slowAudio)
		assert.NoError(t, err)
	})

	cancel, done := startServe(t, &cfg, service)
	waitReady(t, base)

	type result struct {
		body string
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		body, err := postSpeech(t.Context(), base)
		finished <- result{body: body, err: err}
	}()

	<-reached
	cancel()
	time.Sleep(probeEvery * 5)
	close(release)

	got := <-finished
	require.NoError(t, got.err)
	assert.Equal(t, slowAudio, got.body, "the in-flight stream must complete during shutdown")
	require.NoError(t, <-done)
}

// stuckSpeech starts a speech request whose upstream never finishes, slowed
// down by a fake ffmpeg that ignores SIGTERM, and returns once ffmpeg is
// running. Only a speed change runs ffmpeg.
func stuckSpeech(t *testing.T, cfg *ServiceConfig) (cancel context.CancelFunc, done <-chan error, pid int) {
	t.Helper()

	fake := fakeffmpeg.Install(t)
	fake.Stubborn(t)
	cfg.Port = freePort(t)
	base := "http://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	service := newService(t, func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, slowAudio)
		assert.NoError(t, err)
		assert.NoError(t, http.NewResponseController(w).Flush())
		<-r.Context().Done()
	})

	cancel, done = startServe(t, cfg, service)
	waitReady(t, base)

	go func() {
		_, err := postSpeechBody(t.Context(), base, `{"input":"hi","response_format":"mp3","speed":1.5}`)
		assert.Error(t, err, "a cancelled stream must not end cleanly")
	}()

	require.Eventually(t, func() bool {
		var ok bool
		pid, ok = fake.PID()
		return ok
	}, probeTimeout, probeEvery, "ffmpeg never started")

	return cancel, done, pid
}

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

func TestServe_StuckRequest_CancelsAndReapsFFmpeg(t *testing.T) {
	cfg := validConfig(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: 3 * transcode.KillDelay / 2}

	cancel, done, pid := stuckSpeech(t, &cfg)
	signalled := time.Now()
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "every handler returned within shutdown.hard")
	case <-time.After(cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack):
		require.Fail(t, "serve did not return within total+hard")
	}

	assert.Less(t, time.Since(signalled), cfg.ShutdownTiming.Total+cfg.ShutdownTiming.Hard+shutdownSlack)
	assert.True(t, processGone(pid), "ffmpeg %d outlived serve", pid)
}

func TestServe_HandlersOutliveHard_ReturnsError(t *testing.T) {
	cfg := validConfig(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: forcedTotal}

	cancel, done, pid := stuckSpeech(t, &cfg)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "still running")
	case <-time.After(cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack):
		require.Fail(t, "serve did not return within total+hard")
	}

	assert.Eventually(t, func() bool { return processGone(pid) }, probeTimeout, probeEvery, "ffmpeg %d was never killed", pid)
}

func TestServe_PortInUse_ReturnsError(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, listener.Close()) })

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	cfg := validConfig(t)
	cfg.Port = addr.Port

	_, done := startServe(t, &cfg, newService(t, http.NotFound))

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "address already in use")
	case <-time.After(probeTimeout):
		require.Fail(t, "serve did not report the listen failure")
	}
}

func postSpeech(ctx context.Context, base string) (string, error) {
	return postSpeechBody(ctx, base, `{"input":"hi","response_format":"wav"}`)
}

func postSpeechBody(ctx context.Context, base, body string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/speech", strings.NewReader(body))
	if err != nil {
		return "", err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(resp.Body)
	return string(data), errors.Join(err, resp.Body.Close())
}
