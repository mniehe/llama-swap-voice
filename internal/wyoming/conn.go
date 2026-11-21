package wyoming

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

// Error codes in the error event, one per pipeline kind.
const (
	codeInvalid      = "invalid"
	codeBusy         = "busy"
	codeTimeout      = "timeout"
	codeUpstream     = "upstream"
	codeShuttingDown = "shutting-down"
	codeInternal     = "internal"
)

// Request outcomes in the request log besides the error codes.
const (
	outcomeOK     = "ok"
	outcomeClosed = "closed"
	outcomePanic  = "panic"
)

type state int

// A connection in stateSynthesising reads no event until the reply ends.
const (
	stateIdle state = iota
	stateRecording
	stateSynthesising
)

// connection is one client's event loop. Only its own goroutine touches it.
type connection struct {
	server    *Server
	conn      net.Conn
	in        *bufio.Reader
	state     state
	request   *request
	idleSince time.Time
}

// request is one Wyoming request from its first event to its reply.
type request struct {
	id        string
	event     string
	voice     string
	started   time.Time
	ctx       context.Context
	deadline  time.Time
	disarm    func() bool
	cleanup   func()
	format    audioFormat
	recording []byte
	pcm       io.Closer
}

func (s *Server) serveConn(conn net.Conn) {
	c := &connection{server: s, conn: conn, in: bufio.NewReader(conn), idleSince: time.Now()}
	defer s.forgetConn(conn)
	defer s.recoverPanic(conn)

	outcome := outcomePanic
	defer func() { c.endRequest(outcome) }()

	err := c.serve()
	outcome = c.outcomeOf(err)
	c.logEnd(err)
}

// recoverPanic keeps a panic in one connection from ending the process. The
// request cleanup has already run by then, as a deferred call.
func (s *Server) recoverPanic(conn net.Conn) {
	failure := recover()
	if failure == nil {
		return
	}

	s.log.Error("panic recovered",
		slog.Any("panic", failure),
		slog.String("stack", string(debug.Stack())),
		slog.String("remote", conn.RemoteAddr().String()),
	)
}

func (c *connection) serve() error {
	for {
		deadline := c.readDeadline(time.Now())
		if c.request == nil && !c.server.markIdle(c.conn) {
			if c.in.Buffered() == 0 {
				return errShuttingDown
			}
			deadline = expiredDeadline
		}

		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return err
		}

		event, err := ReadEvent(c.in)
		if err != nil {
			return c.readFailed(err)
		}

		// An event the reader had already buffered was not bound by the
		// read deadline, so the idle deadline is checked again here.
		if c.request == nil && !time.Now().Before(c.idleDeadline()) {
			return errIdle
		}

		if handleErr := c.handle(event); handleErr != nil {
			return c.refuse(handleErr)
		}
	}
}

// errShuttingDown ends a connection that Shutdown kept open for its request
// once that request is over. Events the server had already read are handled
// first, so a request among them is refused with shutting-down. They are
// read under expiredDeadline, which fetches nothing more from the socket: a
// request sent later finds the connection closed, and a partly read event
// ends the connection at once.
var errShuttingDown = errors.New("the server is shutting down")

// expiredDeadline lies in the past, so a read under it returns only what the
// reader has buffered.
var expiredDeadline = time.Unix(0, 0)

// errIdle ends a connection whose idle deadline passed between reading an
// event and handling it, the same way a read past that deadline does.
var errIdle = fmt.Errorf("no request began before the idle deadline: %w", os.ErrDeadlineExceeded)

// idleDeadline is when a connection with no request open must have started
// one: idleTimeout after its accept or after its last reply was written.
// Describe, ping and unknown events do not extend it, so pinging clients
// cannot hold every connection slot.
func (c *connection) idleDeadline() time.Time {
	return c.idleSince.Add(c.server.idleTimeout)
}

