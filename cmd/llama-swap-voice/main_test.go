package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/upstream"
	"github.com/mniehe/llama-swap-voice/internal/wyoming"
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

	// heldWindow is how long a test watches serve not return while a
	// Wyoming request is held.
	heldWindow = 300 * time.Millisecond
	// unreadReplyBytes is far more than the loopback socket buffers hold for
	// a client that does not read.
	unreadReplyBytes = 8 << 20
	ipv6Loopback     = "::1"
	// loopbackHost is where tests bind the listeners they pass to serveOn.
	loopbackHost = "127.0.0.1"
	// graceWindow is the shutdown.grace of the test that serves a new
	// Wyoming connection during the grace period.
	graceWindow = time.Second
	// requestLogMessage is the msg of the Wyoming request log line.
	requestLogMessage = "request"
	// drainLogMessage is the msg of the line serve logs after
	// shutdown.grace, before it shuts the listeners down.
	drainLogMessage = "waiting for ongoing requests to finish"

	eventSynthesize = "synthesize"
	eventAudioStart = "audio-start"
	eventAudioChunk = "audio-chunk"
	eventAudioStop  = "audio-stop"
	eventDescribe   = "describe"
	eventInfo       = "info"
	eventPing       = "ping"
	eventPong       = "pong"
)

// listenLoopback binds a port on host that the test owns until it hands the
// listener to serveOn, whose servers close it.
func listenLoopback(t *testing.T, host string) net.Listener {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(host, "0"))
	require.NoError(t, err)
	closeOnCleanup(t, listener)

	return listener
}

// closeOnCleanup closes listener when the test ends unless something else
// already has.
func closeOnCleanup(t *testing.T, listener net.Listener) {
	t.Helper()

	t.Cleanup(func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing the test listener: %v", err)
		}
	})
}

func portOf(t *testing.T, listener net.Listener) int {
	t.Helper()

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	return addr.Port
}

func newService(t *testing.T, respond http.HandlerFunc) *services {
	t.Helper()

	cfg := validConfig(t)
	return serviceFor(t, &cfg, respond, &http.Client{})
}

func serviceFor(t *testing.T, cfg *ServiceConfig, respond http.HandlerFunc, httpClient *http.Client) *services {
	t.Helper()

	fake := httptest.NewServer(respond)
	t.Cleanup(fake.Close)

	client, err := upstream.New(fake.URL, "tts", "asr", httpClient)
	require.NoError(t, err)

	service, err := newServices(cfg, map[string]string{"alice": filepath.Join(cfg.Voices.Dir, "alice.mp3")}, client, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	return service
}

// unpooledClient gives each request a connection of its own: a pooled
// client can dial a spare connection that never sends a request, and
// Shutdown waits 5 s for it to go idle.
func unpooledClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
}

func get(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return 0, err
	}

	resp, err := unpooledClient().Do(req)
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

// served is a serveOn running on listeners the test bound itself.
type served struct {
	cancel context.CancelFunc
	done   <-chan error
	// base is the HTTP listener's URL.
	base string
	// wyoming is the Wyoming listener's address, empty when it is disabled.
	wyoming string
}

func startServe(t *testing.T, cfg *ServiceConfig, service *services) *served {
	t.Helper()

	return startServeLogged(t, cfg, service, slog.New(slog.DiscardHandler))
}

// startServeLogged binds the HTTP listener, and the Wyoming one when cfg
// enables it, on loopback port 0 and serves them.
func startServeLogged(t *testing.T, cfg *ServiceConfig, service *services, logger *slog.Logger) *served {
	t.Helper()

	ls := &listeners{http: listenLoopback(t, loopbackHost)}
	if cfg.Wyoming.Enabled {
		ls.wyoming = listenLoopback(t, loopbackHost)
	}

	return startServeOn(t, cfg, service, ls, logger)
}

func startServeOn(t *testing.T, cfg *ServiceConfig, service *services, ls *listeners, logger *slog.Logger) *served {
	t.Helper()

	s := &served{base: "http://" + ls.http.Addr().String()}
	if ls.wyoming != nil {
		s.wyoming = ls.wyoming.Addr().String()
	}

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- serveOn(ctx, cfg, service, ls, logger)
	}()
	s.cancel, s.done = cancel, result

	return s
}

// launchServe runs serve, which binds the addresses in cfg itself; tests
// use it only where a bind fails.
func launchServe(t *testing.T, cfg *ServiceConfig, service *services) <-chan error {
	t.Helper()

	result := make(chan error, 1)
	go func() {
		result <- serve(t.Context(), cfg, service, slog.New(slog.DiscardHandler))
	}()

	return result
}

func TestServe_Signal_ShutsDownCleanly(t *testing.T) {
	cfg := validConfig(t)

	s := startServe(t, &cfg, newService(t, http.NotFound))
	waitReady(t, s.base)

	s.cancel()

	select {
	case err := <-s.done:
		require.NoError(t, err)
	case <-time.After(probeTimeout):
		require.Fail(t, "serve did not return after the signal")
	}
}

