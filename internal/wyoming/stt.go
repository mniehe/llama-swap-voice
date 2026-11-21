package wyoming

import (
	"bytes"
	"errors"
	"io"
	"time"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const internalErrorText = "internal server error"

// wavHeaderBytes is the length of audio.WAVHeader's result; a test checks it.
// The recording bound leaves room for it, so the whole upload fits
// MaxUploadBytes.
const wavHeaderBytes = 44

var errRequestEnded = errors.New("the request ended before its upstream call began")

// The audio-start formats a recording may use. The decoder reads 16- and
// 24-bit PCM, mono or stereo, from 8 to 48 kHz.
const (
	minRate     = 8000
	maxRate     = 48000
	minWidth    = 2
	maxWidth    = 3
	minChannels = 1
	maxChannels = 2
)

// audioStart opens a recording. The request deadline starts here, so it
// bounds the whole recording as well as the transcription.
func (c *connection) audioStart(event *Event) error {
	if err := c.requireState(stateIdle, event.Type); err != nil {
		return err
	}

	c.beginRequest(typeTranscribe)

	format, err := readFormat(event)
	if err != nil {
		return err
	}

	if err := c.admit(pipeline.WorkTranscribe); err != nil {
		return err
	}

	c.request.format = format
	c.state = stateRecording

	return nil
}

func readFormat(event *Event) (audioFormat, error) {
	var format audioFormat
	if err := decode(event, &format); err != nil {
		return format, invalid("%s data: %v", event.Type, err)
	}

	if format.Rate < minRate || format.Rate > maxRate ||
		format.Width < minWidth || format.Width > maxWidth ||
		format.Channels < minChannels || format.Channels > maxChannels {
		return format, invalid("unsupported audio: %d Hz, %d bytes per sample, %d channels; want %d to %d Hz, %d or %d bytes, %d or %d channels",
			format.Rate, format.Width, format.Channels, minRate, maxRate, minWidth, maxWidth, minChannels, maxChannels)
	}

	return format, nil
}

// audioChunk appends to the recording. The recording and its WAV header share
// the HTTP upload bound, because Transcribe reads the upload whole.
func (c *connection) audioChunk(event *Event) error {
	if err := c.requireState(stateRecording, event.Type); err != nil {
		return err
	}

	format, err := readFormat(event)
	if err != nil {
		return err
	}
	if format != c.request.format {
		return invalid("audio-chunk format %+v differs from audio-start %+v", format, c.request.format)
	}

	limit := c.server.pipeline.Limits().MaxUploadBytes
	if int64(len(c.request.recording))+int64(len(event.Payload)) > limit-wavHeaderBytes {
		return invalid("recording with its WAV header is longer than %d bytes", limit)
	}
	c.request.recording = append(c.request.recording, event.Payload...)

	return nil
}

// audioStop wraps the recording in a WAV header and transcribes it. The
// request ends before the reply, so its slot is free when the client reads it;
// the next idle period starts once the reply is written, so a slow write does
// not use it up. The deadline hook is disarmed first: no read is waiting
// during the call, the request context still bounds it, and a timeout can then
// be reported.
func (c *connection) audioStop() error {
	if err := c.requireState(stateRecording, typeAudioStop); err != nil {
		return err
	}

	req := c.request
	header, err := audio.WAVHeader(req.format.Rate, req.format.Width, req.format.Channels, len(req.recording))
	if err != nil {
		return &pipeline.Error{Kind: pipeline.KindInternal, Message: internalErrorText, Err: err}
	}

	if !req.disarm() {
		c.endRequest(c.outcomeOf(nil))
		return errRequestEnded
	}

	upload := io.MultiReader(bytes.NewReader(header), bytes.NewReader(req.recording))
	text, err := c.server.transcribe(req.ctx, upload)
	if err != nil {
		var failed *pipeline.Error
		if !errors.As(err, &failed) {
			return &pipeline.Error{Kind: pipeline.KindInternal, Message: internalErrorText, Err: err}
		}
		return failed
	}

	c.endRequest(outcomeOK)

	if err := c.reply(typeTranscript, transcript{Text: text}, nil); err != nil {
		return err
	}
	c.idleSince = time.Now()

	return nil
}