// readDeadline covers one whole event. With no request open it is the idle
// deadline. With a request open, each event gets idleTimeout, clamped to the
// request deadline, so a client cannot hold an ASR slot past that deadline by
// sending slowly.
func (c *connection) readDeadline(now time.Time) time.Time {
	if c.request == nil {
		return c.idleDeadline()
	}

	deadline := now.Add(c.server.idleTimeout)
	if !c.request.deadline.IsZero() && c.request.deadline.Before(deadline) {
		return c.request.deadline
	}

	return deadline
}

func (c *connection) readFailed(err error) error {
	if errors.Is(err, errTooLarge) || errors.Is(err, errMalformed) {
		return c.refuse(&pipeline.Error{Kind: pipeline.KindInvalid, Message: err.Error(), Err: err})
	}

	return err
}

func (c *connection) handle(event *Event) error {
	switch event.Type {
	case typeDescribe:
		return c.write(typeInfo, c.server.info())
	case typePing:
		return c.pong(event)
	case typeTranscribe:
		return c.requireState(stateIdle, event.Type)
	case typeAudioStart:
		return c.audioStart(event)
	case typeAudioChunk:
		return c.audioChunk(event)
	case typeAudioStop:
		return c.audioStop()
	case typeSynthesize:
		return c.synthesize(event)
	}

	return nil
}

func (c *connection) requireState(want state, eventType string) error {
	if c.state == want {
		return nil
	}

	if want == stateIdle {
		return invalid("%s is not allowed while audio is being recorded", eventType)
	}

	return invalid("%s is only allowed after audio-start", eventType)
}

func invalid(format string, args ...any) *pipeline.Error {
	return &pipeline.Error{Kind: pipeline.KindInvalid, Message: fmt.Sprintf(format, args...)}
}

// pong copies the ping's text; text that is not a string becomes null.
func (c *connection) pong(event *Event) error {
	var ping pingData
	if err := decode(event, &ping); err != nil {
		ping.Text = nil
	}

	return c.write(typePong, ping)
}

func decode(event *Event, dst any) error {
	if len(event.Data) == 0 {
		return nil
	}

	return json.Unmarshal(event.Data, dst)
}

// refuse ends the request, then tells the client why when the failure is
// the pipeline's or the client's. The connection closes either way. Ending
// the request first frees its slot before the client reads the error.
//
// The pipeline reports any ended request context as a timeout, so a request
// cancelled through the base context is named here, before release cancels
// the context too.
func (c *connection) refuse(err error) error {
	var failed *pipeline.Error
	if !errors.As(err, &failed) {
		return err
	}

	code := errorCode(failed.Kind)
	if c.requestCancelled() {
		code = codeShuttingDown
	}
	c.endRequest(code)

	if writeErr := c.reply(typeError, errorData{Text: failed.Message, Code: code}, nil); writeErr != nil {
		return errors.Join(err, writeErr)
	}

	return err
}

// requestCancelled reports whether the open request's context was cancelled,
// which before release only the server's base context does, at shutdown.
func (c *connection) requestCancelled() bool {
	req := c.request
	return req != nil && req.ctx != nil && errors.Is(req.ctx.Err(), context.Canceled)
}

func errorCode(kind pipeline.Kind) string {
	switch kind {
	case pipeline.KindInvalid:
		return codeInvalid
	case pipeline.KindBusy:
		return codeBusy
	case pipeline.KindTimeout:
		return codeTimeout
	case pipeline.KindUpstream:
		return codeUpstream
	case pipeline.KindShuttingDown:
		return codeShuttingDown
	default:
		return codeInternal
	}
}

// write sends an event that answers describe or ping.
func (c *connection) write(eventType string, data any) error {
	return c.writeBy(c.writeDeadline(time.Now()), eventType, data, nil)
}

// reply sends an event of a request's reply, which gets the whole
// writeTimeout: the request deadline bounds the work behind each event, and
// after the last event the idle period has not begun.
func (c *connection) reply(eventType string, data any, payload []byte) error {
	return c.writeBy(time.Now().Add(c.server.writeTimeout), eventType, data, payload)
}

