package wyoming

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const wantInfo = `{
  "asr": [{
    "name": "llama-swap-voice",
    "description": "qwen3-asr",
    "attribution": {"name": "llama-swap-voice", "url": "https://github.com/mniehe/llama-swap-voice"},
    "installed": true,
    "version": "1.2.3",
    "models": [{
      "name": "qwen3-asr",
      "attribution": {"name": "llama-swap-voice", "url": "https://github.com/mniehe/llama-swap-voice"},
      "installed": true,
      "languages": ["en", "de"]
    }],
    "supports_transcript_streaming": false
  }],
  "tts": [{
    "name": "llama-swap-voice",
    "description": "qwen3-tts",
    "attribution": {"name": "llama-swap-voice", "url": "https://github.com/mniehe/llama-swap-voice"},
    "installed": true,
    "version": "1.2.3",
    "voices": [
      {"name": "narrator", "description": "narrator (default)", "attribution": {"name": "llama-swap-voice", "url": "https://github.com/mniehe/llama-swap-voice"}, "installed": true, "languages": ["en", "de"]},
      {"name": "alice", "description": "alice", "attribution": {"name": "llama-swap-voice", "url": "https://github.com/mniehe/llama-swap-voice"}, "installed": true, "languages": ["en", "de"]},
      {"name": "bob", "description": "bob", "attribution": {"name": "llama-swap-voice", "url": "https://github.com/mniehe/llama-swap-voice"}, "installed": true, "languages": ["en", "de"]}
    ],
    "supports_synthesize_streaming": false
  }]
}`

func TestServe_Describe_ReturnsInfoWithDefaultVoiceFirst(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.send(typeDescribe, nil, nil)

	assert.JSONEq(t, wantInfo, string(c.readType(typeInfo).Data))
}

func TestServe_Describe_StreamingModeStillAdvertisesNoStreaming(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.config.Streaming = ModeSentences })
	c := h.dial()

	c.send(typeDescribe, nil, nil)

	tts := decodeData[info](t, c.readType(typeInfo)).TTS
	require.Len(t, tts, 1)
	assert.False(t, tts[0].SupportsSynthesizeStreaming)
}

func TestServe_Describe_NoLanguagesListsAnEmptyArray(t *testing.T) {
	h := newHarness(t, func(s *setup) { s.config.Languages = nil })
	c := h.dial()

	c.send(typeDescribe, nil, nil)

	data := string(c.readType(typeInfo).Data)
	assert.Contains(t, data, `"languages":[]`)
	assert.NotContains(t, data, `"languages":null`)
}

func TestServe_Describe_RepeatsOnOneConnection(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	for range 3 {
		c.send(typeDescribe, nil, nil)
		c.readType(typeInfo)
	}
}

func TestServe_Ping_ReturnsPongWithText(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.send(typePing, map[string]string{"text": "marco"}, nil)

	assert.JSONEq(t, `{"text":"marco"}`, string(c.readType(typePong).Data))
}

func TestServe_Ping_WithoutTextReturnsNullText(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.send(typePing, nil, nil)

	assert.JSONEq(t, `{"text":null}`, string(c.readType(typePong).Data))
}

func TestServe_UnknownEvents_AreIgnored(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.send("run-satellite", nil, nil)
	c.send("voice-started", map[string]int{"timestamp": 0}, nil)
	c.send("detect", nil, []byte("payload"))
	c.send(typePing, map[string]string{"text": "after"}, nil)

	assert.JSONEq(t, `{"text":"after"}`, string(c.readType(typePong).Data))
}

func pingFrame(header, data, payload string) []byte {
	return []byte(header + "\n" + data + payload)
}

func TestServe_FrameAtTheLimits_IsServed(t *testing.T) {
	atLimitData := padded(t, "", maxDataBytes)
	payload := strings.Repeat("p", maxPayloadBytes)

	tests := []struct {
		name  string
		frame []byte
	}{
		{name: "header line", frame: pingFrame(padded(t, typePing, maxHeaderBytes), "", "")},
		{name: "data", frame: pingFrame(fmt.Sprintf(`{"type":"ping","data_length":%d}`, len(atLimitData)), atLimitData, "")},
		{name: "payload", frame: pingFrame(fmt.Sprintf(`{"type":"ping","payload_length":%d}`, len(payload)), "", payload)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := h.dial()

			written := make(chan error, 1)
			go func() {
				_, err := c.conn.Write(tc.frame)
				written <- err
			}()

			c.readType(typePong)
			require.NoError(t, <-written)
			c.send(typePing, nil, nil)
			c.readType(typePong)
		})
	}
}

