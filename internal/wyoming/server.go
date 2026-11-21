package wyoming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const (
	maxConns     = 64
	idleTimeout  = 60 * time.Second
	writeTimeout = 10 * time.Second
	// No event of a synthesis stream is read and no sentence starts after
	// maxStreamDuration from synthesize-start, so a client cannot keep one
	// open by sending chunks slowly. A sentence already running finishes
	// within its own request timeout.
	maxStreamDuration = 10 * time.Minute
)

// The wait before Serve accepts again after a retried accept error doubles
// from acceptRetryMin to acceptRetryMax, as net/http's does.
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

// retriedAcceptErrors name resources that run out and come back, such as
// file descriptors and memory.
var retriedAcceptErrors = []error{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM}

const (
	programName     = "llama-swap-voice"
	projectURL      = "https://github.com/mniehe/llama-swap-voice"
	defaultSuffix   = " (default)"
	protocolWyoming = "wyoming"
)

// Mode says whether the listener speaks streamed text sentence by sentence
// or waits for the whole text.
type Mode string

const (
	ModeSentences Mode = "sentences"
	ModeWhole     Mode = "whole"
)

// Config holds the Wyoming listener's own settings; the limits come from
// the pipeline.
type Config struct {
	Languages         []string
	Speed             float64
	Streaming         Mode
	StreamingMaxChars int
	Version           string
}

// Server accepts Wyoming connections and serves each on its own goroutine.
type Server struct {
	base         context.Context
	cfg          Config
	pipeline     *pipeline.Pipeline
	log          *slog.Logger
	transcribe   func(ctx context.Context, upload io.Reader) (string, error)
	idleTimeout  time.Duration
	writeTimeout time.Duration
	// maxStreamDuration is a field so that a test can shorten it.
	maxStreamDuration time.Duration

	mu     sync.Mutex
	closed bool
	// stopped is closed together with the first change of closed to true,
	// which ends a Serve call's wait before it accepts again.
	stopped   chan struct{}
	listeners map[net.Listener]struct{}
	// conns says for each connection whether it is busy: a request is open
	// or its reply is being written. Shutdown keeps busy connections open.
	conns map[net.Conn]bool

	goroutines sync.WaitGroup
	drained    func() <-chan struct{}
}

// New returns a server whose request contexts all derive from base, so
// cancelling base ends every request.
func New(base context.Context, cfg *Config, p *pipeline.Pipeline, log *slog.Logger) *Server {
	s := &Server{
		base:              base,
		cfg:               *cfg,
		pipeline:          p,
		log:               log,
		transcribe:        p.Transcribe,
		idleTimeout:       idleTimeout,
		writeTimeout:      writeTimeout,
		maxStreamDuration: maxStreamDuration,
		stopped:           make(chan struct{}),
		listeners:         map[net.Listener]struct{}{},
		conns:             map[net.Conn]bool{},
	}
	s.drained = sync.OnceValue(s.waitGoroutines)

	return s
}

// Serve accepts connections on l until Shutdown or Close, when it returns
// nil. An accept error in retriedAcceptErrors is logged and Serve accepts
// again after a wait that grows with each such error in a row. Any other
// accept error ends Serve and is returned, joined with any error from
// closing l, even when Close runs meanwhile. Serve closes l whenever it
// returns.
func (s *Server) Serve(l net.Listener) error {
	if !s.trackListener(l) {
		return closeIgnoringClosed(l)
	}
	defer s.goroutines.Done()

	var retryDelay time.Duration
	for {
		conn, err := l.Accept()
		if err != nil {
			if s.isClosed() || !isRetriedAcceptError(err) {
				return s.stopServing(l, err)
			}
			retryDelay = nextRetryDelay(retryDelay)
			s.log.Warn("wyoming accept failed, retrying", slog.Any("error", err), slog.Duration("delay", retryDelay))
			s.waitUnlessStopped(retryDelay)
			continue
		}
		retryDelay = 0

		if !s.trackConn(conn) {
			s.log.Warn("wyoming connection refused", slog.String("remote", conn.RemoteAddr().String()), slog.Int("max_connections", maxConns))
			s.closeConn(conn)
			continue
		}

		go func() {
			defer s.goroutines.Done()
			s.serveConn(conn)
		}()
	}
}

// isRetriedAcceptError reports whether err is in retriedAcceptErrors.
func isRetriedAcceptError(err error) bool {
	for _, target := range retriedAcceptErrors {
		if errors.Is(err, target) {
			return true
		}
	}

	return false
}

func nextRetryDelay(previous time.Duration) time.Duration {
	if previous == 0 {
		return acceptRetryMin
	}

	return min(2*previous, acceptRetryMax)
}

// waitUnlessStopped returns after delay, or at once when Shutdown or Close
// runs.
func (s *Server) waitUnlessStopped(delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-s.stopped:
	}
}

// Shutdown stops every Serve call, closes idle connections and refuses new
// requests on the others. It then waits until each Serve call, each of the
// requests and each connection's goroutine has returned, or until ctx ends,
// when it returns ctx.Err(); Close then ends the connections that remain.
// After Close, Shutdown waits for the goroutines that Close did not.
func (s *Server) Shutdown(ctx context.Context) error {
	closeErr := s.closeIdle()

	select {
	case <-s.drained():
		return closeErr
	case <-ctx.Done():
		return errors.Join(ctx.Err(), closeErr)
	}
}

