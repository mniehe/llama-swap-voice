package wyoming

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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/pipeline"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	ttsModelName   = "qwen3-tts"
	asrModelName   = "qwen3-asr"
	asrRoute       = "/upstream/" + asrModelName + "/v1/audio/transcriptions"
	ttsRoute       = "/upstream/" + ttsModelName + "/tts"
	transcriptText = "turn on the kitchen light"
	speechRate     = 24000
	speechWidth    = 2
	speechChannels = 1
	serviceVersion = "1.2.3"
	fakeFormMemory = 1 << 20
	harnessTimeout = 10 * time.Second
	waitTimeout    = 5 * time.Second
	drainTimeout   = 5 * time.Second
	// Config validation keeps streamingMaxChars within tts.maxInputChars.
	harnessMaxInputChars     = 100
	harnessStreamingMaxChars = 50
)

type upstreamCall struct {
	path   string
	fields map[string]string
	files  map[string][]byte
}

// fakeUpstream stands in for llama-swap, recording each multipart request.
type fakeUpstream struct {
	t       *testing.T
	server  *httptest.Server
	mu      sync.Mutex
	calls   []upstreamCall
	respond http.HandlerFunc
}

func newFakeUpstream(t *testing.T, respond http.HandlerFunc) *fakeUpstream {
	f := &fakeUpstream{t: t, respond: respond}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeUpstream) serve(w http.ResponseWriter, r *http.Request) {
	call := upstreamCall{path: r.URL.Path, fields: map[string]string{}, files: map[string][]byte{}}
	if err := r.ParseMultipartForm(fakeFormMemory); err != nil {
		assert.NoError(f.t, err, "upstream got an invalid request")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer func() { assert.NoError(f.t, r.MultipartForm.RemoveAll()) }()

	for key, values := range r.MultipartForm.Value {
		call.fields[key] = values[0]
	}
	for key, headers := range r.MultipartForm.File {
		call.files[key] = readPart(f.t, headers[0])
	}

	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()

	f.respond(w, r)
}

func readPart(t *testing.T, header *multipart.FileHeader) []byte {
	file, err := header.Open()
	if !assert.NoError(t, err) {
		return nil
	}
	defer func() { assert.NoError(t, file.Close()) }()

	data, err := io.ReadAll(file)
	assert.NoError(t, err)

	return data
}

func (f *fakeUpstream) recorded() []upstreamCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]upstreamCall(nil), f.calls...)
}

// inputs returns the text of each TTS request in arrival order.
func (f *fakeUpstream) inputs() []string {
	calls := f.recorded()
	texts := make([]string, 0, len(calls))
	for _, call := range calls {
		texts = append(texts, call.fields["input"])
	}

	return texts
}

func serveTranscript(t *testing.T, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"text": text}))
	}
}

// serveSpeech answers a TTS request with pcm in a 24 kHz mono 16-bit WAV.
// A write fails when the pipeline stops reading, which some tests cause on
// purpose, so a failed write is logged rather than failing the test.
func serveSpeech(t *testing.T, pcm []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		header, err := audio.WAVHeader(speechRate, speechWidth, speechChannels, len(pcm))
		if !assert.NoError(t, err) {
			return
		}

		w.Header().Set("Content-Type", "audio/wav")
		if _, err := w.Write(append(header, pcm...)); err != nil {
			t.Logf("fake upstream: writing speech: %v", err)
		}
	}
}

func serveStatus(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(status), status)
	}
}

// syncBuffer collects log output written from connection goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// lines returns every JSON log record whose msg is message.
func (b *syncBuffer) lines(t *testing.T, message string) []map[string]any {
	t.Helper()

	matched, err := b.records(message)
	require.NoError(t, err)

	return matched
}

// records is lines for a require.Eventually condition, which must not fail
// the test itself.
func (b *syncBuffer) records(message string) ([]map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var matched []map[string]any
	for line := range bytes.Lines(b.buf.Bytes()) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, err
		}
		if record["msg"] == message {
			matched = append(matched, record)
		}
	}

	return matched, nil
}

// logged reports whether exactly want records with msg message exist.
func (b *syncBuffer) logged(message string, want int) func() bool {
	return func() bool {
		matched, err := b.records(message)
		return err == nil && len(matched) == want
	}
}

