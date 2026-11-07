package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/mniehe/llama-swap-voice/internal/transcode"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	contentTypeWAV  = "audio/wav"
	contentTypeMP3  = "audio/mpeg"
	contentTypeOgg  = "audio/ogg"
	contentTypeFLAC = "audio/flac"
	contentTypeAAC  = "audio/aac"
	contentTypePCM  = "audio/pcm"

	defaultSpeechFormat = "mp3"
	normalSpeed         = 1.0
	minSpeed            = 0.25
	maxSpeed            = 4.0
	maxSpeechBodyBytes  = 1 << 20
)

var errNoAudio = errors.New("encoder produced no audio")

type outputFormat struct {
	contentType string
	upstream    upstream.Format
	codec       transcode.Format
}

var outputFormats = map[string]outputFormat{
	"wav":  {contentType: contentTypeWAV, upstream: upstream.WAV},
	"pcm":  {contentType: contentTypePCM, upstream: upstream.WAV, codec: transcode.PCM},
	"mp3":  {contentType: contentTypeMP3, upstream: upstream.WAV, codec: transcode.MP3},
	"opus": {contentType: contentTypeOgg, upstream: upstream.WAV, codec: transcode.Opus},
	"flac": {contentType: contentTypeFLAC, upstream: upstream.WAV, codec: transcode.FLAC},
	"aac":  {contentType: contentTypeAAC, upstream: upstream.WAV, codec: transcode.AAC},
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
	voice     string
	voicePath string
	input     string
	format    string
	output    outputFormat
	speed     float64
}

func (j *speechJob) codec() transcode.Format {
	if j.output.codec == "" && j.speed != normalSpeed {
		return transcode.WAV
	}

	return j.output.codec
}

func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	job, err := s.parseSpeech(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	rec := record(r)
	rec.voice = job.voice
	rec.format = job.format

	audio, err := s.synthesize(r.Context(), upstream.Speech{
		Input:      job.input,
		Lang:       s.cfg.Lang,
		SpeakerRef: job.voicePath,
		Format:     job.output.upstream,
	})
	if err != nil {
		s.upstreamFailure(w, r, err)
		return
	}
	rec.upstreamStatus = audio.Status

	if job.codec() == "" {
		s.passThrough(w, r, audio, &job)
		return
	}

	s.transcoded(w, r, audio, &job)
}

func (s *Server) synthesize(ctx context.Context, speech upstream.Speech) (*upstream.Audio, error) {
	if err := s.ttsGate.acquire(ctx); err != nil {
		return nil, fmt.Errorf("waiting for a free TTS slot: %w", err)
	}

	audio, err := s.upstream.Synthesize(ctx, speech)
	if err != nil {
		s.ttsGate.release()
		return nil, err
	}

	audio.Body = &gatedBody{ReadCloser: audio.Body, release: s.ttsGate.release}
	return audio, nil
}

func (s *Server) parseSpeech(w http.ResponseWriter, r *http.Request) (speechJob, error) {
	var req speechRequest
	body := http.MaxBytesReader(w, r.Body, maxSpeechBodyBytes)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return speechJob{}, fmt.Errorf("invalid JSON body: %w", err)
	}

	input := strings.TrimSpace(req.Input)
	if input == "" {
		return speechJob{}, errors.New("input is required")
	}

	if utf8.RuneCountInString(input) > s.cfg.MaxInputChars {
		return speechJob{}, fmt.Errorf("input exceeds %d characters", s.cfg.MaxInputChars)
	}

	voice, path, err := s.resolveVoice(req.Voice)
	if err != nil {
		return speechJob{}, err
	}

	format, output, err := resolveFormat(req.ResponseFormat)
	if err != nil {
		return speechJob{}, err
	}

	speed, err := resolveSpeed(req.Speed)
	if err != nil {
		return speechJob{}, err
	}

	return speechJob{voice: voice, voicePath: path, input: input, format: format, output: output, speed: speed}, nil
}

func (s *Server) resolveVoice(name string) (voice, path string, err error) {
	if name == "" {
		name = s.cfg.DefaultVoice
	}

	path, ok := s.cfg.Voices[name]
	if !ok {
		return "", "", fmt.Errorf("unknown voice %q; available voices: %s", name, strings.Join(s.voices, ", "))
	}

	return name, path, nil
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

func resolveSpeed(requested *float64) (float64, error) {
	if requested == nil {
		return normalSpeed, nil
	}

	speed := *requested
	if speed < minSpeed || speed > maxSpeed {
		return 0, fmt.Errorf("speed must be between %g and %g", minSpeed, maxSpeed)
	}

	return speed, nil
}

func (s *Server) passThrough(w http.ResponseWriter, r *http.Request, audio *upstream.Audio, job *speechJob) {
	defer s.closeUpstream(audio)

	if err := streamBody(w, job.output.contentType, audio.Body); err != nil {
		s.streamFailure(r, err)
	}
}

func (s *Server) transcoded(w http.ResponseWriter, r *http.Request, audio *upstream.Audio, job *speechJob) {
	encoded, err := s.encoder.Encode(r.Context(), audio.Body, transcode.Options{Format: job.codec(), Speed: job.speed})
	if err != nil {
		s.closeUpstream(audio)
		s.writeError(w, http.StatusInternalServerError, err)
		return
	}

	first, err := readFirstChunk(encoded)
	if err != nil {
		s.closeUpstream(audio)
		s.writeError(w, unlessRequestEnded(r, http.StatusBadGateway), errors.Join(err, encoded.Close()))
		return
	}

	streamErr := streamBody(w, job.output.contentType, io.MultiReader(bytes.NewReader(first), encoded))
	s.closeUpstream(audio)
	closeErr := encoded.Close()

	if err := errors.Join(streamErr, closeErr); err != nil {
		s.streamFailure(r, err)
	}
}

// The response is already decided by the time the upstream body is closed,
// so a close error only matters for diagnosis.
func (s *Server) closeUpstream(audio *upstream.Audio) {
	if err := audio.Body.Close(); err != nil {
		s.log.Debug("closing upstream audio", slog.Any("error", err))
	}
}

func readFirstChunk(r io.Reader) ([]byte, error) {
	chunk := make([]byte, copyBufferBytes)
	n, err := r.Read(chunk)
	if n > 0 {
		return chunk[:n], nil
	}

	if err == nil || err == io.EOF {
		return nil, errNoAudio
	}

	return nil, err
}
