package pipeline

import (
	"bytes"
	"context"
	"io"
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

// Transcribe decodes an untrusted upload to 16 kHz mono WAV, sends it
// upstream and strips the configured prefix from the text. It reads upload
// whole, so the caller must bound it by Limits().MaxUploadBytes.
func (p *Pipeline) Transcribe(ctx context.Context, upload io.Reader) (string, error) {
	wav, err := audio.NormaliseUpload(ctx, upload, asrLimits)
	if err != nil {
		return "", p.decodeFailure(ctx, err)
	}

	rec := upstream.Recording{Name: transcodedName, Data: bytes.NewReader(wav)}

	text, err := p.upstream.Transcribe(ctx, rec)
	if err != nil {
		return "", p.upstreamFailure(ctx, err)
	}

	return p.stripPrefix(text), nil
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