// writeDeadline gives each write writeTimeout, cut short with no request open
// by the idle deadline, so a client that does not read cannot outstay it.
func (c *connection) writeDeadline(now time.Time) time.Time {
	deadline := now.Add(c.server.writeTimeout)
	if c.request == nil && c.idleDeadline().Before(deadline) {
		return c.idleDeadline()
	}

	return deadline
}

// writeBy sends one event; nil data sends an event without data.
func (c *connection) writeBy(deadline time.Time, eventType string, data any, payload []byte) error {
	event := &Event{Type: eventType, Payload: payload}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("encoding %s: %w", eventType, err)
		}
		event.Data = raw
	}

	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}

	return WriteEvent(c.conn, event)
}

// beginRequest records a request before it is admitted, so a refused one
// still gets its log line.
func (c *connection) beginRequest(eventType string) {
	c.request = &request{id: rand.Text(), event: eventType, started: time.Now()}
}

// admit takes a slot for the current request and ties its context to the
// connection: a request that ends closes the connection, which wakes a
// blocked read. The cleanup undoes all of it exactly once.
//
// A hook that has started may not have closed the connection yet, and its
// close would then land on whatever request comes next. So whoever disarms
// the hook too late closes the connection itself, and it is never reused.
//
// Once Shutdown has begun, no request is admitted.
func (c *connection) admit(work pipeline.Work) error {
	p := c.server.pipeline
	if !c.server.beginWork(c.conn) || !p.Enter() {
		return pipeline.ErrShuttingDown
	}

	deadline := p.RequestDeadline(time.Now())
	ctx, release, err := p.Admit(pipeline.WithRequestID(c.server.base, c.request.id), work, deadline)
	if err != nil {
		p.Leave()
		return err
	}

	// Disarming the hook before release matters: release cancels ctx, and
	// the hook would then close a connection that is still in use. The stop
	// result is kept because a second stop call always reports false.
	disarm := sync.OnceValue(context.AfterFunc(ctx, func() { c.server.closeConn(c.conn) }))
	req := c.request
	req.ctx = ctx
	req.deadline = deadline
	req.disarm = disarm
	req.cleanup = sync.OnceFunc(func() {
		if !disarm() {
			c.server.closeConn(c.conn)
		}
		c.server.closeSpeech(req)
		release()
		p.Leave()
	})

	return nil
}

// endRequest runs the request's cleanup and writes its log line. Every exit
// path calls it; with no request open it does nothing.
func (c *connection) endRequest(outcome string) {
	req := c.request
	if req == nil {
		return
	}
	c.request = nil
	c.state = stateIdle

	if req.cleanup != nil {
		req.cleanup()
	}

	c.server.log.Info("request",
		slog.String("request_id", req.id),
		slog.String("protocol", protocolWyoming),
		slog.String("event", req.event),
		slog.String("voice", req.voice),
		slog.Int64("duration_ms", time.Since(req.started).Milliseconds()),
		slog.String("outcome", outcome),
	)
}

// outcomeOf names how a connection's open request ended. A cancelled context
// was cancelled through the server's base context, at shutdown, even when
// that is noticed after the deadline. Otherwise the read deadline and the
// context's timer fire at the same instant in either order, so the deadline
// itself also counts.
func (c *connection) outcomeOf(err error) string {
	if c.requestCancelled() {
		return codeShuttingDown
	}
	if req := c.request; req != nil && req.ctx != nil {
		if req.ctx.Err() != nil || !time.Now().Before(req.deadline) {
			return codeTimeout
		}
	}

	var failed *pipeline.Error
	if errors.As(err, &failed) {
		return errorCode(failed.Kind)
	}

	return outcomeClosed
}

func (c *connection) logEnd(err error) {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return
	}

	c.server.log.Debug("wyoming connection ended", slog.String("remote", c.conn.RemoteAddr().String()), slog.Any("error", err))
}
