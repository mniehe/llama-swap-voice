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
	wavHeader    = "RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\xc0\x5d\x00\x00\x80\xbb\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xff"
	slowAudio    = wavHeader + "slow audio"

	forcedTotal   = 200 * time.Millisecond
	cancelledHard = time.Second
	shutdownSlack = 500 * time.Millisecond

	sampleBytes       = 2
	halfSecondSamples = 12000
	stretchedSpeed    = 1.5
	lengthTolerance   = 0.02
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

	cfg := validConfig(t)
	return serviceFor(t, &cfg, respond, &http.Client{})
}

func serviceFor(t *testing.T, cfg *ServiceConfig, respond http.HandlerFunc, httpClient *http.Client) *server.Server {
	t.Helper()

	fake := httptest.NewServer(respond)
	t.Cleanup(fake.Close)

	client, err := upstream.New(fake.URL, "tts", "asr", httpClient)
	require.NoError(t, err)

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

// stuckSpeech starts a speech request through service and returns once the
// upstream reply has paused mid-body.
func stuckSpeech(t *testing.T, cfg *ServiceConfig, service *server.Server, paused <-chan struct{}) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	base := "http://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	cancel, done = startServe(t, cfg, service)
	waitReady(t, base)

	go func() {
		_, err := postSpeechBody(t.Context(), base, `{"input":"hi","response_format":"mp3"}`)
		assert.Error(t, err, "a cancelled stream must not end cleanly")
	}()

	select {
	case <-paused:
	case <-time.After(probeTimeout):
		require.FailNow(t, "the upstream reply never started")
	}

	return cancel, done
}

func TestServe_StuckRequest_IsCancelledWithinHard(t *testing.T) {
	cfg := validConfig(t)
	cfg.Port = freePort(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: cancelledHard}

	paused := make(chan struct{})
	upstreamCancelled := make(chan struct{})
	service := serviceFor(t, &cfg, func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, slowAudio)
		assert.NoError(t, err)
		assert.NoError(t, http.NewResponseController(w).Flush())
		close(paused)
		<-r.Context().Done()
		close(upstreamCancelled)
	}, &http.Client{})

	cancel, done := stuckSpeech(t, &cfg, service, paused)
	signalled := time.Now()
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "every handler returned within shutdown.hard")
	case <-time.After(cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack):
		require.Fail(t, "serve did not return within total+hard")
	}

	assert.Less(t, time.Since(signalled), cfg.ShutdownTiming.Total+cfg.ShutdownTiming.Hard+shutdownSlack)
	select {
	case <-upstreamCancelled:
	case <-time.After(probeTimeout):
		assert.Fail(t, "the upstream request outlived serve")
	}
}

// stubbornBody sends audio, then ignores cancellation until release closes,
// standing in for an upstream read that will not return.
type stubbornBody struct {
	audio   []byte
	paused  chan<- struct{}
	release <-chan struct{}
}

func (b *stubbornBody) Read(p []byte) (int, error) {
	if len(b.audio) > 0 {
		n := copy(p, b.audio)
		b.audio = b.audio[n:]
		return n, nil
	}

	if b.paused != nil {
		close(b.paused)
		b.paused = nil
	}
	<-b.release

	return 0, io.EOF
}

func (*stubbornBody) Close() error {
	return nil
}

type stubbornTransport struct {
	body *stubbornBody
}

func (s stubbornTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return nil, err
	}

	if err := r.Body.Close(); err != nil {
		return nil, err
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"audio/wav"}},
		Body:       s.body,
		Request:    r,
	}, nil
}

func TestServe_HandlersOutliveHard_ReturnsError(t *testing.T) {
	cfg := validConfig(t)
	cfg.Port = freePort(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: forcedTotal}

	paused := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	body := &stubbornBody{audio: []byte(slowAudio), paused: paused, release: release}
	service := serviceFor(t, &cfg, http.NotFound, &http.Client{Transport: stubbornTransport{body: body}})

	cancel, done := stuckSpeech(t, &cfg, service, paused)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.Contains(t, err.Error(), "still running")
	case <-time.After(cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack):
		require.Fail(t, "serve did not return within total+hard")
	}
}

func TestServe_EmptyPATH_StartsAndStretchesSpeech(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(envPrefix+"VOICES_DIR", voicesDir(t, "alice.mp3"))
	t.Setenv(envPrefix+"VOICES_DEFAULT", "alice")

	cfg, err := loadConfig()
	require.NoError(t, err, "nothing on PATH must be needed to start")
	cfg.Port = freePort(t)
	base := "http://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))

	silence := wavHeader + strings.Repeat("\x00", halfSecondSamples*sampleBytes)
	service := serviceFor(t, &cfg, func(w http.ResponseWriter, _ *http.Request) {
		_, writeErr := io.WriteString(w, silence)
		assert.NoError(t, writeErr)
	}, &http.Client{})

	cancel, done := startServe(t, &cfg, service)
	waitReady(t, base)

	pcm, err := postSpeechBody(t.Context(), base, `{"input":"hi","response_format":"pcm","speed":1.5}`)
	require.NoError(t, err)
	want := halfSecondSamples / stretchedSpeed
	assert.InDelta(t, want, float64(len(pcm)/sampleBytes), want*lengthTolerance)

	cancel()
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
