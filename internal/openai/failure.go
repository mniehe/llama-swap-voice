package openai

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const retryAfterHeader = "Retry-After"

// failure answers a pipeline error with the status each kind had before the
// pipeline existed, and records the upstream status for the request log.
func (s *Server) failure(w http.ResponseWriter, r *http.Request, err error) {
	var failed *pipeline.Error
	if !errors.As(err, &failed) {
		s.log.Error("unexpected pipeline failure", slog.String("request_id", record(r).id), slog.Any("error", err))
		s.writeError(w, http.StatusInternalServerError, errInternal)
		return
	}

	if failed.UpstreamStatus != 0 {
		record(r).upstreamStatus = failed.UpstreamStatus
	}

	switch failed.Kind {
	case pipeline.KindInvalid:
		s.writeError(w, http.StatusBadRequest, failed)
	case pipeline.KindBusy, pipeline.KindShuttingDown:
		s.reject(w, failed)
	case pipeline.KindTimeout:
		s.writeError(w, http.StatusGatewayTimeout, failed)
	case pipeline.KindUpstream:
		s.upstreamError(w, failed)
	default:
		s.writeError(w, http.StatusInternalServerError, failed)
	}
}

func (s *Server) upstreamError(w http.ResponseWriter, failed *pipeline.Error) {
	if failed.Upstream == nil {
		s.writeError(w, http.StatusBadGateway, failed)
		return
	}

	if failed.Upstream.RetryAfter != "" {
		w.Header().Set(retryAfterHeader, failed.Upstream.RetryAfter)
	}
	s.writeError(w, clientStatus(failed.Upstream.Code), failed)
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
