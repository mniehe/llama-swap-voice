package wyoming

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

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
// period starts once audio-stop is written. Inside a stream the event is
// ignored: Home Assistant sends it with the whole text for servers that do
// not stream.
func (c *connection) synthesize(event *Event) error {
	if c.state == stateStreaming {
		return nil
	}
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

	started := false
	if err := c.speakAdmitted(data.voiceName(), data.Text, &started); err != nil {
		return err
	}

	c.endRequest(outcomeOK)

	if err := c.reply(typeAudioStop, nil, nil); err != nil {
		return err
	}
	c.idleSince = time.Now()

	return nil
}

// speakAdmitted speaks text for the admitted request. The deadline hook is
// disarmed first, as for a transcription: no read waits during the reply, the
// request context still bounds the upstream call and every chunk, and a
// timeout can then be reported.
func (c *connection) speakAdmitted(voice, text string, started *bool) error {
	req := c.request
	if !req.disarm() {
		c.endRequest(c.outcomeOf(nil))
		return errRequestEnded
	}

	return speak(req.ctx, c, voice, text, started)
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

// stream is one streamed synthesis, from synthesize-start to its end. It
// holds no slot: each sentence is a request of its own. It counts once in the
// pipeline's admission, from the moment it is opened.
type stream struct {
	id        string
	requested string
	// voice is the resolved voice of the last sentence, for the log.
	voice    string
	started  time.Time
	deadline time.Time
	opened   bool
	// sentences splits the text in ModeSentences; in ModeWhole it is nil and
	// the text collects in whole.
	sentences    *splitter
	whole        string
	wholeRunes   int
	audioStarted bool
	spoken       int
}

// add takes a chunk of text and returns the segments ready to speak.
func (st *stream) add(text string) []string {
	if st.sentences == nil {
		st.whole += text
		st.wholeRunes += utf8.RuneCountInString(text)
		return nil
	}

	return st.sentences.Add(text)
}

// restRunes is the length in runes of the text not yet spoken.
func (st *stream) restRunes() int {
	if st.sentences == nil {
		return st.wholeRunes
	}

	return utf8.RuneCountInString(st.sentences.Rest())
}

func (st *stream) rest() string {
	if st.sentences == nil {
		return st.whole
	}

	return st.sentences.Rest()
}

// synthesizeStart opens a stream. No event is read and no sentence starts
// after the stream deadline; a sentence already running finishes within its
// own request deadline. A refused stream still gets its log line.
func (c *connection) synthesizeStart(event *Event) error {
	if err := c.requireState(stateIdle, event.Type); err != nil {
		return err
	}

	now := time.Now()
	st := &stream{id: rand.Text(), started: now, deadline: now.Add(c.server.maxStreamDuration)}
	c.stream = st
	c.state = stateStreaming

	var data synthesizeData
	if err := decode(event, &data); err != nil {
		return invalid("%s data: %v", event.Type, err)
	}
	st.requested = data.voiceName()

	if !c.enter() {
		return pipeline.ErrShuttingDown
	}
	st.opened = true

	if c.server.cfg.Streaming == ModeSentences {
		st.sentences = newSplitter(c.server.cfg.StreamingMaxChars)
	}

	return nil
}

// synthesizeChunk adds text to the stream and, in ModeSentences, speaks each
// segment it completes, in order, on the connection's goroutine. The text not
// yet spoken plus the chunk is bounded like the text of one synthesize. The
// chunk is counted whole before the splitter sees it: the splitter would
// release its sentences and drop its leading whitespace, and neither would
// be counted afterwards.
func (c *connection) synthesizeChunk(event *Event) error {
	if err := c.requireState(stateStreaming, event.Type); err != nil {
		return err
	}

	var data synthesizeChunkData
	if err := decode(event, &data); err != nil {
		return invalid("%s data: %v", event.Type, err)
	}

	unspoken := c.stream.restRunes() + utf8.RuneCountInString(data.Text)
	if limit := c.server.pipeline.Limits().MaxInputChars; unspoken > limit {
		return invalid("unspoken text exceeds %d characters", limit)
	}

	for _, text := range c.stream.add(data.Text) {
		if err := c.speakSentence(text); err != nil {
			return err
		}
	}

	return nil
}

// synthesizeStop speaks what the stream still holds, then ends it. The
// stream ends before audio-stop, as a request does, and the next idle period
// starts once synthesize-stopped is written.
func (c *connection) synthesizeStop() error {
	if err := c.requireState(stateStreaming, typeSynthesizeStop); err != nil {
		return err
	}

	st := c.stream
	if text := st.rest(); strings.TrimSpace(text) != "" {
		if err := c.speakSentence(text); err != nil {
			return err
		}
	}

	c.endStream(outcomeOK)

	if st.audioStarted {
		if err := c.reply(typeAudioStop, nil, nil); err != nil {
			return err
		}
	}
	if err := c.reply(typeSynthesizeStopped, nil, nil); err != nil {
		return err
	}
	c.idleSince = time.Now()

	return nil
}

// speakSentence speaks one segment of the open stream as a request of its
// own, with a TTS slot and a fresh request deadline that are both given back
// before it returns. The stream's admission covers it, so it neither enters
// nor leaves. Its requests carry the stream's id. No sentence starts once the
// stream deadline has passed, since no read between sentences would notice.
func (c *connection) speakSentence(text string) error {
	st := c.stream
	if !time.Now().Before(st.deadline) {
		return c.streamTimeout(os.ErrDeadlineExceeded)
	}
	c.request = &request{id: st.id, event: typeSynthesizeStart, started: time.Now()}

	if err := c.admitSlot(pipeline.WorkSpeak); err != nil {
		return err
	}

	err := c.speakAdmitted(st.requested, text, &st.audioStarted)
	if req := c.request; req != nil && req.voice != "" {
		st.voice = req.voice
	}
	if err != nil {
		return err
	}

	st.spoken++
	c.endRequest(outcomeOK)

	return nil
}

// endStream gives back the stream's admission and writes its one log line.
// Every exit path calls it after endRequest; with no stream open it does
// nothing.
func (c *connection) endStream(outcome string) {
	st := c.stream
	if st == nil {
		return
	}
	c.stream = nil
	c.state = stateIdle

	if st.opened {
		c.server.pipeline.Leave()
	}

	c.server.log.Info("request",
		slog.String("request_id", st.id),
		slog.String("protocol", protocolWyoming),
		slog.String("event", typeSynthesizeStart),
		slog.String("voice", st.voice),
		slog.Int64("duration_ms", time.Since(st.started).Milliseconds()),
		slog.String("outcome", outcome),
		slog.Int("sentences", st.spoken),
	)
}
