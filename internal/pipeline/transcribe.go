package pipeline

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	transcodedName  = "audio.wav"
	maxAudioMinutes = 20
)

var asrLimits = audio.Limits{MaxDuration: maxAudioMinutes * time.Minute}

// Source names the entrypoint a transcription arrived through.
type Source string

const (
	SourceOpenAI  Source = "openai"
	SourceWyoming Source = "wyoming"
)

// Transcribe decodes an untrusted upload to 16 kHz mono WAV, sends it
// upstream and strips the configured prefix from the text. It reads upload
// whole, so the caller must bound it by Limits().MaxUploadBytes. When
// recordings are on, a successful transcription is saved before Transcribe
// returns; a failed save is logged and does not change the result.
func (p *Pipeline) Transcribe(ctx context.Context, upload io.Reader, source Source) (string, error) {
	wav, err := audio.NormaliseUpload(ctx, upload, asrLimits)
	if err != nil {
		return "", p.decodeFailure(ctx, err)
	}

	rec := upstream.Recording{Name: transcodedName, Data: bytes.NewReader(wav)}

	text, err := p.upstream.Transcribe(ctx, rec)
	if err != nil {
		return "", p.upstreamFailure(ctx, err)
	}

	transcript := p.stripPrefix(text)
	p.record(ctx, wav, source, transcript)

	return transcript, nil
}

func (p *Pipeline) record(ctx context.Context, wav []byte, source Source, transcript string) {
	if p.cfg.Recorder == nil {
		return
	}

	if err := p.cfg.Recorder.Save(wav, string(source), transcript); err != nil {
		p.log.Warn("could not save recording", requestID(ctx), slog.String("source", string(source)), slog.Any("error", err))
	}
}

func (p *Pipeline) stripPrefix(text string) string {
	if p.prefix == nil {
		return strings.TrimSpace(text)
	}

	loc := p.prefix.FindStringIndex(text)
	if len(loc) == 0 || loc[0] != 0 {
		return strings.TrimSpace(text)
	}

	return strings.TrimSpace(text[loc[1]:])
}
