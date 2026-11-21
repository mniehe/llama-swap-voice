package wyoming

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveInBubble runs Serve on an in-memory listener inside a synctest
// bubble, so the test sees Shutdown's real connection tracking.
func (h *harness) serveInBubble() (listener *pipeListener, served <-chan error) {
	l := newPipeListener()
	result := make(chan error, 1)
	go func() { result <- h.server.Serve(l) }()

	return l, result
}

func (p *pipeListener) dialClient(t *testing.T) *client {
	conn := p.dial(t)
	return &client{t: t, conn: conn, in: newReader(conn)}
}

// shutdownAsync starts Shutdown and returns a channel that receives its
// result.
func shutdownAsync(ctx context.Context, s *Server) <-chan error {
	result := make(chan error, 1)
	go func() { result <- s.Shutdown(ctx) }()

	return result
}

func requireNotReturned(t *testing.T, result <-chan error, what string) {
	t.Helper()

	select {
	case err := <-result:
		require.FailNow(t, what+" returned early", "error: %v", err)
	default:
	}
}

// heldTranscriber blocks each transcription until release closes.
func heldTranscriber(entered chan<- struct{}, release <-chan struct{}) func(context.Context, io.Reader) (string, error) {
	return func(context.Context, io.Reader) (string, error) {
		entered <- struct{}{}
		<-release
		return transcriptText, nil
	}
}

func instantTranscriber(context.Context, io.Reader) (string, error) {
	return transcriptText, nil
}

func TestShutdown_ServeReturnsNilAndTheListenerIsClosed(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	c.send(typePing, nil, nil)
	c.readType(typePong)

	require.NoError(t, h.server.Shutdown(t.Context()))

	require.NoError(t, h.serveResult())
	dialled, err := net.DialTimeout("tcp", h.addr, time.Second)
	if err == nil {
		closeQuietly(t, dialled)
	}
	assert.Error(t, err, "the listener still accepts connections after Shutdown")
}

func TestShutdown_IdleConnection_IsClosedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.send(typePing, nil, nil)
		c.readType(typePong)
		start := time.Now()

		require.NoError(t, h.server.Shutdown(t.Context()))

		assert.Zero(t, time.Since(start))
		c.requireClosed()
		require.NoError(t, <-served)
	})
}

// A connection with a request open is kept. Shutdown waits for the request
// and then for the connection's goroutine, which still refuses the request
// the client had already sent behind the first.
func TestShutdown_RetainedConnection_FinishesItsRequestAndRefusesTheNext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withServer(func(s *Server) { s.transcribe = instantTranscriber }))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.send(typeAudioStart, homeAssistantFormat, nil)
		c.send(typeAudioChunk, homeAssistantFormat, silence(homeAssistantFormat, 10*time.Millisecond))
		synctest.Wait()

		shutdown := shutdownAsync(t.Context(), h.server)
		synctest.Wait()
		requireNotReturned(t, shutdown, "Shutdown with a recording open")

		var frames bytes.Buffer
		require.NoError(t, WriteEvent(&frames, &Event{Type: typeAudioStop}))
		require.NoError(t, WriteEvent(&frames, &Event{Type: typeSynthesize, Data: []byte(`{"text":"Hello there."}`)}))
		c.sendRaw(frames.Bytes())
		c.requireTranscript(transcriptText)
		synctest.Wait()
		requireNotReturned(t, shutdown, "Shutdown while the connection's goroutine writes")

		assert.Contains(t, c.requireError(codeShuttingDown).Text, "shutting down")
		c.requireClosed()
		require.NoError(t, <-shutdown)
		require.NoError(t, <-served)
	})
}

func TestShutdown_ActiveRequest_IsWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		h := newBubbleHarness(t, withServer(func(s *Server) { s.transcribe = heldTranscriber(entered, release) }))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.send(typeAudioStart, homeAssistantFormat, nil)
		c.send(typeAudioChunk, homeAssistantFormat, silence(homeAssistantFormat, 10*time.Millisecond))
		c.send(typeAudioStop, nil, nil)
		<-entered

		shutdown := shutdownAsync(t.Context(), h.server)
		synctest.Wait()
		requireNotReturned(t, shutdown, "Shutdown during a transcription")

		close(release)
		c.requireTranscript(transcriptText)
		c.requireClosed()
		require.NoError(t, <-shutdown)
		require.NoError(t, <-served)
	})
}