func TestServe_Signal_DrainsInFlightRequest(t *testing.T) {
	cfg := validConfig(t)

	reached := make(chan struct{})
	release := make(chan struct{})
	service := newService(t, func(w http.ResponseWriter, _ *http.Request) {
		close(reached)
		<-release
		_, err := io.WriteString(w, slowAudio)
		assert.NoError(t, err)
	})

	s := startServe(t, &cfg, service)
	waitReady(t, s.base)

	type result struct {
		body string
		err  error
	}
	finished := make(chan result, 1)
	go func() {
		body, err := postSpeech(t.Context(), s.base)
		finished <- result{body: body, err: err}
	}()

	<-reached
	s.cancel()
	time.Sleep(probeEvery * 5)
	close(release)

	got := <-finished
	require.NoError(t, got.err)
	assert.Equal(t, slowAudio, got.body, "the in-flight stream must complete during shutdown")
	require.NoError(t, <-s.done)
}

// stuckSpeech starts a speech request through service and returns once the
// upstream reply has paused mid-body.
func stuckSpeech(t *testing.T, cfg *ServiceConfig, service *services, paused <-chan struct{}) *served {
	t.Helper()

	s := startServe(t, cfg, service)
	waitReady(t, s.base)

	go func() {
		_, err := postSpeechBody(t.Context(), s.base, `{"input":"hi","response_format":"mp3"}`)
		assert.Error(t, err, "a cancelled stream must not end cleanly")
	}()

	select {
	case <-paused:
	case <-time.After(probeTimeout):
		require.FailNow(t, "the upstream reply never started")
	}

	return s
}

func TestServe_StuckRequest_IsCancelledWithinHard(t *testing.T) {
	cfg := validConfig(t)
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

	s := stuckSpeech(t, &cfg, service, paused)
	signalled := time.Now()
	s.cancel()

	select {
	case err := <-s.done:
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
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: forcedTotal}

	paused := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	body := &stubbornBody{audio: []byte(slowAudio), paused: paused, release: release}
	service := serviceFor(t, &cfg, http.NotFound, &http.Client{Transport: stubbornTransport{body: body}})

	s := stuckSpeech(t, &cfg, service, paused)
	s.cancel()

	select {
	case err := <-s.done:
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

	silence := wavHeader + strings.Repeat("\x00", halfSecondSamples*sampleBytes)
	service := serviceFor(t, &cfg, func(w http.ResponseWriter, _ *http.Request) {
		_, writeErr := io.WriteString(w, silence)
		assert.NoError(t, writeErr)
	}, &http.Client{})

	s := startServe(t, &cfg, service)
	waitReady(t, s.base)

	pcm, err := postSpeechBody(t.Context(), s.base, `{"input":"hi","response_format":"pcm","speed":1.5}`)
	require.NoError(t, err)
	want := halfSecondSamples / stretchedSpeed
	assert.InDelta(t, want, float64(len(pcm)/sampleBytes), want*lengthTolerance)

	s.cancel()
	require.NoError(t, <-s.done)
}

func TestServe_PortInUse_ReturnsError(t *testing.T) {
	cfg := validConfig(t)
	cfg.Port = portOf(t, listenLoopback(t, loopbackHost))

	done := launchServe(t, &cfg, newService(t, http.NotFound))

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

// wyomingEnabled is a valid configuration with the Wyoming listener on.
func wyomingEnabled(t *testing.T) ServiceConfig {
	t.Helper()

	cfg := validConfig(t)
	cfg.Wyoming.Enabled = true

	return cfg
}

type wyomingClient struct {
	t    *testing.T
	conn net.Conn
	in   *bufio.Reader
}

// dialWyoming connects once serveOn reports ready.
func dialWyoming(t *testing.T, s *served) *wyomingClient {
	t.Helper()

	waitReady(t, s.base)

	return connectWyoming(t, s)
}

func connectWyoming(t *testing.T, s *served) *wyomingClient {
	t.Helper()

	conn, err := (&net.Dialer{Timeout: probeTimeout}).DialContext(t.Context(), "tcp", s.wyoming)
	require.NoError(t, err)
	t.Cleanup(func() {
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Errorf("closing the Wyoming client: %v", closeErr)
		}
	})

	return &wyomingClient{t: t, conn: conn, in: bufio.NewReader(conn)}
}

func (c *wyomingClient) send(eventType, data string) {
	c.t.Helper()

	event := &wyoming.Event{Type: eventType}
	if data != "" {
		event.Data = []byte(data)
	}
	require.NoError(c.t, c.conn.SetWriteDeadline(time.Now().Add(probeTimeout)))
	require.NoError(c.t, wyoming.WriteEvent(c.conn, event))
}

func (c *wyomingClient) read() *wyoming.Event {
	c.t.Helper()

	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(probeTimeout)))
	event, err := wyoming.ReadEvent(c.in)
	require.NoError(c.t, err)

	return event
}

func (c *wyomingClient) readType(eventType string) *wyoming.Event {
	c.t.Helper()

	event := c.read()
	require.Equal(c.t, eventType, event.Type, "event data %s", event.Data)

	return event
}

