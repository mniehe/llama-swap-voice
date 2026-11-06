package server

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"time"
)

const requestIDHeader = "X-Request-Id"

type recordKey struct{}

type requestRecord struct {
	id             string
	voice          string
	format         string
	upstreamStatus int
}

func record(r *http.Request) *requestRecord {
	rec, ok := r.Context().Value(recordKey{}).(*requestRecord)
	if !ok {
		return &requestRecord{}
	}

	return rec
}

func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &requestRecord{id: rand.Text()}
		w.Header().Set(requestIDHeader, rec.id)

		tracked := &trackingWriter{ResponseWriter: w, start: time.Now(), status: http.StatusOK}
		defer s.logRequest(r, rec, tracked)

		next.ServeHTTP(tracked, r.WithContext(context.WithValue(r.Context(), recordKey{}, rec)))
	})
}

func (s *Server) logRequest(r *http.Request, rec *requestRecord, tracked *trackingWriter) {
	s.log.Info("request",
		slog.String("request_id", rec.id),
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
		slog.Int("status", tracked.status),
		slog.String("voice", rec.voice),
		slog.String("format", rec.format),
		slog.Int("upstream_status", rec.upstreamStatus),
		slog.Int64("ttfb_ms", elapsedMillis(tracked.start, tracked.firstByte)),
		slog.Int64("duration_ms", elapsedMillis(tracked.start, time.Now())),
	)
}

type trackingWriter struct {
	http.ResponseWriter
	start       time.Time
	firstByte   time.Time
	status      int
	wroteHeader bool
}

func (t *trackingWriter) WriteHeader(code int) {
	if !t.wroteHeader {
		t.status = code
		t.wroteHeader = true
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(p []byte) (int, error) {
	if t.firstByte.IsZero() && len(p) > 0 {
		t.firstByte = time.Now()
	}
	t.wroteHeader = true

	return t.ResponseWriter.Write(p)
}

func (t *trackingWriter) Unwrap() http.ResponseWriter {
	return t.ResponseWriter
}
