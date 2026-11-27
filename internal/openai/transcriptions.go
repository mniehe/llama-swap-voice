package openai

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const (
	formatJSON        = "json"
	formatVerboseJSON = "verbose_json"
	formatText        = "text"

	uploadMemory     = 8 << 20
	uploadFieldName  = "file"
	formatFieldName  = "response_format"
	missingFileError = "multipart field \"file\" is required"
)

var transcriptFormats = map[string]bool{
	formatJSON:        true,
	formatVerboseJSON: true,
	formatText:        true,
}

type transcriptBody struct {
	Text string `json:"text"`
}

func (s *Server) handleTranscription(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.pipeline.Limits().MaxUploadBytes)
	if err := r.ParseMultipartForm(uploadMemory); err != nil {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("invalid multipart body: %w", err))
		return
	}
	defer s.removeUploads(r)

	format := r.FormValue(formatFieldName)
	if format == "" {
		format = formatJSON
	}
	record(r).format = format

	if !transcriptFormats[format] {
		s.writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported response_format %q", format))
		return
	}

	file, _, err := r.FormFile(uploadFieldName)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, errors.New(missingFileError))
		return
	}
	defer file.Close()

	text, err := s.pipeline.Transcribe(r.Context(), file, pipeline.SourceOpenAI)
	if err != nil {
		s.failure(w, r, err)
		return
	}
	record(r).upstreamStatus = http.StatusOK

	s.writeTranscript(w, format, text)
}

func (s *Server) removeUploads(r *http.Request) {
	if err := r.MultipartForm.RemoveAll(); err != nil {
		s.log.Warn("could not remove spooled upload", slog.String("request_id", record(r).id), slog.Any("error", err))
	}
}

func (s *Server) writeTranscript(w http.ResponseWriter, format, text string) {
	if format == formatText {
		s.writeText(w, http.StatusOK, text)
		return
	}

	s.writeJSON(w, http.StatusOK, transcriptBody{Text: text})
}
