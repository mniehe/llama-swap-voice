package openai

import (
	"net/http"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

func (s *Server) admitted(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.pipeline.Enter() {
			s.failure(w, r, pipeline.ErrShuttingDown)
			return
		}
		defer s.pipeline.Leave()

		next.ServeHTTP(w, r)
	})
}