// Each frame ends where the server stops reading it, so no unread bytes can
// reset the connection and the error event reliably arrives before the close.
func TestServe_OverLimitHeader_SendsInvalidErrorAndCloses(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "header line", header: padded(t, typePing, maxHeaderBytes+1)},
		{name: "data length", header: fmt.Sprintf(`{"type":"ping","data_length":%d}`, maxDataBytes+1)},
		{name: "payload length", header: fmt.Sprintf(`{"type":"ping","payload_length":%d}`, maxPayloadBytes+1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			c := h.dial()

			c.sendRaw([]byte(tc.header + "\n"))

			c.requireError(codeInvalid)
			c.requireClosed()
		})
	}
}

func TestServe_MalformedFrame_SendsInvalidErrorAndCloses(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.sendRaw([]byte("not json\n"))

	c.requireError(codeInvalid)
	c.requireClosed()
}

func TestServe_ConnectionLimit_ClosesExcessAtAccept(t *testing.T) {
	h := newHarness(t)

	clients := make([]*client, 0, maxConns)
	for range maxConns {
		c := h.dial()
		c.send(typePing, nil, nil)
		c.readType(typePong)
		clients = append(clients, c)
	}

	excess := h.dial()
	excess.requireClosed()

	require.NoError(t, clients[0].conn.Close())
	require.Eventually(t, func() bool {
		next, err := net.Dial("tcp", h.addr)
		if err != nil {
			return false
		}
		defer closeQuietly(t, next)

		return pinged(next, newReader(next), time.Second)
	}, waitTimeout, 50*time.Millisecond, "no connection was served after one closed")
}

func TestServe_IdleClient_ClosedAtIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		start := time.Now()
		c, done := h.servePipe()

		c.send(typePing, nil, nil)
		c.readType(typePong)
		<-done

		assert.Equal(t, h.server.idleTimeout, time.Since(start))
	})
}

// pinged sends a ping and reports whether a pong came back within limit. It
// never fails the test, so a closed connection simply reports false.
func pinged(conn net.Conn, in *bufio.Reader, limit time.Duration) bool {
	if err := conn.SetDeadline(time.Now().Add(limit)); err != nil {
		return false
	}
	if err := WriteEvent(conn, &Event{Type: typePing}); err != nil {
		return false
	}

	event, err := ReadEvent(in)
	return err == nil && event.Type == typePong
}

func TestServe_PingingClient_ClosedAtIdleTimeoutFromAccept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		gap := h.server.idleTimeout / 3
		start := time.Now()
		c, done := h.servePipe()

		for ping := range 3 {
			if ping > 0 {
				<-time.After(gap)
			}
			c.send(typePing, nil, nil)
			c.readType(typePong)
		}
		<-done

		assert.Equal(t, h.server.idleTimeout, time.Since(start), "pings moved the idle deadline")
	})
}

func TestReadDeadline(t *testing.T) {
	const idle = time.Minute
	now := time.Now()
	s := &Server{idleTimeout: idle}

	tests := []struct {
		name    string
		request *request
		want    time.Time
	}{
		{name: "no request counts from the idle start", want: now.Add(idle - 10*time.Second)},
		{name: "an earlier request deadline clamps", request: &request{deadline: now.Add(time.Second)}, want: now.Add(time.Second)},
		{name: "a later request deadline does not", request: &request{deadline: now.Add(2 * idle)}, want: now.Add(idle)},
		{name: "an unadmitted request", request: &request{}, want: now.Add(idle)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &connection{server: s, request: tc.request, idleSince: now.Add(-10 * time.Second)}

			assert.Equal(t, tc.want, c.readDeadline(now))
		})
	}
}

func TestOutcomeOf_CancelledBeforeDeadlineSeenAfterIt_IsShuttingDown(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(time.Hour))
	cancel()
	c := &connection{request: &request{ctx: ctx, deadline: time.Now().Add(-time.Second)}}

	assert.Equal(t, codeShuttingDown, c.outcomeOf(nil))
}

