package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	formatJSON        = "json"
	formatVerboseJSON = "verbose_json"
	formatText        = "text"

	maxUploadBytes   = 25 << 20
	uploadMemory     = 8 << 20
	transcodedName   = "audio.wav"
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
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
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

	file, header, err := r.FormFile(uploadFieldName)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, errors.New(missingFileError))
		return
	}
	defer file.Close()

	rec, err := s.recording(r, header.Filename, file)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	text, err := s.upstream.Transcribe(r.Context(), rec)
	if err != nil {
		s.upstreamFailure(w, r, err)
		return
	}
	record(r).upstreamStatus = http.StatusOK

	s.writeTranscript(w, format, s.stripPrefix(text))
}

func (s *Server) recording(r *http.Request, name string, data io.Reader) (upstream.Recording, error) {
	if !s.cfg.TranscodeASRInput {
		return upstream.Recording{Name: name, Data: data}, nil
	}

	wav, err := s.encoder.ToASRInput(r.Context(), data)
	if err != nil {
		return upstream.Recording{}, fmt.Errorf("could not decode audio: %w", err)
	}

	return upstream.Recording{Name: transcodedName, Data: bytes.NewReader(wav)}, nil
}

func (s *Server) stripPrefix(text string) string {
	if s.prefix == nil {
		return strings.TrimSpace(text)
	}

	loc := s.prefix.FindStringIndex(text)
	if len(loc) == 0 || loc[0] != 0 {
		return strings.TrimSpace(text)
	}

	return strings.TrimSpace(text[loc[1]:])
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