// readSpeech reads audio-start, the chunks and audio-stop, and returns the
// PCM.
func (c *wyomingClient) readSpeech() []byte {
	c.t.Helper()

	c.readType(eventAudioStart)
	var pcm []byte
	for {
		event := c.read()
		if event.Type == eventAudioStop {
			return pcm
		}
		require.Equal(c.t, eventAudioChunk, event.Type, "event data %s", event.Data)
		pcm = append(pcm, event.Payload...)
	}
}

// requireClosed reads until the service closes the connection.
func (c *wyomingClient) requireClosed() {
	c.t.Helper()

	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(probeTimeout)))
	_, err := io.Copy(io.Discard, c.in)
	var timeout net.Error
	assert.False(c.t, errors.As(err, &timeout) && timeout.Timeout(), "the Wyoming connection is still open")
}

func requireServeReturns(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(within):
		require.FailNow(t, "serve did not return", "within %s", within)
		return nil
	}
}

func TestServe_Wyoming_DrainsInFlightRequest(t *testing.T) {
	cfg := wyomingEnabled(t)
	pcm := strings.Repeat("\x01\x02", halfSecondSamples)
	reached := make(chan struct{})
	release := make(chan struct{})
	releaseUpstream := sync.OnceFunc(func() { close(release) })
	service := serviceFor(t, &cfg, func(w http.ResponseWriter, _ *http.Request) {
		close(reached)
		<-release
		_, err := io.WriteString(w, wavHeader+pcm)
		assert.NoError(t, err)
	}, &http.Client{})
	t.Cleanup(releaseUpstream)

	s := startServe(t, &cfg, service)
	c := dialWyoming(t, s)
	c.send(eventSynthesize, `{"text":"hi"}`)
	select {
	case <-reached:
	case <-time.After(probeTimeout):
		require.FailNow(t, "the synthesis never reached the upstream")
	}

	s.cancel()
	select {
	case err := <-s.done:
		require.FailNow(t, "serve returned while a Wyoming synthesis was held", "error: %v", err)
	case <-time.After(heldWindow):
	}
	releaseUpstream()

	assert.Equal(t, pcm, string(c.readSpeech()), "the held synthesis completes during shutdown")
	require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
}

// stuckSynthesis starts a Wyoming synthesis through service and returns once
// the upstream reply has paused mid-body.
func stuckSynthesis(t *testing.T, cfg *ServiceConfig, service *services, paused <-chan struct{}) *served {
	t.Helper()

	s := startServe(t, cfg, service)
	c := dialWyoming(t, s)
	c.send(eventSynthesize, `{"text":"hi"}`)

	select {
	case <-paused:
	case <-time.After(probeTimeout):
		require.FailNow(t, "the upstream reply never started")
	}

	return s
}

func TestServe_Wyoming_StuckRequest_IsCancelledWithinHard(t *testing.T) {
	cfg := wyomingEnabled(t)
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

	s := stuckSynthesis(t, &cfg, service, paused)
	signalled := time.Now()
	s.cancel()

	budget := cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack
	require.NoError(t, requireServeReturns(t, s.done, budget), "every handler returned within shutdown.hard")
	assert.Less(t, time.Since(signalled), budget)
	select {
	case <-upstreamCancelled:
	case <-time.After(probeTimeout):
		assert.Fail(t, "the upstream request outlived serve")
	}
}

func TestServe_Wyoming_HandlerOutlivesHard_ReturnsError(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: forcedTotal}

	paused := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	body := &stubbornBody{audio: []byte(slowAudio), paused: paused, release: release}
	service := serviceFor(t, &cfg, http.NotFound, &http.Client{Transport: stubbornTransport{body: body}})

	s := stuckSynthesis(t, &cfg, service, paused)
	s.cancel()

	err := requireServeReturns(t, s.done, cfg.ShutdownTiming.Total+cfg.ShutdownTiming.Hard+shutdownSlack)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still running")
}

// The reply is far larger than the socket buffers and the client never
// reads it, so the service's write blocks; cancelling the request cannot
// unblock it, but closing the connection does.
func TestServe_Wyoming_UnreadReply_IsClosedWithinHard(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: cancelledHard}
	reply := wavHeader + strings.Repeat("\x00", unreadReplyBytes)
	written := make(chan struct{})
	service := serviceFor(t, &cfg, func(w http.ResponseWriter, _ *http.Request) {
		defer close(written)
		if _, err := io.WriteString(w, reply); err != nil {
			t.Logf("fake upstream: writing speech: %v", err)
		}
	}, &http.Client{})

	s := startServe(t, &cfg, service)
	c := dialWyoming(t, s)
	c.send(eventSynthesize, `{"text":"hi"}`)
	c.readType(eventAudioStart)
	signalled := time.Now()
	s.cancel()

	budget := cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack
	require.NoError(t, requireServeReturns(t, s.done, budget))
	elapsed := time.Since(signalled)
	assert.Less(t, elapsed, budget)
	assert.GreaterOrEqual(t, elapsed, cfg.ShutdownTiming.Total, "the reply was not blocked: its request ended before shutdown.total")
	<-written
}

// gatedHandler discards every record but holds each record with message
// until open runs. held is closed when the first one arrives.
type gatedHandler struct {
	message string
	first   sync.Once
	held    chan struct{}
	gate    chan struct{}
	open    func()
}

