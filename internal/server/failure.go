package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

func (s *Server) upstreamFailure(w http.ResponseWriter, r *http.Request, err error) {
	var statusErr *upstream.StatusError
	if errors.As(err, &statusErr) {
		record(r).upstreamStatus = statusErr.Code
		s.writeError(w, clientStatus(statusErr.Code), err)
		return
	}

	if r.Context().Err() != nil {
		s.log.Info("request ended before upstream answered",
			slog.String("request_id", record(r).id),
			slog.Any("error", err),
		)
		s.writeError(w, http.StatusGatewayTimeout, err)
		return
	}

	s.writeError(w, http.StatusBadGateway, err)
}

func (s *Server) streamFailure(r *http.Request, err error) {
	s.logStreamFailure(r, err)
	panic(http.ErrAbortHandler)
}

func (s *Server) logStreamFailure(r *http.Request, err error) {
	if r.Context().Err() != nil {
		s.log.Info("stream ended early",
			slog.String("request_id", record(r).id),
			slog.Any("reason", r.Context().Err()),
			slog.Any("error", err),
		)
		return
	}

	s.log.Error("stream failed", slog.String("request_id", record(r).id), slog.Any("error", err))
}

func unlessRequestEnded(r *http.Request, status int) int {
	if r.Context().Err() != nil {
		return http.StatusGatewayTimeout
	}

	return status
}

func clientStatus(upstreamCode int) int {
	if upstreamCode < http.StatusBadRequest {
		return http.StatusBadGateway
	}

	return upstreamCode
}
