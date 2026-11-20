package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	normalSpeed     = 1.0
	pcmSampleBytes  = 2
	firstChunkBytes = 32 * 1024
)

var errNoAudio = errors.New("upstream sent no audio")

// SpeechJob is a validated request: the resolved voice and the trimmed input.
type SpeechJob struct {
	Voice     string
	voicePath string
	input     string
}

// Speech is a reply whose first samples have already arrived. The caller
// must close PCM, which also frees the TTS queue slot.
type Speech struct {
	Voice          string
	UpstreamStatus int
	Retried        bool
	PCM            io.ReadCloser
}

// PrepareSpeech validates input and voice before any slot or upstream work,
// so an entrypoint can reject a bad request in its own validation order.
func (p *Pipeline) PrepareSpeech(voice, input string) (SpeechJob, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return SpeechJob{}, invalid("input is required")
	}

	if utf8.RuneCountInString(input) > p.cfg.MaxInputChars {
		return SpeechJob{}, invalid("input exceeds %d characters", p.cfg.MaxInputChars)
	}

	name, path, err := p.resolveVoice(voice)
	if err != nil {
		return SpeechJob{}, err
	}

	return SpeechJob{Voice: name, voicePath: path, input: input}, nil
}

func (p *Pipeline) resolveVoice(name string) (voice, path string, err error) {
	if name == "" {
		name = p.cfg.DefaultVoice
	}

	path, ok := p.cfg.Voices[name]
	if !ok {
		return "", "", invalid("unknown voice %q; available voices: %s", name, strings.Join(p.voices, ", "))
	}

	return name, path, nil
}

// ResolveSpeed returns normal speed when none was requested.
func ResolveSpeed(requested *float64) (float64, error) {
	if requested == nil {
		return normalSpeed, nil
	}

	speed := *requested
	if speed < audio.MinSpeed || speed > audio.MaxSpeed {
		return 0, invalid("speed must be between %g and %g", audio.MinSpeed, audio.MaxSpeed)
	}

	return speed, nil
}

// Speak returns once the first PCM samples are in hand, so an entrypoint can
// still answer with an error until then. On failure the upstream body is
// already closed and the queue slot is free.
func (p *Pipeline) Speak(ctx context.Context, job SpeechJob, speed float64) (*Speech, error) {
	reply, err := p.synthesize(ctx, &upstream.Speech{
		Voice:      job.Voice,
		Input:      job.input,
		Lang:       p.cfg.Lang,
		SpeakerRef: job.voicePath,
		Format:     upstream.WAV,
	})
	if err != nil {
		return nil, p.upstreamFailure(ctx, err)
	}

	// Deferred rather than closed on the error path, so a panic while reading
	// the reply still frees the queue slot.
	handedOver := false
	defer func() {
		if !handedOver {
			p.closeReply(ctx, reply.Body)
		}
	}()

	pcm, err := p.replyPCM(ctx, reply, speed)
	if err != nil {
		return nil, err
	}

	handedOver = true
	return &Speech{Voice: job.Voice, UpstreamStatus: reply.Status, Retried: reply.Retried, PCM: pcm}, nil
}

func (p *Pipeline) synthesize(ctx context.Context, speech *upstream.Speech) (*upstream.Audio, error) {
	if err := p.ttsGate.acquire(ctx); err != nil {
		return nil, fmt.Errorf("waiting for a free TTS slot: %w", err)
	}

	reply, err := p.upstream.Synthesize(ctx, speech)
	if err != nil {
		p.ttsGate.release()
		return nil, err
	}

	reply.Body = &gatedBody{ReadCloser: reply.Body, release: p.ttsGate.release}
	return reply, nil
}

// replyPCM reads through to the first whole sample.
func (p *Pipeline) replyPCM(ctx context.Context, reply *upstream.Audio, speed float64) (io.ReadCloser, error) {
	pcm, err := speechPCM(ctx, reply.Body, speed)
	if err != nil {
		return nil, answered(err, reply)
	}

	first, err := firstPCM(ctx, pcm)
	if err != nil {
		return nil, answered(err, reply)
	}

	return &speechStream{Reader: io.MultiReader(bytes.NewReader(first), pcm), body: reply.Body}, nil
}

// answered records the upstream status and retry on err, because the
// upstream did answer before the reply failed.
func answered(err error, reply *upstream.Audio) error {
	var failure *Error
	if errors.As(err, &failure) {
		failure.UpstreamStatus = reply.Status
		failure.Retried = reply.Retried
	}

	return err
}

// speechPCM strips the upstream WAV header and, when the request changes the
// speed, time-stretches the samples as they arrive.
func speechPCM(ctx context.Context, body io.Reader, speed float64) (io.Reader, error) {
	pcm, err := audio.NewWAVReader(body)
	if err != nil {
		wrappedErr := fmt.Errorf("upstream audio: %w", err)
		return nil, wrapped(endedOr(ctx, KindUpstream), wrappedErr)
	}

	stretched, err := audio.NewStretcher(pcm, speed)
	if err != nil {
		return nil, wrapped(endedOr(ctx, KindInternal), err)
	}

	return stretched, nil
}

// firstPCM waits for at least one whole sample, so a reply that cannot hold
// any audio fails before an entrypoint commits to it.
func firstPCM(ctx context.Context, pcm io.Reader) ([]byte, error) {
	chunk := make([]byte, firstChunkBytes)
	n, err := io.ReadAtLeast(pcm, chunk, pcmSampleBytes)
	if err == nil {
		return chunk[:n], nil
	}

	if errors.Is(err, io.EOF) {
		err = errNoAudio
	}

	return nil, wrapped(endedOr(ctx, KindUpstream), err)
}

// The reply is already decided by the time the upstream body is closed, so a
// close error only matters for diagnosis.
func (p *Pipeline) closeReply(ctx context.Context, body io.Closer) {
	if err := body.Close(); err != nil {
		p.log.Debug("closing upstream audio", requestID(ctx), slog.Any("error", err))
	}
}

// speechStream closes the gated upstream body, which frees the queue slot.
type speechStream struct {
	io.Reader
	body io.Closer
}

func (s *speechStream) Close() error {
	return s.body.Close()
}