func newGatedHandler(t *testing.T, message string) *gatedHandler {
	g := &gatedHandler{message: message, held: make(chan struct{}), gate: make(chan struct{})}
	g.open = sync.OnceFunc(func() { close(g.gate) })
	t.Cleanup(g.open)

	return g
}

func (*gatedHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

//nolint:gocritic // hugeParam: slog.Handler fixes the record's type.
func (g *gatedHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message != g.message {
		return nil
	}

	g.first.Do(func() { close(g.held) })
	<-g.gate

	return nil
}

func (g *gatedHandler) WithAttrs([]slog.Attr) slog.Handler {
	return g
}

func (g *gatedHandler) WithGroup(string) slog.Handler {
	return g
}

// The Wyoming request has left the pipeline, so Drain returns, but its
// connection goroutine is held writing the request log line; shutdown.hard
// must also bound the wait for that goroutine.
func TestServe_Wyoming_GoroutineOutlivesHard_ReturnsError(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: cancelledHard}
	reached := make(chan struct{})
	service := serviceFor(t, &cfg, func(_ http.ResponseWriter, r *http.Request) {
		// The server notices a dropped client only once the body is read.
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		close(reached)
		<-r.Context().Done()
	}, &http.Client{})
	logs := newGatedHandler(t, requestLogMessage)

	s := startServeLogged(t, &cfg, service, slog.New(logs))
	c := dialWyoming(t, s)
	c.send(eventSynthesize, `{"text":"hi"}`)
	select {
	case <-reached:
	case <-time.After(probeTimeout):
		require.FailNow(t, "the synthesis never reached the upstream")
	}
	s.cancel()

	err := requireServeReturns(t, s.done, cfg.ShutdownTiming.Total+cfg.ShutdownTiming.Hard+shutdownSlack)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still running")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case <-logs.held:
	default:
		require.FailNow(t, "the request log line was never written")
	}
	assert.NoError(t, service.pipeline.Drain(t.Context()), "the request still holds its pipeline admission")
}

// stoppedListener fails every Accept with err.
type stoppedListener struct {
	err error
}

func (l stoppedListener) Accept() (net.Conn, error) {
	return nil, l.err
}

func (stoppedListener) Close() error {
	return nil
}

func (stoppedListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

func TestServeWyoming_AcceptError_IsReturnedWrapped(t *testing.T) {
	cfg := validConfig(t)
	service := newService(t, http.NotFound)
	wyomingCfg := cfg.wyomingConfig(Version)
	server := wyoming.New(t.Context(), &wyomingCfg, service.pipeline, slog.New(slog.DiscardHandler))
	acceptErr := errors.New("accept broke")

	err := serveWyoming(server, stoppedListener{err: acceptErr})

	require.ErrorIs(t, err, acceptErr)
	assert.True(t, strings.HasPrefix(err.Error(), "wyoming server: wyoming accept"), "error %q", err)
}

// When one server's Serve fails, serveOn returns before the other server's
// goroutine may have registered its listener; that listener must still be
// closed when serveOn returns.
func TestServeOn_AcceptFails_ClosesThePeerListener(t *testing.T) {
	acceptErr := errors.New("accept broke")
	tests := map[string]func(peer net.Listener) *listeners{
		"http fails": func(peer net.Listener) *listeners {
			return &listeners{http: stoppedListener{err: acceptErr}, wyoming: peer}
		},
		"wyoming fails": func(peer net.Listener) *listeners {
			return &listeners{http: peer, wyoming: stoppedListener{err: acceptErr}}
		},
	}

	for name, handed := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := wyomingEnabled(t)
			peer := listenLoopback(t, loopbackHost)

			err := serveOn(t.Context(), &cfg, newService(t, http.NotFound), handed(peer), slog.New(slog.DiscardHandler))

			require.ErrorIs(t, err, acceptErr)
			assert.ErrorIs(t, peer.Close(), net.ErrClosed, "serveOn returned with the peer listener open")
		})
	}
}

// During shutdown.grace the Wyoming listener still accepts connections and
// serves them, as the HTTP listener does. The log line that ends the grace
// period is held, so the listeners stay as the grace period left them until
// the test has checked them.
func TestServe_Wyoming_GraceKeepsServingNewConnections(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.ShutdownTiming.Grace = graceWindow
	logs := newGatedHandler(t, drainLogMessage)
	s := startServeLogged(t, &cfg, newService(t, http.NotFound), slog.New(logs))
	waitReady(t, s.base)

	signalled := time.Now()
	s.cancel()
	select {
	case <-logs.held:
	case <-time.After(cfg.ShutdownTiming.Grace + probeTimeout):
		require.FailNow(t, "the grace period never ended")
	}
	assert.GreaterOrEqual(t, time.Since(signalled), cfg.ShutdownTiming.Grace, "the listeners were shut down before shutdown.grace ended")
	status, err := get(t.Context(), s.base+"/internal/readiness")
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, status, "the readiness probe did not report the shutdown")
	c := connectWyoming(t, s)
	c.send(eventDescribe, "")
	c.readType(eventInfo)

	select {
	case err = <-s.done:
		require.FailNow(t, "serve returned during shutdown.grace", "error: %v", err)
	default:
	}
	logs.open()
	require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
	c.requireClosed()
}