// The request context's timer and the wall-clock deadline are separate
// signals of one deadline, and either can be seen first.
func TestOutcomeOf_DeadlineExceeded_IsTimeout(t *testing.T) {
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		deadline time.Time
	}{
		{name: "the context's deadline passed, the wall clock's not yet", ctx: expired, deadline: time.Now().Add(time.Hour)},
		{name: "the wall clock's deadline passed, the context's not yet", ctx: t.Context(), deadline: time.Now().Add(-time.Second)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &connection{request: &request{ctx: tc.ctx, deadline: tc.deadline}}

			assert.Equal(t, codeTimeout, c.outcomeOf(nil))
		})
	}
}

func TestWriteDeadline(t *testing.T) {
	const (
		idle  = time.Minute
		write = 10 * time.Second
	)
	now := time.Now()
	s := &Server{idleTimeout: idle, writeTimeout: write}

	tests := []struct {
		name      string
		idleSince time.Time
		request   *request
		want      time.Time
	}{
		{name: "no request and the idle deadline comes first", idleSince: now.Add(-idle + time.Second), want: now.Add(time.Second)},
		{name: "no request and the write timeout comes first", idleSince: now, want: now.Add(write)},
		{name: "a request ignores the idle deadline", idleSince: now.Add(-idle), request: &request{}, want: now.Add(write)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &connection{server: s, request: tc.request, idleSince: tc.idleSince}

			assert.Equal(t, tc.want, c.writeDeadline(now))
		})
	}
}

// Events already buffered are not read from the socket, so the read deadline
// alone cannot stop them; this audio-start waits in the reader, not the pipe.
func TestServe_BufferedEventAfterIdleDeadline_EndsTheConnection(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	c, _ := h.pipeConnection()
	var buffered bytes.Buffer
	require.NoError(t, WriteEvent(&buffered, audioStartEvent(t)))
	c.in = bufio.NewReader(&buffered)
	c.idleSince = time.Now().Add(-2 * h.server.idleTimeout)

	err := c.serve()
	opened := c.request != nil
	c.endRequest(outcomeClosed)

	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
	assert.False(t, opened, "a request began after the idle deadline")
}

// An open request is bound by its own deadline, not the idle deadline, so
// the events buffered behind its audio-start are still handled.
func TestServe_BufferedEventPastIdleDeadlineDuringRequest_IsHandled(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot), withServer(func(s *Server) {
		s.transcribe = func(context.Context, io.Reader) (string, error) { return transcriptText, nil }
	}))
	c, peer := h.pipeConnection()
	c.idleSince = time.Now().Add(-2 * h.server.idleTimeout)
	require.NoError(t, c.handle(audioStartEvent(t)))
	var buffered bytes.Buffer
	require.NoError(t, WriteEvent(&buffered, &Event{Type: typeAudioChunk, Data: audioStartEvent(t).Data, Payload: []byte{0, 0}}))
	require.NoError(t, WriteEvent(&buffered, &Event{Type: typeAudioStop}))
	c.in = bufio.NewReader(&buffered)

	served := make(chan error, 1)
	go func() {
		err := c.serve()
		c.endRequest(outcomeClosed)
		closeQuietly(t, c.conn)
		served <- err
	}()

	(&client{t: t, conn: peer, in: newReader(peer)}).requireTranscript(transcriptText)
	require.ErrorIs(t, <-served, io.EOF, "serve stopped before the buffered events ran out")
}

func TestRefuse_PastTheIdleDeadline_StillSendsTheError(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	c, peer := h.pipeConnection()
	c.idleSince = time.Now().Add(-2 * h.server.idleTimeout)
	require.NoError(t, c.handle(audioStartEvent(t)))

	refused := make(chan error, 1)
	go func() { refused <- c.refuse(invalid("refused")) }()

	failed := (&client{t: t, conn: peer, in: newReader(peer)}).requireError(codeInvalid)
	assert.Equal(t, "refused", failed.Text)
	require.Error(t, <-refused)
}

func audioStartEvent(t *testing.T) *Event {
	t.Helper()

	raw, err := json.Marshal(homeAssistantFormat)
	require.NoError(t, err)

	return &Event{Type: typeAudioStart, Data: raw}
}

// With no request open, a pong the peer does not read is cut off at the idle
// deadline instead of lasting writeTimeout. The pipe has no buffer, so the
// server cannot get past that pong while the peer does not read.
func TestServe_BlockedPongWithNoRequest_EndsAtIdleDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withServer(func(s *Server) { s.writeTimeout = time.Hour }))
		start := time.Now()
		c, done := h.servePipe()
		var pings bytes.Buffer
		for range 3 {
			require.NoError(t, WriteEvent(&pings, &Event{Type: typePing}))
		}

		c.sendRaw(pings.Bytes())
		synctest.Wait()
		select {
		case <-done:
			require.FailNow(t, "the connection ended before its first pong")
		default:
		}
		require.Zero(t, time.Since(start), "the pong was not blocked from the start")
		<-done

		assert.Equal(t, h.server.idleTimeout, time.Since(start))
		unread, err := io.ReadAll(c.in)
		require.NoError(t, err)
		assert.Empty(t, unread, "part of the pong was written")
	})
}

