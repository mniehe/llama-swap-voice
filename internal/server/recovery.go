package server

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

const internalErrorText = "Internal Server Error"

func (s *Server) recovered(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			aborted := recover()
			if aborted == nil {
				return
			}

			// net/http closes the connection without a response on this
			// sentinel, which is how a broken audio stream reaches the client.
			if err, ok := aborted.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(aborted)
			}

			s.log.Error("panic recovered",
				slog.Any("panic", aborted),
				slog.String("stack", string(debug.Stack())),
				slog.String("path", r.URL.Path),
			)
			http.Error(w, internalErrorText, http.StatusInternalServerError)
		}()

		next.ServeHTTP(w, r)
	})
}