type setup struct {
	respond  http.HandlerFunc
	pipeline []func(*pipeline.Config)
	server   []func(*Server)
	config   Config
	listener net.Listener
	base     context.Context
}

type option func(*setup)

func withUpstream(respond http.HandlerFunc) option {
	return func(s *setup) { s.respond = respond }
}

func withPipeline(tweak func(*pipeline.Config)) option {
	return func(s *setup) { s.pipeline = append(s.pipeline, tweak) }
}

func withServer(tweak func(*Server)) option {
	return func(s *setup) { s.server = append(s.server, tweak) }
}

func withListener(l net.Listener) option {
	return func(s *setup) { s.listener = l }
}

func withBase(ctx context.Context) option {
	return func(s *setup) { s.base = ctx }
}

func oneASRSlot(cfg *pipeline.Config) {
	cfg.MaxConcurrent = 1
}

func requestTimeout(timeout time.Duration) option {
	return withPipeline(func(cfg *pipeline.Config) { cfg.RequestTimeout = timeout })
}

type harness struct {
	t        *testing.T
	server   *Server
	pipeline *pipeline.Pipeline
	upstream *fakeUpstream
	addr     string
	logs     *syncBuffer
	served   chan error

	serveOnce sync.Once
	serveErr  error
}

func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()

	cfg := newSetup(t, opts)
	h := &harness{t: t, logs: &syncBuffer{}, served: make(chan error, 1)}
	h.upstream = newFakeUpstream(t, cfg.respond)
	h.build(&cfg, h.upstream.server.URL, &http.Client{})

	listener := cfg.listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
	}
	h.addr = listener.Addr().String()

	go func() { h.served <- h.server.Serve(listener) }()
	t.Cleanup(h.stop)

	return h
}

func newSetup(t *testing.T, opts []option) setup {
	cfg := setup{
		respond: serveTranscript(t, transcriptText),
		config:  Config{Languages: []string{"en", "de"}, Speed: 1, Streaming: ModeWhole, StreamingMaxChars: harnessStreamingMaxChars, Version: serviceVersion},
		base:    t.Context(),
	}
	for _, apply := range opts {
		apply(&cfg)
	}

	return cfg
}

// build makes the pipeline, calling the upstream at upstreamURL through
// httpClient, and the server.
func (h *harness) build(cfg *setup, upstreamURL string, httpClient *http.Client) {
	h.t.Helper()

	h.pipeline = newPipelineWith(h.t, upstreamURL, httpClient, cfg.pipeline)
	log := slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.server = New(cfg.base, &cfg.config, h.pipeline, log)
	for _, apply := range cfg.server {
		apply(h.server)
	}
}

// memoryUpstream names the upstream of a bubble harness, which memoryTransport
// serves without a socket.
const memoryUpstream = "http://upstream.invalid"

// newBubbleHarness is newHarness for a test inside a synctest bubble: no
// listener, and an upstream served in memory, because a bubble's virtual
// clock stops while a goroutine waits on a real socket. The test serves each
// connection with servePipe or serveInBubble.
func newBubbleHarness(t *testing.T, opts ...option) *harness {
	t.Helper()

	cfg := newSetup(t, opts)
	h := &harness{t: t, logs: &syncBuffer{}}
	h.upstream = &fakeUpstream{t: t, respond: cfg.respond}
	h.build(&cfg, memoryUpstream, &http.Client{Transport: memoryTransport{t: t, handler: http.HandlerFunc(h.upstream.serve)}})
	t.Cleanup(h.requireDrained)

	return h
}

// memoryTransport answers each request by running handler on it, with the
// response body on an in-memory pipe.
type memoryTransport struct {
	t       *testing.T
	handler http.Handler
}

func (m memoryTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body, pipe := io.Pipe()
	w := &memoryResponse{header: http.Header{}, body: pipe, started: make(chan struct{})}
	go func() {
		m.handler.ServeHTTP(w, r)
		w.WriteHeader(http.StatusOK)
		assert.NoError(m.t, pipe.Close())
	}()

	select {
	case <-w.started:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}

	return &http.Response{
		Status:        http.StatusText(w.status),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.header,
		Body:          body,
		ContentLength: -1,
		Request:       r,
	}, nil
}