// pipeListener hands Serve one end of an in-memory pipe, which has no
// buffer, so a client that stops reading blocks the server's next write.
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
}

func (p *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-p.conns:
		return conn, nil
	case <-p.closed:
		return nil, net.ErrClosed
	}
}

// Close reports net.ErrClosed on every call after the first, as a real
// listener does; Serve and Server.Close may call it at the same time.
func (p *pipeListener) Close() error {
	err := net.ErrClosed
	p.once.Do(func() {
		close(p.closed)
		err = nil
	})

	return err
}

func (p *pipeListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

func (p *pipeListener) dial(t *testing.T) net.Conn {
	server, client := net.Pipe()
	t.Cleanup(func() { closeQuietly(t, client) })
	p.conns <- server

	return client
}

// dialGated is dial with the server's end wrapped in a gatedConn.
func (p *pipeListener) dialGated(t *testing.T) (*gatedConn, *client) {
	server, conn := net.Pipe()
	t.Cleanup(func() { closeQuietly(t, conn) })
	gated := newGatedConn(t, server)
	p.conns <- gated

	return gated, &client{t: t, conn: conn, in: newReader(conn)}
}

func TestServe_ClientNotReading_ClosedAtWriteTimeout(t *testing.T) {
	const timeout = 200 * time.Millisecond
	listener := newPipeListener()
	h := newHarness(t, withListener(listener), withServer(func(s *Server) { s.writeTimeout = timeout }))
	conn := listener.dial(t)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(waitTimeout)))

	require.NoError(t, WriteEvent(conn, &Event{Type: typeDescribe}))
	require.Eventually(t, h.logs.logged("wyoming connection ended", 1), waitTimeout, 10*time.Millisecond)

	unread, err := io.ReadAll(conn)
	require.NoError(t, err, "the server should have closed the pipe, which reads as EOF")
	assert.Empty(t, unread, "the info event was still waiting to be written")
}

type failingListener struct {
	net.Listener
	err      error
	closeErr error
}

func (f failingListener) Accept() (net.Conn, error) {
	return nil, f.err
}

func (f failingListener) Close() error {
	return errors.Join(f.Listener.Close(), f.closeErr)
}

// closeGatedListener fails every Accept and holds its first Close until the
// test opens the gate, so Server.Close can run while Serve is closing it.
type closeGatedListener struct {
	failingListener
	first   sync.Once
	started chan struct{}
	gate    chan struct{}
}

func (g *closeGatedListener) Close() error {
	held := false
	g.first.Do(func() { held = true })
	if held {
		close(g.started)
		<-g.gate
	}

	return g.failingListener.Close()
}

func TestServe_AcceptErrorWhileClosing_IsReturned(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	acceptErr := errors.New("accept broke")
	l := &closeGatedListener{
		failingListener: failingListener{Listener: inner, err: acceptErr},
		started:         make(chan struct{}),
		gate:            make(chan struct{}),
	}
	p := newPipeline(t, newFakeUpstream(t, serveTranscript(t, transcriptText)).server.URL, nil)
	s := New(t.Context(), &Config{}, p, discardLogger())
	served := make(chan error, 1)

	go func() { served <- s.Serve(l) }()
	select {
	case <-l.started:
	case <-time.After(waitTimeout):
		require.FailNow(t, "Serve did not close its listener")
	}
	require.NoError(t, s.Close())
	close(l.gate)

	select {
	case err = <-served:
	case <-time.After(waitTimeout):
		require.FailNow(t, "Serve did not return")
	}
	assert.ErrorIs(t, err, acceptErr, "Close during Serve's own close hid the accept error")
}

func TestServe_AcceptAndCloseErrors_AreBothReturned(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	acceptErr := errors.New("accept broke")
	closeErr := errors.New("close broke")
	p := newPipeline(t, newFakeUpstream(t, serveTranscript(t, transcriptText)).server.URL, nil)
	s := New(t.Context(), &Config{}, p, discardLogger())

	err = s.Serve(failingListener{Listener: inner, err: acceptErr, closeErr: closeErr})

	assert.ErrorIs(t, err, acceptErr)
	assert.ErrorIs(t, err, closeErr)
	assert.NoError(t, s.Close())
}

