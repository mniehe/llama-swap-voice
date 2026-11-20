package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const (
	contentTypeWAV = "audio/wav"
	contentTypeMP3 = "audio/mpeg"
	contentTypeOgg = "audio/ogg"
	contentTypePCM = "audio/pcm"

	defaultSpeechFormat = "mp3"
	maxSpeechBodyBytes  = 1 << 20
)

// newEncoder is a variable so tests can substitute an encoder that fails.
var newEncoder = audio.NewEncoder

type outputFormat struct {
	contentType string
	codec       audio.Format
}

var outputFormats = map[string]outputFormat{
	"wav":  {contentType: contentTypeWAV, codec: audio.WAV},
	"pcm":  {contentType: contentTypePCM, codec: audio.PCM},
	"mp3":  {contentType: contentTypeMP3, codec: audio.MP3},
	"opus": {contentType: contentTypeOgg, codec: audio.Opus},
}

type speechRequest struct {
	Model          string   `json:"model"`
	Voice          string   `json:"voice"`
	Input          string   `json:"input"`
	ResponseFormat string   `json:"response_format"`
	Speed          *float64 `json:"speed"`
	Instructions   string   `json:"instructions"`
}

type speechJob struct {
	job    pipeline.SpeechJob
	format string
	output outputFormat
	speed  float64
}

func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	job, err := s.parseSpeech(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	rec := record(r)
	rec.voice = job.job.Voice
	rec.format = job.format

	speech, err := s.pipeline.Speak(r.Context(), job.job, job.speed)
	if err != nil {
		var failed *pipeline.Error
		if errors.As(err, &failed) {
			s.logRetry(rec, failed.Retried)
		}
		s.failure(w, r, err)
		return
	}
	defer s.closeSpeech(speech)

	rec.upstreamStatus = speech.UpstreamStatus
	s.logRetry(rec, speech.Retried)

	s.encoded(w, r, speech, &job)
}

func (s *Server) logRetry(rec *requestRecord, retried bool) {
	if retried {
		s.log.Info("upstream speech retried", slog.String("request_id", rec.id))
	}
}

// Validation runs in a fixed order, input and voice before format and speed,
// so the same bad request always gets the same message.
func (s *Server) parseSpeech(w http.ResponseWriter, r *http.Request) (speechJob, error) {
	var req speechRequest
	body := http.MaxBytesReader(w, r.Body, maxSpeechBodyBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return speechJob{}, fmt.Errorf("invalid JSON body: %w", err)
	}

	job, err := s.pipeline.PrepareSpeech(req.Voice, req.Input)
	if err != nil {
		return speechJob{}, err
	}

	format, output, err := resolveFormat(req.ResponseFormat)
	if err != nil {
		return speechJob{}, err
	}

	speed, err := pipeline.ResolveSpeed(req.Speed)
	if err != nil {
		return speechJob{}, err
	}

	return speechJob{job: job, format: format, output: output, speed: speed}, nil
}

func resolveFormat(name string) (string, outputFormat, error) {
	if name == "" {
		name = defaultSpeechFormat
	}

	output, ok := outputFormats[name]
	if !ok {
		return "", outputFormat{}, fmt.Errorf("unsupported response_format %q", name)
	}

	return name, output, nil
}

// The pipeline holds the 200 back until the first samples arrive; encoded
// holds it until the encoder exists too, so a failure before any audio is
// still a JSON error.
func (s *Server) encoded(w http.ResponseWriter, r *http.Request, speech *pipeline.Speech, job *speechJob) {
	w.Header().Set("Content-Type", job.output.contentType)
	encoder, err := newEncoder(job.output.codec, &flushWriter{w: w, rc: http.NewResponseController(w)})
	if err != nil {
		s.encoderFailure(w, r, err)
		return
	}

	if err := streamEncoded(encoder, speech.PCM); err != nil {
		s.streamFailure(r, err)
	}
}

// encoderFailure is a JSON error unless writing the Opus headers already
// committed the 200.
func (s *Server) encoderFailure(w http.ResponseWriter, r *http.Request, err error) {
	if committed(w) {
		s.streamFailure(r, err)
	}

	s.writeError(w, unlessRequestEnded(r, http.StatusInternalServerError), err)
}

// The encoder is closed on every path, a failed write included, because an
// Opus encoder holds memory outside the Go heap until then. The first write
// commits the 200.
func streamEncoded(encoder io.WriteCloser, pcm io.Reader) (err error) {
	defer func() { err = errors.Join(err, encoder.Close()) }()

	_, err = io.CopyBuffer(encoder, pcm, make([]byte, copyBufferBytes))
	return err
}

// The response is already decided by the time the upstream body is closed,
// so a close error only matters for diagnosis.
func (s *Server) closeSpeech(speech *pipeline.Speech) {
	if err := speech.PCM.Close(); err != nil {
		s.log.Debug("closing upstream audio", slog.Any("error", err))
	}
}