func TestServe_Wyoming_SilentRecording_DoesNotDelayShutdown(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: cancelledHard}

	s := startServe(t, &cfg, newService(t, http.NotFound))
	c := dialWyoming(t, s)
	c.send(eventAudioStart, `{"rate":16000,"width":2,"channels":1}`)
	c.send(eventPing, "")
	c.readType(eventPong)
	signalled := time.Now()
	s.cancel()

	budget := cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack
	require.NoError(t, requireServeReturns(t, s.done, budget))
	assert.Less(t, time.Since(signalled), budget)
	c.requireClosed()
}

// listenRecorder binds as serve does and records every address it was asked
// for and every listener it opened.
type listenRecorder struct {
	addrs []string
	// ctxErrs holds each call's ctx.Err() at the time of the call.
	ctxErrs   []error
	listeners []net.Listener
}

func (r *listenRecorder) listen(ctx context.Context, network, addr string) (net.Listener, error) {
	r.addrs = append(r.addrs, addr)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	listener, err := (&net.ListenConfig{}).Listen(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	r.listeners = append(r.listeners, listener)

	return listener, nil
}

// A stop signal while the HTTP address resolves must end in the grace
// period, not in a failed start, so the HTTP bind ignores cancellation.
func TestBind_CancelledContext_BindsHTTP(t *testing.T) {
	cfg := validConfig(t)
	cfg.Host = loopbackHost
	cfg.Port = 0
	recorder := &listenRecorder{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	ls, err := bind(ctx, &cfg, slog.New(slog.DiscardHandler), recorder.listen)

	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ls.http.Close()) })
	assert.Equal(t, []error{nil}, recorder.ctxErrs, "the HTTP bind was given a cancelled context")
}

func TestBind_WyomingDisabled_OpensNoPort(t *testing.T) {
	cfg := validConfig(t)
	cfg.Host = loopbackHost
	cfg.Port = 0
	recorder := &listenRecorder{}

	ls, err := bind(t.Context(), &cfg, slog.New(slog.DiscardHandler), recorder.listen)

	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ls.http.Close()) })
	assert.Nil(t, ls.wyoming, "a Wyoming listener was opened while it is disabled")
	assert.Equal(t, []string{net.JoinHostPort(loopbackHost, "0")}, recorder.addrs, "bind must open the HTTP listener alone")
}

func TestServe_WyomingPortInUse_ReturnsError(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.Host = loopbackHost
	cfg.Port = portOf(t, listenLoopback(t, loopbackHost))
	cfg.Wyoming.Host = loopbackHost
	cfg.Wyoming.Port = portOf(t, listenLoopback(t, loopbackHost))
	recorder := &listenRecorder{}

	ls, err := bind(t.Context(), &cfg, slog.New(slog.DiscardHandler), recorder.listen)

	require.Error(t, err)
	assert.Nil(t, ls)
	assert.Equal(t, []string{net.JoinHostPort(cfg.Wyoming.Host, strconv.Itoa(cfg.Wyoming.Port))}, recorder.addrs,
		"bind tried the HTTP port after the Wyoming bind failed")

	err = requireServeReturns(t, launchServe(t, &cfg, newService(t, http.NotFound)), probeTimeout)
	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "wyoming listener: "), "error %q", err)
	assert.Contains(t, err.Error(), "address already in use")
}

func TestBind_HTTPPortInUse_ClosesTheWyomingListener(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.Host = loopbackHost
	cfg.Port = portOf(t, listenLoopback(t, loopbackHost))
	cfg.Wyoming.Host = loopbackHost
	cfg.Wyoming.Port = 0
	recorder := &listenRecorder{}

	ls, err := bind(t.Context(), &cfg, slog.New(slog.DiscardHandler), recorder.listen)

	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "http listener: "), "error %q", err)
	assert.Contains(t, err.Error(), "address already in use")
	assert.Nil(t, ls)
	require.Len(t, recorder.listeners, 1, "only the Wyoming bind succeeds")
	assert.ErrorIs(t, recorder.listeners[0].Close(), net.ErrClosed, "bind returned with the Wyoming listener open")
}

func TestServe_HTTPPortInUse_ReturnsError(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.Host = loopbackHost
	cfg.Port = portOf(t, listenLoopback(t, loopbackHost))
	cfg.Wyoming.Host = loopbackHost
	cfg.Wyoming.Port = 0

	err := requireServeReturns(t, launchServe(t, &cfg, newService(t, http.NotFound)), probeTimeout)

	require.Error(t, err)
	assert.True(t, strings.HasPrefix(err.Error(), "http listener: "), "error %q", err)
	assert.Contains(t, err.Error(), "address already in use")
}

