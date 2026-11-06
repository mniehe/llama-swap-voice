package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/server"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	probeTimeout = 3 * time.Second
	probeEvery   = 10 * time.Millisecond
	slowAudio    = "RIFF....WAVEslow audio"
)

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
	body := strings.NewReader(`{"input":"hi","response_format":"wav"}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/speech", body)
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