// memoryResponse is the http.ResponseWriter of memoryTransport. Writes block
// until the client reads them.
type memoryResponse struct {
	header  http.Header
	body    *io.PipeWriter
	status  int
	once    sync.Once
	started chan struct{}
}

func (w *memoryResponse) Header() http.Header {
	return w.header
}

func (w *memoryResponse) WriteHeader(status int) {
	w.once.Do(func() {
		w.status = status
		close(w.started)
	})
}

func (w *memoryResponse) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(p)
}

func (w *memoryResponse) Flush() {}

// servePipe runs serveConn on one end of an in-memory pipe and returns a
// client on the other end, and a channel closed when serveConn returns.
func (h *harness) servePipe() (c *client, done <-chan struct{}) {
	server, peer := net.Pipe()
	h.t.Cleanup(func() { closeQuietly(h.t, peer) })

	served := make(chan struct{})
	go func() {
		defer close(served)
		h.server.serveConn(server)
	}()

	return &client{t: h.t, conn: peer, in: newReader(peer)}, served
}

func newPipeline(t *testing.T, upstreamURL string, tweaks []func(*pipeline.Config)) *pipeline.Pipeline {
	t.Helper()

	return newPipelineWith(t, upstreamURL, &http.Client{}, tweaks)
}

func newPipelineWith(t *testing.T, upstreamURL string, httpClient *http.Client, tweaks []func(*pipeline.Config)) *pipeline.Pipeline {
	t.Helper()

	dir := t.TempDir()
	cfg := pipeline.Config{
		TTSModel: ttsModelName,
		ASRModel: asrModelName,
		Voices: map[string]string{
			"narrator": writeFile(t, dir, "narrator.wav"),
			"bob":      writeFile(t, dir, "bob.wav"),
			"alice":    writeFile(t, dir, "alice.wav"),
		},
		DefaultVoice:     "narrator",
		Lang:             "en",
		RequestTimeout:   harnessTimeout,
		MaxInputChars:    harnessMaxInputChars,
		MaxConcurrent:    4,
		MaxConcurrentTTS: 8,
		TTSConcurrency:   1,
	}
	for _, apply := range tweaks {
		apply(&cfg)
	}

	client, err := upstream.New(upstreamURL, ttsModelName, asrModelName, httpClient)
	require.NoError(t, err)

	p, err := pipeline.New(&cfg, client, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	return p
}

func writeFile(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(name), 0o600))

	return path
}

// stop closes the server, then proves that no request is still admitted.
func (h *harness) stop() {
	assert.NoError(h.t, h.server.Close())
	assert.NoError(h.t, h.serveResult(), "Serve after Close")

	h.requireDrained()
}

var errServeRunning = errors.New("serve did not return")

// serveResult waits for Serve to return and gives its error, the same on
// every call; errServeRunning means it was still running after waitTimeout.
func (h *harness) serveResult() error {
	h.serveOnce.Do(func() {
		select {
		case h.serveErr = <-h.served:
		case <-time.After(waitTimeout):
			h.serveErr = errServeRunning
		}
	})

	return h.serveErr
}

// requireDrained fails if a request is still admitted: a leaked admission
// keeps Drain waiting.
func (h *harness) requireDrained() {
	ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	assert.NoError(h.t, h.pipeline.Drain(ctx), "a request is still admitted")
}

// slotFree reports whether an ASR slot can be taken now, giving it back.
func (h *harness) slotFree() bool {
	_, release, err := h.pipeline.Admit(context.Background(), pipeline.WorkTranscribe, time.Now().Add(time.Minute))
	if err != nil {
		return false
	}
	release()

	return true
}

func (h *harness) requireSlotFreed() {
	h.t.Helper()

	require.Eventually(h.t, h.slotFree, waitTimeout, 10*time.Millisecond, "the ASR slot was not released")
}

type client struct {
	t    *testing.T
	conn net.Conn
	in   *bufio.Reader
}

func (h *harness) dial() *client {
	h.t.Helper()

	conn, err := net.Dial("tcp", h.addr)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { closeQuietly(h.t, conn) })

	return &client{t: h.t, conn: conn, in: newReader(conn)}
}

