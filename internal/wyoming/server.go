package wyoming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const (
	maxConns     = 64
	idleTimeout  = 60 * time.Second
	writeTimeout = 10 * time.Second
)

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

	mu        sync.Mutex
	closed    bool
	listeners map[net.Listener]struct{}
	conns     map[net.Conn]struct{}
}

// New returns a server whose request contexts all derive from base, so
// cancelling base ends every request.
func New(base context.Context, cfg *Config, p *pipeline.Pipeline, log *slog.Logger) *Server {
	return &Server{
		base:         base,
		cfg:          *cfg,
		pipeline:     p,
		log:          log,
		transcribe:   p.Transcribe,
		idleTimeout:  idleTimeout,
		writeTimeout: writeTimeout,
		listeners:    map[net.Listener]struct{}{},
		conns:        map[net.Conn]struct{}{},
	}
}

// Serve accepts connections on l until Close, when it returns nil. Any
// other accept error ends Serve and is returned, joined with any error from
// closing l, even when Close runs meanwhile. Serve closes l whenever it
// returns.
func (s *Server) Serve(l net.Listener) error {
	if !s.trackListener(l) {
		return closeIgnoringClosed(l)
	}

	for {
		conn, err := l.Accept()
		if err != nil {
			return s.stopServing(l, err)
		}

		if !s.trackConn(conn) {
			s.log.Warn("wyoming connection refused", slog.String("remote", conn.RemoteAddr().String()), slog.Int("max_connections", maxConns))
			s.closeConn(conn)
			continue
		}

		go s.serveConn(conn)
	}
}

// Close stops every Serve call and closes every open connection. It does not
// wait for the connection goroutines.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true

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
// that is still open. A close error never hides the accept error. Only the
// accept error that Close itself causes means Serve was stopped: Close can
// also run while a real accept failure is being handled.
func (s *Server) stopServing(l net.Listener, acceptErr error) error {
	closeErr := closeIgnoringClosed(l)
	s.forgetListener(l)

	if !s.isClosed() || !errors.Is(acceptErr, net.ErrClosed) {
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

func (s *Server) trackListener(l net.Listener) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return false
	}
	s.listeners[l] = struct{}{}

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
	s.conns[conn] = struct{}{}

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

	tts := ttsProgram{artefact: s.artefact(programName, catalogue.TTSModel), Voices: voices}
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
