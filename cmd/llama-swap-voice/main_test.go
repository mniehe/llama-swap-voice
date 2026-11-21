package main

import (
	"bufio"
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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/openai"
	"github.com/mniehe/llama-swap-voice/internal/pipeline"
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

func freePort(t *testing.T) int {
	t.Helper()

	return freePortOn(t, "127.0.0.1")
}

func freePortOn(t *testing.T, host string) int {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(host, "0"))
	require.NoError(t, err)

	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	require.NoError(t, listener.Close())

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

	serverCfg := cfg.serverConfig(map[string]string{"alice": filepath.Join(cfg.Voices.Dir, "alice.mp3")})
	pipe, err := pipeline.New(&serverCfg, client, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	return &services{api: openai.New(pipe, slog.New(slog.DiscardHandler)), pipeline: pipe}
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

func startServe(t *testing.T, cfg *ServiceConfig, service *services) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	return startServeLogged(t, cfg, service, slog.New(slog.DiscardHandler))
}

func startServeLogged(t *testing.T, cfg *ServiceConfig, service *services, logger *slog.Logger) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- serve(ctx, cfg, service, logger)
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
func stuckSpeech(t *testing.T, cfg *ServiceConfig, service *services, paused <-chan struct{}) (cancel context.CancelFunc, done <-chan error) {
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

// wyomingEnabled is a valid configuration on free ports with the Wyoming
// listener on.
func wyomingEnabled(t *testing.T) ServiceConfig {
	t.Helper()

	cfg := validConfig(t)
	cfg.Port = freePort(t)
	cfg.Wyoming.Enabled = true
	cfg.Wyoming.Port = freePort(t)

	return cfg
}

func httpBase(cfg *ServiceConfig) string {
	return "http://" + net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
}

func wyomingAddr(cfg *ServiceConfig) string {
	return net.JoinHostPort(cfg.Wyoming.Host, strconv.Itoa(cfg.Wyoming.Port))
}

type wyomingClient struct {
	t    *testing.T
	conn net.Conn
	in   *bufio.Reader
}

// dialWyoming connects once serve reports ready, which it does only after
// the Wyoming port is bound.
func dialWyoming(t *testing.T, cfg *ServiceConfig) *wyomingClient {
	t.Helper()

	waitReady(t, httpBase(cfg))

	return connectWyoming(t, cfg)
}

func connectWyoming(t *testing.T, cfg *ServiceConfig) *wyomingClient {
	t.Helper()

	conn, err := (&net.Dialer{Timeout: probeTimeout}).DialContext(t.Context(), "tcp", wyomingAddr(cfg))
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

	cancel, done := startServe(t, &cfg, service)
	c := dialWyoming(t, &cfg)
	c.send(eventSynthesize, `{"text":"hi"}`)
	select {
	case <-reached:
	case <-time.After(probeTimeout):
		require.FailNow(t, "the synthesis never reached the upstream")
	}

	cancel()
	select {
	case err := <-done:
		require.FailNow(t, "serve returned while a Wyoming synthesis was held", "error: %v", err)
	case <-time.After(heldWindow):
	}
	releaseUpstream()

	assert.Equal(t, pcm, string(c.readSpeech()), "the held synthesis completes during shutdown")
	require.NoError(t, requireServeReturns(t, done, probeTimeout))
}

// stuckSynthesis starts a Wyoming synthesis through service and returns once
// the upstream reply has paused mid-body.
func stuckSynthesis(t *testing.T, cfg *ServiceConfig, service *services, paused <-chan struct{}) (cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	cancel, done = startServe(t, cfg, service)
	c := dialWyoming(t, cfg)
	c.send(eventSynthesize, `{"text":"hi"}`)

	select {
	case <-paused:
	case <-time.After(probeTimeout):
		require.FailNow(t, "the upstream reply never started")
	}

	return cancel, done
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

	cancel, done := stuckSynthesis(t, &cfg, service, paused)
	signalled := time.Now()
	cancel()

	budget := cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack
	require.NoError(t, requireServeReturns(t, done, budget), "every handler returned within shutdown.hard")
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

	cancel, done := stuckSynthesis(t, &cfg, service, paused)
	cancel()

	err := requireServeReturns(t, done, cfg.ShutdownTiming.Total+cfg.ShutdownTiming.Hard+shutdownSlack)
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

	cancel, done := startServe(t, &cfg, service)
	c := dialWyoming(t, &cfg)
	c.send(eventSynthesize, `{"text":"hi"}`)
	c.readType(eventAudioStart)
	signalled := time.Now()
	cancel()

	budget := cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack
	require.NoError(t, requireServeReturns(t, done, budget))
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

	cancel, done := startServeLogged(t, &cfg, service, slog.New(logs))
	c := dialWyoming(t, &cfg)
	c.send(eventSynthesize, `{"text":"hi"}`)
	select {
	case <-reached:
	case <-time.After(probeTimeout):
		require.FailNow(t, "the synthesis never reached the upstream")
	}
	cancel()

	err := requireServeReturns(t, done, cfg.ShutdownTiming.Total+cfg.ShutdownTiming.Hard+shutdownSlack)
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

// During shutdown.grace the Wyoming listener still accepts connections and
// serves them, as the HTTP listener does. The log line that ends the grace
// period is held, so the listeners stay as the grace period left them until
// the test has checked them.
func TestServe_Wyoming_GraceKeepsServingNewConnections(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.ShutdownTiming.Grace = graceWindow
	logs := newGatedHandler(t, drainLogMessage)
	cancel, done := startServeLogged(t, &cfg, newService(t, http.NotFound), slog.New(logs))
	waitReady(t, httpBase(&cfg))

	cancel()
	select {
	case <-logs.held:
	case <-time.After(cfg.ShutdownTiming.Grace + probeTimeout):
		require.FailNow(t, "the grace period never ended")
	}
	status, err := get(t.Context(), httpBase(&cfg)+"/internal/readiness")
	require.NoError(t, err)
	assert.Equal(t, http.StatusServiceUnavailable, status, "the readiness probe did not report the shutdown")
	c := connectWyoming(t, &cfg)
	c.send(eventDescribe, "")
	c.readType(eventInfo)

	select {
	case err = <-done:
		require.FailNow(t, "serve returned during shutdown.grace", "error: %v", err)
	default:
	}
	logs.open()
	require.NoError(t, requireServeReturns(t, done, probeTimeout))
	c.requireClosed()
}

func TestServe_Wyoming_SilentRecording_DoesNotDelayShutdown(t *testing.T) {
	cfg := wyomingEnabled(t)
	cfg.ShutdownTiming = ShutdownTimingConfig{Total: forcedTotal, Hard: cancelledHard}

	cancel, done := startServe(t, &cfg, newService(t, http.NotFound))
	c := dialWyoming(t, &cfg)
	c.send(eventAudioStart, `{"rate":16000,"width":2,"channels":1}`)
	c.send(eventPing, "")
	c.readType(eventPong)
	signalled := time.Now()
	cancel()

	budget := cfg.ShutdownTiming.Total + cfg.ShutdownTiming.Hard + shutdownSlack
	require.NoError(t, requireServeReturns(t, done, budget))
	assert.Less(t, time.Since(signalled), budget)
	c.requireClosed()
}

func TestServe_WyomingDisabled_OpensNoPort(t *testing.T) {
	cfg := validConfig(t)
	cfg.Port = freePort(t)
	cfg.Wyoming.Port = freePort(t)

	cancel, done := startServe(t, &cfg, newService(t, http.NotFound))
	waitReady(t, httpBase(&cfg))

	conn, err := (&net.Dialer{Timeout: probeTimeout}).DialContext(t.Context(), "tcp", wyomingAddr(&cfg))
	if err == nil {
		assert.NoError(t, conn.Close())
	}
	require.Error(t, err, "the Wyoming port accepts connections while the listener is disabled")
	cancel()
	require.NoError(t, requireServeReturns(t, done, probeTimeout))
}

func TestServe_WyomingPortInUse_ReturnsError(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, listener.Close()) })
	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	cfg := wyomingEnabled(t)
	cfg.Wyoming.Port = addr.Port

	_, done := startServe(t, &cfg, newService(t, http.NotFound))

	err = requireServeReturns(t, done, probeTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wyoming")
	assert.Contains(t, err.Error(), "address already in use")
	status, getErr := get(t.Context(), httpBase(&cfg)+"/internal/readiness")
	assert.Error(t, getErr, "the HTTP server started although start-up failed, status %d", status)
}

func TestServe_HTTPPortInUse_ClosesTheWyomingListener(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, listener.Close()) })
	addr, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)

	cfg := wyomingEnabled(t)
	cfg.Port = addr.Port

	_, done := startServe(t, &cfg, newService(t, http.NotFound))

	err = requireServeReturns(t, done, probeTimeout)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "address already in use")
	conn, dialErr := (&net.Dialer{Timeout: probeTimeout}).DialContext(t.Context(), "tcp", wyomingAddr(&cfg))
	if dialErr == nil {
		assert.NoError(t, conn.Close())
	}
	assert.Error(t, dialErr, "the Wyoming listener outlived serve")
}

func TestServe_Wyoming_IPv6Loopback_Binds(t *testing.T) {
	probe, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(ipv6Loopback, "0"))
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	require.NoError(t, probe.Close())

	cfg := wyomingEnabled(t)
	cfg.Wyoming.Host = ipv6Loopback
	cfg.Wyoming.Port = freePortOn(t, ipv6Loopback)
	require.NoError(t, cfg.Validate())

	cancel, done := startServe(t, &cfg, newService(t, http.NotFound))
	c := dialWyoming(t, &cfg)
	c.send(eventDescribe, "")
	c.readType(eventInfo)

	cancel()
	require.NoError(t, requireServeReturns(t, done, probeTimeout))
}