func newReader(conn net.Conn) *bufio.Reader {
	return bufio.NewReader(conn)
}

// closeQuietly closes a test's own end of a connection, which the server
// may already have closed.
func closeQuietly(t *testing.T, conn net.Conn) {
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("closing the client's connection: %v", err)
	}
}

// gatedConn holds its first Close until the test opens the gate, so a test
// can catch the deadline hook after it started and before the socket closed.
// Later Close calls pass straight through.
type gatedConn struct {
	net.Conn
	first   sync.Once
	started chan struct{}
	gate    chan struct{}
}

func newGatedConn(t *testing.T, conn net.Conn) *gatedConn {
	g := &gatedConn{Conn: conn, started: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(g.open)

	return g
}

func (g *gatedConn) Close() error {
	held := false
	g.first.Do(func() { held = true })
	if held {
		close(g.started)
		<-g.gate
	}

	return g.Conn.Close()
}

func (g *gatedConn) open() {
	select {
	case <-g.gate:
	default:
		close(g.gate)
	}
}

func (g *gatedConn) requireCloseStarted(t *testing.T) {
	t.Helper()

	select {
	case <-g.started:
	case <-time.After(waitTimeout):
		require.FailNow(t, "nothing called Close")
	}
}

// pipeConnection returns a connection that the test drives directly, with
// no serve loop, and the peer end of its in-memory pipe.
func (h *harness) pipeConnection() (*connection, net.Conn) {
	h.t.Helper()

	server, peer := net.Pipe()
	h.t.Cleanup(func() {
		closeQuietly(h.t, server)
		closeQuietly(h.t, peer)
	})

	return &connection{server: h.server, conn: server, in: bufio.NewReader(server), idleSince: time.Now()}, peer
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func (c *client) send(eventType string, data any, payload []byte) {
	c.t.Helper()

	event := Event{Type: eventType, Payload: payload}
	if data != nil {
		raw, err := json.Marshal(data)
		require.NoError(c.t, err)
		event.Data = raw
	}

	require.NoError(c.t, c.conn.SetWriteDeadline(time.Now().Add(waitTimeout)))
	require.NoError(c.t, WriteEvent(c.conn, &event))
}

func (c *client) sendRaw(frame []byte) {
	c.t.Helper()

	require.NoError(c.t, c.conn.SetWriteDeadline(time.Now().Add(waitTimeout)))
	_, err := c.conn.Write(frame)
	require.NoError(c.t, err)
}

func (c *client) read() *Event {
	c.t.Helper()

	require.NoError(c.t, c.conn.SetReadDeadline(time.Now().Add(waitTimeout)))
	event, err := ReadEvent(c.in)
	require.NoError(c.t, err)

	return event
}

func (c *client) readType(eventType string) *Event {
	c.t.Helper()

	event := c.read()
	require.Equal(c.t, eventType, event.Type, "event data %s", event.Data)

	return event
}

func (c *client) requireError(code string) errorData {
	c.t.Helper()

	var failed errorData
	require.NoError(c.t, json.Unmarshal(c.readType(typeError).Data, &failed))
	assert.Equal(c.t, code, failed.Code, "error %q", failed.Text)
	assert.NotEmpty(c.t, failed.Text)

	return failed
}

// closedWithin reads until the server closes the connection, failing if
// that takes longer than limit. It returns the events read on the way.
func (c *client) closedWithin(limit time.Duration) []*Event {
	c.t.Helper()

	err := c.conn.SetReadDeadline(time.Now().Add(limit))
	if errors.Is(err, io.ErrClosedPipe) {
		// An in-memory pipe has no buffer, so nothing is left to read.
		return nil
	}
	require.NoError(c.t, err)

	var events []*Event
	for {
		event, err := ReadEvent(c.in)
		if err == nil {
			events = append(events, event)
			continue
		}

		var timeout net.Error
		require.False(c.t, errors.As(err, &timeout) && timeout.Timeout(), "connection still open after %s", limit)

		return events
	}
}

func (c *client) requireClosed() {
	c.t.Helper()

	c.closedWithin(waitTimeout)
}

func decodeData[T any](t *testing.T, event *Event) T {
	t.Helper()

	var value T
	require.NoError(t, json.Unmarshal(event.Data, &value))

	return value
}
