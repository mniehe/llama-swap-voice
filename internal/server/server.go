package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sync/atomic"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/transcode"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

var routePrefixes = []string{"/v1", ""}

var errBusy = errors.New("too many concurrent requests, retry shortly")

type Config struct {
	TTSModel          string
	ASRModel          string
	Voices            map[string]string
	DefaultVoice      string
	Lang              string
	FFmpeg            string
	RequestTimeout    time.Duration
	MaxInputChars     int
	MaxConcurrent     int
	ASRPrefixRegex    string
	TranscodeASRInput bool
}

type Server struct {
	cfg          Config
	upstream     *upstream.Client
	encoder      *transcode.Encoder
	prefix       *regexp.Regexp
	voices       []string
	slots        chan struct{}
	log          *slog.Logger
	shuttingDown atomic.Bool
}

func New(cfg *Config, client *upstream.Client, log *slog.Logger) (*Server, error) {
	prefix, err := compilePrefix(cfg.ASRPrefixRegex)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:      *cfg,
		upstream: client,
		encoder:  transcode.New(cfg.FFmpeg),
		prefix:   prefix,
		voices:   voiceNames(cfg.Voices),
		slots:    make(chan struct{}, cfg.MaxConcurrent),
		log:      log,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, prefix := range routePrefixes {
		mux.HandleFunc("POST "+prefix+"/audio/speech", s.limited(s.handleSpeech))
		mux.HandleFunc("POST "+prefix+"/audio/transcriptions", s.limited(s.handleTranscription))
		mux.HandleFunc("GET "+prefix+"/models", s.handleModels)
		mux.HandleFunc("GET "+prefix+"/audio/voices", s.handleVoices)
	}
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /internal/liveness", s.handleLiveness)
	mux.HandleFunc("GET /internal/readiness", s.handleReadiness)

	return s.recovered(s.logged(mux))
}

// SetShuttingDown makes the readiness probe fail so load balancers stop
// routing new requests while in-flight ones drain.
func (s *Server) SetShuttingDown() {
	s.shuttingDown.Store(true)
}

func (s *Server) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case s.slots <- struct{}{}:
		default:
			s.writeError(w, http.StatusServiceUnavailable, errBusy)
			return
		}
		defer func() { <-s.slots }()

		deadline := time.Now().Add(s.cfg.RequestTimeout)
		s.setDeadlines(w, deadline)

		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()

		next(w, r.WithContext(ctx))
	}
}

func (s *Server) setDeadlines(w http.ResponseWriter, deadline time.Time) {
	rc := http.NewResponseController(w)
	err := errors.Join(rc.SetReadDeadline(deadline), rc.SetWriteDeadline(deadline))
	if err != nil {
		s.log.Warn("could not set connection deadlines", slog.Any("error", err))
	}
}

func compilePrefix(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}

	return regexp.Compile(pattern)
}

func voiceNames(voices map[string]string) []string {
	names := make([]string, 0, len(voices))
	for name := range voices {
		names = append(names, name)
	}
	slices.Sort(names)

	return names
}

func elapsedMillis(start, end time.Time) int64 {
	if end.IsZero() {
		return -1
	}

	return end.Sub(start).Milliseconds()
}