// markClosed runs with s.mu held.
func (s *Server) markClosed() {
	if !s.closed {
		close(s.stopped)
	}
	s.closed = true
}

func (s *Server) closeIdle() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.markClosed()

	errs := make([]error, 0, len(s.listeners)+len(s.conns))
	for l := range s.listeners {
		errs = append(errs, closeIgnoringClosed(l))
	}
	for conn, busy := range s.conns {
		if !busy {
			errs = append(errs, closeIgnoringClosed(conn))
		}
	}

	return errors.Join(errs...)
}

// waitGoroutines runs once, after closed is set, so no Serve call or
// connection goroutine can start while it waits.
func (s *Server) waitGoroutines() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		s.goroutines.Wait()
		close(done)
	}()

	return done
}

// beginWork marks conn busy, unless Shutdown or Close has begun.
func (s *Server) beginWork(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return false
	}
	if _, tracked := s.conns[conn]; tracked {
		s.conns[conn] = true
	}

	return true
}

// markIdle marks conn idle and reports whether the server still serves.
func (s *Server) markIdle(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, tracked := s.conns[conn]; tracked {
		s.conns[conn] = false
	}

	return !s.closed
}

// Close stops every Serve call and closes every open connection. It does not
// wait for the connection goroutines.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.markClosed()

	errs := make([]error, 0, len(s.listeners)+len(s.conns))
	for l := range s.listeners {
		errs = append(errs, closeIgnoringClosed(l))
	}
	for conn := range s.conns {
		errs = append(errs, closeIgnoringClosed(conn))
	}

	return errors.Join(errs...)
}

// stopServing closes l before forgetting it, so Close cannot miss a listener
// that is still open. A close error never hides the accept error. Serve was
// stopped only when the server is closed and the accept error is one that
// Close causes or one that Serve would have retried: Close can also run
// while a real accept failure is being handled.
func (s *Server) stopServing(l net.Listener, acceptErr error) error {
	closeErr := closeIgnoringClosed(l)
	s.forgetListener(l)

	stopped := errors.Is(acceptErr, net.ErrClosed) || isRetriedAcceptError(acceptErr)
	if !s.isClosed() || !stopped {
		return errors.Join(fmt.Errorf("wyoming accept: %w", acceptErr), closeErr)
	}

	if closeErr != nil {
		s.log.Warn("closing wyoming listener", slog.Any("error", closeErr))
	}

	return nil
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closed
}

// trackListener counts the Serve call among the goroutines, so Shutdown also
// waits for a connection that Serve has accepted but not yet tracked.
func (s *Server) trackListener(l net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return false
	}
	s.listeners[l] = struct{}{}
	s.goroutines.Add(1)

	return true
}

func (s *Server) forgetListener(l net.Listener) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.listeners, l)
}

func (s *Server) trackConn(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || len(s.conns) >= maxConns {
		return false
	}
	s.conns[conn] = false
	s.goroutines.Add(1)

	return true
}

func (s *Server) forgetConn(conn net.Conn) {
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()

	s.closeConn(conn)
}

func (s *Server) closeConn(conn net.Conn) {
	if err := closeIgnoringClosed(conn); err != nil {
		s.log.Debug("closing wyoming connection", slog.String("remote", conn.RemoteAddr().String()), slog.Any("error", err))
	}
}

func closeIgnoringClosed(c io.Closer) error {
	if err := c.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}

	return nil
}

// info lists one ASR program and one TTS program. The default voice comes
// first and says so; the pipeline's catalogue keeps the rest sorted.
func (s *Server) info() info {
	catalogue := s.pipeline.Catalogue()
	languages := s.cfg.Languages
	if languages == nil {
		languages = []string{}
	}

	voices := make([]ttsVoice, 0, len(catalogue.Voices))
	for _, name := range catalogue.Voices {
		voice := ttsVoice{artefact: s.artefact(name, name), Languages: languages}
		if name != catalogue.DefaultVoice {
			voices = append(voices, voice)
			continue
		}
		voice.Description = name + defaultSuffix
		voices = append([]ttsVoice{voice}, voices...)
	}

	asr := asrProgram{
		artefact: s.artefact(programName, catalogue.ASRModel),
		Models:   []asrModel{{artefact: s.artefact(catalogue.ASRModel, ""), Languages: languages}},
	}
	asr.Version = s.cfg.Version

	tts := ttsProgram{
		artefact:                    s.artefact(programName, catalogue.TTSModel),
		Voices:                      voices,
		SupportsSynthesizeStreaming: s.cfg.Streaming == ModeSentences,
	}
	tts.Version = s.cfg.Version

	return info{ASR: []asrProgram{asr}, TTS: []ttsProgram{tts}}
}

func (s *Server) artefact(name, description string) artefact {
	return artefact{
		Name:        name,
		Attribution: attribution{Name: programName, URL: projectURL},
		Installed:   true,
		Description: description,
	}
}