func TestShutdown_NewConnectionAfterShutdown_IsRefused(t *testing.T) {
	h := newHarness(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	require.NoError(t, h.server.Shutdown(t.Context()))

	assert.NoError(t, h.server.Serve(listener), "Serve after Shutdown")
	_, err = listener.Accept()
	assert.ErrorIs(t, err, net.ErrClosed, "Serve after Shutdown leaves its listener open")
}

func TestShutdown_ContextExpires_ReturnsItsErrorAndCloseEndsTheConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 3 * time.Second
		h := newBubbleHarness(t, requestTimeout(time.Hour))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.send(typeAudioStart, homeAssistantFormat, nil)
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), budget)
		defer cancel()
		start := time.Now()

		err := h.server.Shutdown(ctx)

		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, budget, time.Since(start))
		c.send(typePing, nil, nil)
		c.readType(typePong)

		require.NoError(t, h.server.Close())
		c.requireClosed()
		require.NoError(t, h.server.Shutdown(t.Context()), "Shutdown after Close waits for the goroutines")
		require.NoError(t, <-served)
	})
}

// A client that sent part of a new event behind its request gets the reply,
// and then the connection ends: Shutdown does not wait for the rest.
func TestShutdown_IncompleteBufferedEventAfterTheRequest_EndsAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withServer(func(s *Server) { s.transcribe = instantTranscriber }))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.send(typeAudioStart, homeAssistantFormat, nil)
		c.send(typeAudioChunk, homeAssistantFormat, silence(homeAssistantFormat, 10*time.Millisecond))
		synctest.Wait()
		shutdown := shutdownAsync(t.Context(), h.server)
		synctest.Wait()

		var frames bytes.Buffer
		require.NoError(t, WriteEvent(&frames, &Event{Type: typeAudioStop}))
		require.NoError(t, frames.WriteByte('{'))
		c.sendRaw(frames.Bytes())
		c.requireTranscript(transcriptText)
		start := time.Now()

		require.NoError(t, <-shutdown)
		assert.Zero(t, time.Since(start), "Shutdown waited for the rest of the buffered event")
		c.requireClosed()
		require.NoError(t, <-served)
	})
}

func TestShutdown_IdleAfterRequest_IsClosedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withServer(func(s *Server) { s.transcribe = instantTranscriber }))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.record(homeAssistantFormat, silence(homeAssistantFormat, 10*time.Millisecond))
		c.requireTranscript(transcriptText)
		synctest.Wait()
		start := time.Now()

		require.NoError(t, h.server.Shutdown(t.Context()))

		assert.Zero(t, time.Since(start), "Shutdown kept a connection whose request had ended")
		c.requireClosed()
		require.NoError(t, <-served)
	})
}

// heldAcceptListener holds the first connection it accepts until release
// closes, as if Serve were paused between accepting it and tracking it.
type heldAcceptListener struct {
	*pipeListener
	once     sync.Once
	accepted chan struct{}
	release  chan struct{}
}

func (l *heldAcceptListener) Accept() (net.Conn, error) {
	conn, err := l.pipeListener.Accept()
	if err != nil {
		return nil, err
	}

	held := false
	l.once.Do(func() { held = true })
	if held {
		close(l.accepted)
		<-l.release
	}

	return conn, nil
}

func TestShutdown_ConnectionAcceptedNotYetTracked_IsWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t)
		listener := &heldAcceptListener{pipeListener: newPipeListener(), accepted: make(chan struct{}), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(listener.release) })
		t.Cleanup(release)
		served := make(chan error, 1)
		go func() { served <- h.server.Serve(listener) }()
		c := listener.dialClient(t)
		<-listener.accepted

		shutdown := shutdownAsync(t.Context(), h.server)
		synctest.Wait()
		requireNotReturned(t, shutdown, "Shutdown while Serve holds an accepted connection")

		release()
		require.NoError(t, <-shutdown)
		c.requireClosed()
		require.NoError(t, <-served)
	})
}

func TestShutdown_BufferedPingAfterTheRequest_IsAnsweredThenClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withServer(func(s *Server) { s.transcribe = instantTranscriber }))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.send(typeAudioStart, homeAssistantFormat, nil)
		c.send(typeAudioChunk, homeAssistantFormat, silence(homeAssistantFormat, 10*time.Millisecond))
		synctest.Wait()
		shutdown := shutdownAsync(t.Context(), h.server)
		synctest.Wait()

		var frames bytes.Buffer
		require.NoError(t, WriteEvent(&frames, &Event{Type: typeAudioStop}))
		require.NoError(t, WriteEvent(&frames, &Event{Type: typePing}))
		c.sendRaw(frames.Bytes())
		c.requireTranscript(transcriptText)
		c.readType(typePong)

		c.requireClosed()
		require.NoError(t, <-shutdown)
		require.NoError(t, <-served)
	})
}
