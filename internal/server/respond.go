package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

const (
	contentTypeJSON = "application/json"
	contentTypeText = "text/plain; charset=utf-8"
	copyBufferBytes = 32 * 1024
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.logWriteFailure(err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, err error) {
	s.writeJSON(w, status, errorBody{Error: errorDetail{Message: err.Error()}})
}

func (s *Server) writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", contentTypeText)
	w.WriteHeader(status)
	if _, err := io.WriteString(w, text); err != nil {
		s.logWriteFailure(err)
	}
}

func (s *Server) logWriteFailure(err error) {
	s.log.Info("client went away before the response was written", slog.Any("error", err))
}

func streamBody(w http.ResponseWriter, contentType string, body io.Reader) error {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)

	flusher := &flushWriter{w: w, rc: http.NewResponseController(w)}
	_, err := io.CopyBuffer(flusher, body, make([]byte, copyBufferBytes))
	return err
}

type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}

	return n, f.rc.Flush()
}