func TestServe_AcceptError_IsReturned(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	acceptErr := errors.New("accept broke")
	p := newPipeline(t, newFakeUpstream(t, serveTranscript(t, transcriptText)).server.URL, nil)
	s := New(t.Context(), &Config{}, p, discardLogger())

	err = s.Serve(failingListener{Listener: inner, err: acceptErr})

	assert.ErrorIs(t, err, acceptErr)
	dialled, dialErr := net.DialTimeout("tcp", inner.Addr().String(), time.Second)
	if dialErr == nil {
		closeQuietly(t, dialled)
	}
	assert.Error(t, dialErr, "the listener still accepts connections after Serve returned")
	assert.NoError(t, s.Close())
}

func TestServe_AfterClose_ReturnsNilAndClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := newPipeline(t, newFakeUpstream(t, serveTranscript(t, transcriptText)).server.URL, nil)
	s := New(t.Context(), &Config{}, p, discardLogger())
	require.NoError(t, s.Close())

	require.NoError(t, s.Serve(listener))

	_, err = listener.Accept()
	assert.ErrorIs(t, err, net.ErrClosed)
}

func TestServe_Close_ClosesOpenConnections(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	c.send(typePing, nil, nil)
	c.readType(typePong)

	require.NoError(t, h.server.Close())

	c.requireClosed()
}

func TestServe_Panic_ClosesThatConnectionAndFreesTheRequest(t *testing.T) {
	h := newHarness(t,
		withPipeline(oneASRSlot),
		withServer(func(s *Server) {
			s.transcribe = func(context.Context, io.Reader) (string, error) { panic("transcriber exploded") }
		}),
	)
	bystander := h.dial()
	c := h.dial()

	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.send(typeAudioChunk, homeAssistantFormat, silence(homeAssistantFormat, 100*time.Millisecond))
	c.send(typeAudioStop, nil, nil)

	c.requireClosed()
	h.requireSlotFreed()
	bystander.send(typePing, nil, nil)
	bystander.readType(typePong)

	require.Eventually(t, h.logs.logged("panic recovered", 1), waitTimeout, 10*time.Millisecond)
	recovered := h.logs.lines(t, "panic recovered")[0]
	assert.Equal(t, "transcriber exploded", recovered["panic"])
	assert.Contains(t, recovered["stack"], "TestServe_Panic_ClosesThatConnectionAndFreesTheRequest.func1.1", "the stack names the panicking frame")
	h.requireOutcome(outcomePanic)
}

// The cleanup must close the connection itself when the deadline hook has
// started but not yet closed it, or that close could land on a later request.
func TestEndRequest_HookAlreadyStarted_ClosesTheConnection(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	h := newHarness(t, withPipeline(oneASRSlot), withBase(base))
	server, peer := net.Pipe()
	t.Cleanup(func() { closeQuietly(t, peer) })
	gated := newGatedConn(t, server)
	c := &connection{server: h.server, conn: gated, in: bufio.NewReader(gated)}
	c.beginRequest(typeTranscribe)
	require.NoError(t, c.admit(pipeline.WorkTranscribe))
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(time.Second)))

	cancel()
	gated.requireCloseStarted(t)
	c.endRequest(outcomeOK)

	_, err := peer.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "the connection is still open while the hook waits to close it")
	assert.True(t, h.slotFree())
}

func TestTranscribe_HookStartedBeforeAudioStop_ClosesWithoutReply(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	listener := newPipeListener()
	h := newHarness(t,
		withListener(listener),
		withBase(base),
		withPipeline(oneASRSlot),
		withServer(func(s *Server) {
			s.transcribe = func(context.Context, io.Reader) (string, error) { return transcriptText, nil }
		}),
	)
	gated, c := listener.dialGated(t)

	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.send(typeAudioChunk, homeAssistantFormat, silence(homeAssistantFormat, 10*time.Millisecond))
	cancel()
	gated.requireCloseStarted(t)
	c.send(typeAudioStop, nil, nil)

	events := c.closedWithin(waitTimeout)
	assert.Empty(t, events, "a request whose hook started must not be answered on that connection")
	h.requireSlotFreed()
	h.requireOutcome(codeShuttingDown)
}