func TestServe_Wyoming_IPv6Loopback_Binds(t *testing.T) {
	probe, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(ipv6Loopback, "0"))
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	require.NoError(t, probe.Close())

	cfg := wyomingEnabled(t)
	cfg.Wyoming.Host = ipv6Loopback
	require.NoError(t, cfg.Validate())
	cfg.Host = loopbackHost
	cfg.Port = 0
	cfg.Wyoming.Port = 0

	ls, err := bind(t.Context(), &cfg, slog.New(slog.DiscardHandler), (&net.ListenConfig{}).Listen)
	require.NoError(t, err)
	closeOnCleanup(t, ls.http)
	closeOnCleanup(t, ls.wyoming)
	wyomingAddr, ok := ls.wyoming.Addr().(*net.TCPAddr)
	require.True(t, ok)
	assert.True(t, wyomingAddr.IP.Equal(net.IPv6loopback), "the Wyoming listener bound %s", wyomingAddr)
	httpAddr, ok := ls.http.Addr().(*net.TCPAddr)
	require.True(t, ok)
	assert.True(t, httpAddr.IP.Equal(net.ParseIP(loopbackHost)), "the HTTP listener bound %s", httpAddr)

	s := startServeOn(t, &cfg, newService(t, http.NotFound), ls, slog.New(slog.DiscardHandler))
	c := dialWyoming(t, s)
	c.send(eventDescribe, "")
	c.readType(eventInfo)

	s.cancel()
	require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
}

func TestServeWith_BindsAndServes(t *testing.T) {
	tests := map[string]bool{"wyoming disabled": false, "wyoming enabled": true}

	for name, wyomingOn := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Host = loopbackHost
			cfg.Port = 0
			cfg.Wyoming.Enabled = wyomingOn
			cfg.Wyoming.Host = loopbackHost
			cfg.Wyoming.Port = 0
			bound := make(chan net.Listener, 2)
			listen := func(ctx context.Context, network, addr string) (net.Listener, error) {
				listener, err := (&net.ListenConfig{}).Listen(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				bound <- listener

				return listener, nil
			}

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- serveWith(ctx, &cfg, newService(t, http.NotFound), slog.New(slog.DiscardHandler), listen)
			}()

			s := &served{cancel: cancel, done: done}
			if wyomingOn {
				s.wyoming = nextListener(t, bound).Addr().String()
			}
			s.base = "http://" + nextListener(t, bound).Addr().String()
			waitReady(t, s.base)
			if wyomingOn {
				c := connectWyoming(t, s)
				c.send(eventDescribe, "")
				c.readType(eventInfo)
			}

			s.cancel()
			require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
		})
	}
}

// nextListener returns the next listener serveWith's listen function opened;
// bind opens Wyoming before HTTP.
func nextListener(t *testing.T, bound <-chan net.Listener) net.Listener {
	t.Helper()

	select {
	case listener := <-bound:
		return listener
	case <-time.After(probeTimeout):
		require.FailNow(t, "serveWith did not bind")
		return nil
	}
}

const (
	recordedTranscript = "turn on the porch light"
	eventTranscribe    = "transcribe"
	eventTranscript    = "transcript"
	homeAssistantAudio = `{"rate":16000,"width":2,"channels":1}`
)

// serveASR answers every upstream call with recordedTranscript as an OpenAI
// transcription.
func serveASR(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, `{"text":"`+recordedTranscript+`"}`); err != nil {
		panic(err)
	}
}

func recordingsOn(t *testing.T, cfg *ServiceConfig) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "recordings")
	cfg.Recordings = RecordingsConfig{Enable: true, Dir: dir}

	return dir
}

// savedPair returns the two file names of the one recording saved in dir,
// or nil until its metadata is in place. The store writes the audio first and
// renames each file into place from a temporary name, so a final .json beside
// its .opus is a whole recording.
func savedPair(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	present := map[string]bool{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".tmp") {
			present[entry.Name()] = true
		}
	}

	for name := range present {
		id, ok := strings.CutSuffix(name, ".json")
		if ok && present[id+".opus"] {
			return []string{id + ".json", id + ".opus"}
		}
	}

	return nil
}

// waitForSaved waits until the background worker has saved one recording
// in dir and returns its two file names, the .json first.
func waitForSaved(t *testing.T, dir string) []string {
	t.Helper()

	var files []string
	require.Eventually(t, func() bool {
		files = savedPair(t, dir)
		return files != nil
	}, probeTimeout, 10*time.Millisecond, "the recording was not saved")

	return files
}

func readSaved(t *testing.T, dir, name string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(data, &meta))

	return meta
}

func postTranscription(t *testing.T, base string) string {
	t.Helper()

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "clip.wav")
	require.NoError(t, err)
	_, err = part.Write([]byte(wavHeader + strings.Repeat("\x01\x02", halfSecondSamples)))
	require.NoError(t, err)
	require.NoError(t, form.Close())

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/audio/transcriptions", &body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", form.FormDataContentType())
	resp, err := unpooledClient().Do(req)
	require.NoError(t, err)
	defer func() { assert.NoError(t, resp.Body.Close()) }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body %s", data)

	return string(data)
}

func (c *wyomingClient) sendChunk(data string, payload []byte) {
	c.t.Helper()

	require.NoError(c.t, c.conn.SetWriteDeadline(time.Now().Add(probeTimeout)))
	require.NoError(c.t, wyoming.WriteEvent(c.conn, &wyoming.Event{Type: eventAudioChunk, Data: []byte(data), Payload: payload}))
}

