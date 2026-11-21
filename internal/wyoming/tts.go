package wyoming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

// The PCM that Speak returns, which every reply event describes.
const (
	replyRate     = 24000
	replyWidth    = 2
	replyChannels = 1
	// chunkBytes is 100 ms of reply audio, a whole number of samples.
	chunkBytes = 4800
)

var replyFormat = audioFormat{Rate: replyRate, Width: replyWidth, Channels: replyChannels}

// synthesize speaks the text of one synthesize event. The request ends before
// audio-stop, so its slot is free when the client reads it; the next idle
// period starts once audio-stop is written. The deadline hook is disarmed
// first, as for a transcription: no read waits during the reply, the request
// context still bounds the upstream call and every chunk, and a timeout can
// then be reported.
func (c *connection) synthesize(event *Event) error {
	if err := c.requireState(stateIdle, event.Type); err != nil {
		return err
	}

	c.beginRequest(typeSynthesize)

	var data synthesizeData
	if err := decode(event, &data); err != nil {
		return invalid("%s data: %v", event.Type, err)
	}

	if err := c.admit(pipeline.WorkSpeak); err != nil {
		return err
	}
	c.state = stateSynthesising

	req := c.request
	if !req.disarm() {
		c.endRequest(c.outcomeOf(nil))
		return errRequestEnded
	}

	started := false
	if err := speak(req.ctx, c, data.voiceName(), data.Text, &started); err != nil {
		return err
	}

	c.endRequest(outcomeOK)

	if err := c.reply(typeAudioStop, nil, nil); err != nil {
		return err
	}
	c.idleSince = time.Now()

	return nil
}

// speak sends text in voice as audio-start, unless *started says it was
// already sent, and then as audio-chunk events while the PCM arrives. The
// caller sends audio-stop. ctx is the open request's, and that request's
// cleanup closes the PCM on every path.
func speak(ctx context.Context, c *connection, voice, text string, started *bool) error {
	p := c.server.pipeline
	job, err := p.PrepareSpeech(voice, text)
	if err != nil {
		return err
	}
	c.request.voice = job.Voice

	speech, err := p.Speak(ctx, job, c.server.cfg.Speed)
	if err != nil {
		return err
	}
	c.request.pcm = speech.PCM

	if !*started {
		if startErr := c.reply(typeAudioStart, replyFormat, nil); startErr != nil {
			return startErr
		}
		*started = true
	}

	return c.sendPCM(ctx, speech.PCM)
}

func (c *connection) sendPCM(ctx context.Context, pcm io.Reader) error {
	chunk := make([]byte, chunkBytes)
	for {
		n, readErr := fill(pcm, chunk)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return speechFailure(ctx, readErr)
		}

		if n > 0 {
			if err := c.sendChunk(ctx, chunk[:n]); err != nil {
				return err
			}
		}

		if readErr != nil {
			return nil
		}
	}
}

// sendChunk sends nothing once the request has ended, so its deadline bounds
// the reply even while the PCM keeps arriving.
func (c *connection) sendChunk(ctx context.Context, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return speechFailure(ctx, err)
	}

	return c.reply(typeAudioChunk, replyFormat, payload)
}

// fill reads until buf is full or pcm ends, so every chunk but the last is
// full. It returns io.EOF only when pcm ended. Speak's PCM ends on a whole
// sample, so with chunkBytes even every chunk holds whole samples.
func fill(pcm io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		read, err := pcm.Read(buf[n:])
		n += read
		if err != nil {
			return n, err
		}
	}

	return n, nil
}

// speechFailure names a reply that failed after Speak returned. An ended
// request explains whatever the read then reported.
func speechFailure(ctx context.Context, err error) *pipeline.Error {
	kind := pipeline.KindUpstream
	if ctx.Err() != nil {
		kind = pipeline.KindTimeout
	}

	return &pipeline.Error{Kind: kind, Message: fmt.Sprintf("upstream audio: %v", err), Err: err}
}

// closeSpeech closes a request's PCM, which frees its place at the TTS gate.
// The reply is decided by then, so a close error only matters for diagnosis.
func (s *Server) closeSpeech(req *request) {
	if req.pcm == nil {
		return
	}

	if err := req.pcm.Close(); err != nil {
		s.log.Debug("closing upstream audio", slog.String("request_id", req.id), slog.Any("error", err))
	}
}
