package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/transcode"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	controlRequestTimeout = 10 * time.Second
	responseWriteSlack    = 2 * time.Second
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
	TTSConcurrency    int
	ASRPrefixRegex    string
	TranscodeASRInput bool
}

type Server struct {
	cfg            Config
	upstream       *upstream.Client
	encoder        *transcode.Encoder
	prefix         *regexp.Regexp
	voices         []string
	slots          chan struct{}
	ttsGate        *ttsGate
	log            *slog.Logger
	shuttingDown   atomic.Bool
	admission      admission
	controlTimeout time.Duration
}

func New(cfg *Config, client *upstream.Client, log *slog.Logger) (*Server, error) {
	prefix, err := compilePrefix(cfg.ASRPrefixRegex)
	if err != nil {
		return nil, err
	}

	return &Server{
		cfg:            *cfg,
		upstream:       client,
		encoder:        transcode.New(cfg.FFmpeg),
		prefix:         prefix,
		voices:         voiceNames(cfg.Voices),
		slots:          make(chan struct{}, cfg.MaxConcurrent),
		ttsGate:        newTTSGate(cfg.TTSConcurrency),
		log:            log,
		controlTimeout: controlRequestTimeout,
	}, nil
}

type route struct {
	method  string
	path    string
	handler http.HandlerFunc
}

func (s *Server) routes() []route {
	probes := []route{
		{http.MethodGet, "/health", s.handleHealth},
		{http.MethodGet, "/internal/liveness", s.handleLiveness},
		{http.MethodGet, "/internal/readiness", s.handleReadiness},
	}
	api := []route{
		{http.MethodPost, "/audio/speech", s.limited(s.handleSpeech)},
		{http.MethodPost, "/audio/transcriptions", s.limited(s.handleTranscription)},
		{http.MethodGet, "/models", s.handleModels},
		{http.MethodGet, "/audio/voices", s.handleVoices},
	}

	routes := make([]route, 0, len(probes)+len(routePrefixes)*len(api))
	routes = append(routes, probes...)
	for _, prefix := range routePrefixes {
		for _, rt := range api {
			rt.path = prefix + rt.path
			routes = append(routes, rt)
		}
	}

	return routes
}

// A pattern with a method outranks the same path without one, so the
// method-less patterns only see wrong methods and "/" only unknown paths.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	allowed := map[string][]string{}
	for _, rt := range s.routes() {
		mux.HandleFunc(rt.method+" "+rt.path, rt.handler)
		allowed[rt.path] = append(allowed[rt.path], servedMethods(rt.method)...)
	}

	for path, methods := range allowed {
		mux.HandleFunc(path, s.methodNotAllowed(methods))
	}
	mux.HandleFunc("/", s.handleNotFound)

	return s.logged(s.recovered(s.bounded(s.admitted(mux))))
}

func servedMethods(method string) []string {
	if method == http.MethodGet {
		return []string{http.MethodGet, http.MethodHead}
	}

	return []string{method}
}

func (s *Server) methodNotAllowed(methods []string) http.HandlerFunc {
	allow := strings.Join(methods, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		s.writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s is not allowed on %s", r.Method, r.URL.Path))
	}
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, http.StatusNotFound, fmt.Errorf("%s not found", r.URL.Path))
}

// SetShuttingDown makes the readiness probe fail so load balancers stop
// routing new requests while in-flight ones drain.
func (s *Server) SetShuttingDown() {
	s.shuttingDown.Store(true)
}

// Every request gets connection deadlines before any handler runs, so a
// stalled request body cannot hold a connection open. Audio routes replace
// these deadlines with their own in limited.
func (s *Server) bounded(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(s.controlTimeout)
		s.setDeadlines(w, deadline, deadline)
		next.ServeHTTP(w, r)
	})
}

// The request context ends at deadline, so writes get responseWriteSlack
// longer: otherwise the 504 a timeout produces never reaches the client.
func (s *Server) limited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deadline := time.Now().Add(s.cfg.RequestTimeout)
		s.setDeadlines(w, deadline, deadline.Add(responseWriteSlack))

		select {
		case s.slots <- struct{}{}:
		default:
			s.reject(w, errBusy)
			return
		}
		defer func() { <-s.slots }()

		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()

		next(w, r.WithContext(ctx))
	}
}

// The request body is left unread, and closing the connection stops net/http
// from trying to drain it after the response.
func (s *Server) reject(w http.ResponseWriter, err error) {
	w.Header().Set("Connection", "close")
	s.writeError(w, http.StatusServiceUnavailable, err)
}

func (s *Server) setDeadlines(w http.ResponseWriter, read, write time.Time) {
	rc := http.NewResponseController(w)
	err := errors.Join(rc.SetReadDeadline(read), rc.SetWriteDeadline(write))
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