func TestServe_RecordingsOff_WritesNothing(t *testing.T) {
	cfg := validConfig(t)
	dir := filepath.Join(t.TempDir(), "recordings")
	cfg.Recordings = RecordingsConfig{Enable: false, Dir: dir}
	s := startServe(t, &cfg, serviceFor(t, &cfg, serveASR, &http.Client{}))
	waitReady(t, s.base)

	assert.Contains(t, postTranscription(t, s.base), recordedTranscript)

	assert.NoDirExists(t, dir)
	status, err := get(t.Context(), s.base+"/recordings")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, status)
	s.cancel()
	require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
}

func TestServe_RecordingsOn_SavesHTTPTranscription(t *testing.T) {
	cfg := validConfig(t)
	dir := recordingsOn(t, &cfg)
	s := startServe(t, &cfg, serviceFor(t, &cfg, serveASR, &http.Client{}))
	waitReady(t, s.base)

	postTranscription(t, s.base)

	files := waitForSaved(t, dir)
	id := strings.TrimSuffix(files[0], ".json")
	assert.Equal(t, []string{id + ".json", id + ".opus"}, files)
	meta := readSaved(t, dir, files[0])
	assert.Equal(t, "openai", meta["source"])
	assert.Equal(t, recordedTranscript, meta["transcript"])
	assert.Equal(t, false, meta["keep"])
	assert.InDelta(t, 500, meta["durationMs"], 1)
	atText, ok := meta["at"].(string)
	require.True(t, ok, "at is %T", meta["at"])
	at, err := time.Parse(time.RFC3339, atText)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), at, time.Minute)

	status, err := get(t.Context(), s.base+"/recordings")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	s.cancel()
	require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
}

func TestServe_RecordingsOn_SavesWyomingTranscriptionAsWyoming(t *testing.T) {
	cfg := wyomingEnabled(t)
	dir := recordingsOn(t, &cfg)
	s := startServe(t, &cfg, serviceFor(t, &cfg, serveASR, &http.Client{}))
	c := dialWyoming(t, s)

	c.send(eventTranscribe, `{"language":"en"}`)
	c.send(eventAudioStart, homeAssistantAudio)
	c.sendChunk(homeAssistantAudio, []byte(strings.Repeat("\x01\x02", halfSecondSamples)))
	c.send(eventAudioStop, "")
	transcript := c.readType(eventTranscript)

	assert.Contains(t, string(transcript.Data), recordedTranscript)
	files := waitForSaved(t, dir)
	meta := readSaved(t, dir, files[0])
	assert.Equal(t, "wyoming", meta["source"])
	assert.Equal(t, recordedTranscript, meta["transcript"])
	s.cancel()
	require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
}

// statusAs is the status of a GET of url sent with Host set to host.
func statusAs(t *testing.T, url, host string) int {
	t.Helper()

	client := unpooledClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	req.Host = host
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, resp.Body)
	assert.NoError(t, err)
	assert.NoError(t, resp.Body.Close())

	return resp.StatusCode
}

func TestServe_RecordingsOn_AnswersOnlyAllowedHosts(t *testing.T) {
	cfg := validConfig(t)
	recordingsOn(t, &cfg)
	cfg.Recordings.AllowedHosts = "voice.example"
	// The listener still binds loopback; only the allow-list sees this host.
	cfg.Host = "listen.example"
	s := startServe(t, &cfg, serviceFor(t, &cfg, serveASR, &http.Client{}))
	waitReady(t, s.base)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(s.base, "http://"))
	require.NoError(t, err)

	assert.Equal(t, http.StatusOK, statusAs(t, s.base+"/recordings", net.JoinHostPort("listen.example", port)), "the listen host")
	assert.Equal(t, http.StatusOK, statusAs(t, s.base+"/recordings", "voice.example"))
	assert.Equal(t, http.StatusOK, statusAs(t, s.base+"/recordings", "voice.example:443"))
	assert.Equal(t, http.StatusOK, statusAs(t, s.base+"/recordings", strings.TrimPrefix(s.base, "http://")))
	assert.Equal(t, http.StatusForbidden, statusAs(t, s.base+"/recordings", "evil.example"))
	s.cancel()
	require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
}

func TestNewServices_RecordingsDirUnwritable_FailsWrapped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	parent := t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o500))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(parent, 0o700)) })
	cfg := validConfig(t)
	cfg.Recordings = RecordingsConfig{Enable: true, Dir: filepath.Join(parent, "recordings")}
	client, err := upstream.New("http://127.0.0.1:1", "tts", "asr", &http.Client{})
	require.NoError(t, err)

	_, err = newServices(&cfg, map[string]string{}, client, slog.New(slog.DiscardHandler))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "recordings")
	assert.ErrorIs(t, err, os.ErrPermission)
}

