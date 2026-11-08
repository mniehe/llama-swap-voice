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

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	contentTypeWAV = "audio/wav"
	contentTypeMP3 = "audio/mpeg"
	contentTypeOgg = "audio/ogg"
	contentTypePCM = "audio/pcm"

	defaultSpeechFormat = "mp3"
	normalSpeed         = 1.0
	maxSpeechBodyBytes  = 1 << 20
	pcmSampleBytes      = 2
)

var errNoAudio = errors.New("upstream sent no audio")

// newEncoder is a variable so tests can substitute an encoder that fails.
var newEncoder = audio.NewEncoder

type outputFormat struct {
	contentType string
	upstream    upstream.Format
	codec       audio.Format
}

var outputFormats = map[string]outputFormat{
	"wav":  {contentType: contentTypeWAV, upstream: upstream.WAV, codec: audio.WAV},
	"pcm":  {contentType: contentTypePCM, upstream: upstream.WAV, codec: audio.PCM},
	"mp3":  {contentType: contentTypeMP3, upstream: upstream.WAV, codec: audio.MP3},
	"opus": {contentType: contentTypeOgg, upstream: upstream.WAV, codec: audio.Opus},
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

func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	job, err := s.parseSpeech(w, r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err)
		return
	}

	rec := record(r)
	rec.voice = job.voice
	rec.format = job.format

	reply, err := s.synthesize(r.Context(), upstream.Speech{
		Input:      job.input,
		Lang:       s.cfg.Lang,
		SpeakerRef: job.voicePath,
		Format:     job.output.upstream,
	})
	if err != nil {
		s.upstreamFailure(w, r, err)
		return
	}
	rec.upstreamStatus = reply.Status

	s.encoded(w, r, reply, &job)
}

func (s *Server) synthesize(ctx context.Context, speech upstream.Speech) (*upstream.Audio, error) {
	if err := s.ttsGate.acquire(ctx); err != nil {
		return nil, fmt.Errorf("waiting for a free TTS slot: %w", err)
	}

	reply, err := s.upstream.Synthesize(ctx, speech)
	if err != nil {
		s.ttsGate.release()
		return nil, err
	}

	reply.Body = &gatedBody{ReadCloser: reply.Body, release: s.ttsGate.release}
	return reply, nil
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
	if speed < audio.MinSpeed || speed > audio.MaxSpeed {
		return 0, fmt.Errorf("speed must be between %g and %g", audio.MinSpeed, audio.MaxSpeed)
	}

	return speed, nil
}

// encoded holds the 200 back until the first samples arrive and the encoder
// exists, so a failure before any audio is still a JSON error.
func (s *Server) encoded(w http.ResponseWriter, r *http.Request, reply *upstream.Audio, job *speechJob) {
	defer s.closeUpstream(reply)

	pcm, status, err := speechPCM(reply.Body, job.speed)
	if err != nil {
		s.writeError(w, unlessRequestEnded(r, status), err)
		return
	}

	first, err := readFirstChunk(pcm)
	if err != nil {
		s.writeError(w, unlessRequestEnded(r, http.StatusBadGateway), err)
		return
	}

	w.Header().Set("Content-Type", job.output.contentType)
	encoder, err := newEncoder(job.output.codec, &flushWriter{w: w, rc: http.NewResponseController(w)})
	if err != nil {
		s.encoderFailure(w, r, err)
		return
	}

	if err := streamEncoded(encoder, io.MultiReader(bytes.NewReader(first), pcm)); err != nil {
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

// speechPCM strips the upstream WAV header and, when the request changes the
// speed, time-stretches the samples as they arrive. On failure it also
// returns the status to answer with.
func speechPCM(body io.Reader, speed float64) (io.Reader, int, error) {
	pcm, err := audio.NewWAVReader(body)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("upstream audio: %w", err)
	}

	stretched, err := audio.NewStretcher(pcm, speed)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	return stretched, http.StatusOK, nil
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
func (s *Server) closeUpstream(reply *upstream.Audio) {
	if err := reply.Body.Close(); err != nil {
		s.log.Debug("closing upstream audio", slog.Any("error", err))
	}
}

// readFirstChunk waits for at least one whole sample, so the 200 is never
// committed for a reply that cannot hold any audio.
func readFirstChunk(r io.Reader) ([]byte, error) {
	chunk := make([]byte, copyBufferBytes)
	n, err := io.ReadAtLeast(r, chunk, pcmSampleBytes)
	if err == nil {
		return chunk[:n], nil
	}

	if errors.Is(err, io.EOF) {
		return nil, errNoAudio
	}

	return nil, err
}