func TestNewServices_RecordingsOn_CreatesPrivateDir(t *testing.T) {
	cfg := validConfig(t)
	dir := recordingsOn(t, &cfg)
	client, err := upstream.New("http://127.0.0.1:1", "tts", "asr", &http.Client{})
	require.NoError(t, err)

	service, err := newServices(&cfg, map[string]string{}, client, slog.New(slog.DiscardHandler))

	require.NoError(t, err)
	require.NotNil(t, service.recordings)
	require.NotNil(t, service.runRecordings)
	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

// stopWaitCheck is how long a test watches a stop that must not return
// yet.
const stopWaitCheck = 200 * time.Millisecond

// syncBuffer is a log destination that tests read while the service writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func TestServe_StartsTheRecordingsRunAndWaitsForIt(t *testing.T) {
	cfg := validConfig(t)
	service := serviceFor(t, &cfg, serveASR, &http.Client{})
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	service.runRecordings = func(ctx, _ context.Context) {
		close(started)
		<-ctx.Done()
		<-release
		close(returned)
	}
	s := startServe(t, &cfg, service)
	waitReady(t, s.base)

	select {
	case <-started:
	case <-time.After(probeTimeout):
		require.FailNow(t, "serve did not start the recordings run")
	}

	s.cancel()
	select {
	case err := <-s.done:
		require.FailNow(t, "serve returned while the recordings run was still running", "error %v", err)
	case <-time.After(stopWaitCheck):
	}
	close(release)
	require.NoError(t, requireServeReturns(t, s.done, probeTimeout))
	select {
	case <-returned:
	default:
		require.FailNow(t, "serve returned before the recordings run did")
	}
}

// synctestDeadline is how far ahead the startRecordings tests set the
// shutdown deadline; time in a synctest bubble moves only when every
// goroutine in it is blocked, so its length costs nothing.
const synctestDeadline = time.Minute

func TestStartRecordings_StopWaitsForTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release, returned := make(chan struct{}), make(chan struct{})
		stop := startRecordings(func(ctx, _ context.Context) {
			<-ctx.Done()
			<-release
			close(returned)
		}, slog.New(slog.DiscardHandler))

		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			stop(time.Now().Add(synctestDeadline))
		}()

		synctest.Wait()
		select {
		case <-stopped:
			require.FailNow(t, "stop returned while the run was still running")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case <-stopped:
		default:
			require.FailNow(t, "stop did not return after the run did")
		}
		select {
		case <-returned:
		default:
			require.FailNow(t, "stop returned before the run did")
		}
	})
}

func TestStartRecordings_DrainEndsAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		drained := make(chan time.Time, 1)
		stop := startRecordings(func(ctx, drain context.Context) {
			<-ctx.Done()
			<-drain.Done()
			drained <- time.Now()
		}, slog.New(slog.DiscardHandler))
		deadline := time.Now().Add(synctestDeadline)

		stop(deadline)

		assert.Equal(t, deadline, <-drained, "the drain ends at the deadline, not before")
	})
}

func TestStartRecordings_StopGivesUpAtTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logs := &syncBuffer{}
		block := make(chan struct{})
		defer close(block)
		stop := startRecordings(func(context.Context, context.Context) { <-block }, slog.New(slog.NewJSONHandler(logs, nil)))

		began := time.Now()
		stop(began.Add(synctestDeadline))

		assert.Equal(t, synctestDeadline, time.Since(began), "stop returns at the deadline")
		assert.Contains(t, logs.String(), "recordings still saving at the shutdown deadline")
	})
}

const (
	// drainTotal and drainHard are the shutdown budget of the drain test:
	// Total is long enough that a deadline counted from the end of
	// shutdown, Total later, misses the slack.
	drainTotal = time.Second
	drainHard  = 300 * time.Millisecond
)

func TestServe_RecordingsDrainDeadlineRunsFromShutdownStart(t *testing.T) {
	cfg := validConfig(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: drainTotal, Hard: drainHard}
	paused := make(chan struct{})
	service := serviceFor(t, &cfg, func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, slowAudio)
		assert.NoError(t, err)
		assert.NoError(t, http.NewResponseController(w).Flush())
		close(paused)
		<-r.Context().Done()
	}, &http.Client{})
	drainEnded := make(chan time.Time, 1)
	service.runRecordings = func(ctx, drain context.Context) {
		<-ctx.Done()
		<-drain.Done()
		drainEnded <- time.Now()
	}
	// The stuck request holds shutdown for all of Total, so shutdown ends
	// Total after it starts.
	s := stuckSpeech(t, &cfg, service, paused)
	signalled := time.Now()
	s.cancel()

	budget := drainTotal + drainHard
	require.NoError(t, requireServeReturns(t, s.done, budget+probeTimeout))
	ended := <-drainEnded

	assert.GreaterOrEqual(t, ended.Sub(signalled), budget, "the drain lasts the whole budget")
	assert.Less(t, ended.Sub(signalled), budget+shutdownSlack, "the budget counts from the start of shutdown, not its end")
}

func TestStartRecordings_RecordingsOffIsNoOp(t *testing.T) {
	startRecordings(nil, slog.New(slog.DiscardHandler))(time.Now())
}

func TestRecordingsDeadline_IsTheShutdownBudgetFromItsStart(t *testing.T) {
	timing := ShutdownTimingConfig{Total: 15 * time.Second, Grace: time.Hour, Hard: 3 * time.Second}
	began := time.Date(2026, time.November, 25, 15, 30, 0, 0, time.UTC)

	assert.Equal(t, began.Add(18*time.Second), recordingsDeadline(began, &timing))
	assert.WithinDuration(t, time.Now().Add(18*time.Second), recordingsDeadline(time.Time{}, &timing), time.Second, "an unset start is now")
}
