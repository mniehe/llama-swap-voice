package wyoming

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

func sentencesMode(s *setup) {
	s.config.Streaming = ModeSentences
}

func streamingMaxChars(n int) option {
	return func(s *setup) { s.config.StreamingMaxChars = n }
}

func maxStream(d time.Duration) option {
	return withServer(func(s *Server) { s.maxStreamDuration = d })
}

func (c *client) startStream(voice string) {
	c.t.Helper()

	data := map[string]any{}
	if voice != "" {
		data["voice"] = map[string]string{"name": voice, "speaker": "ignored", "language": "xx"}
	}
	c.send(typeSynthesizeStart, data, nil)
}

func (c *client) streamChunk(text string) {
	c.t.Helper()

	c.send(typeSynthesizeChunk, map[string]string{"text": text}, nil)
}

func (c *client) stopStream() {
	c.t.Helper()

	c.send(typeSynthesizeStop, nil, nil)
}

// readPCM reads audio-chunk events until they hold n bytes.
func (c *client) readPCM(n int) []byte {
	c.t.Helper()

	var pcm []byte
	for len(pcm) < n {
		event := c.readType(typeAudioChunk)
		assert.Equal(c.t, wantSpeechFormat, decodeData[audioFormat](c.t, event))
		pcm = append(pcm, event.Payload...)
	}

	return pcm
}

// readStreamEnd reads audio-chunk events until audio-stop, then
// synthesize-stopped, and returns the PCM.
func (c *client) readStreamEnd() []byte {
	c.t.Helper()

	var pcm []byte
	for {
		event := c.read()
		if event.Type == typeAudioStop {
			break
		}
		require.Equal(c.t, typeAudioChunk, event.Type, "event data %s", event.Data)
		pcm = append(pcm, event.Payload...)
	}
	c.readType(typeSynthesizeStopped)

	return pcm
}

// requireStreamRefused expects an error with code, then synthesize-stopped,
// then the close.
func (c *client) requireStreamRefused(code string) errorData {
	c.t.Helper()

	failed := c.requireError(code)
	c.readType(typeSynthesizeStopped)
	c.requireClosed()

	return failed
}

// streamRecord returns the one request record, which must be the stream's.
func (h *harness) streamRecord() map[string]any {
	h.t.Helper()

	require.Eventually(h.t, h.logs.logged("request", 1), waitTimeout, 10*time.Millisecond)
	records := h.logs.lines(h.t, "request")
	require.Len(h.t, records, 1)
	assert.Equal(h.t, typeSynthesizeStart, records[0]["event"])
	assert.Equal(h.t, protocolWyoming, records[0]["protocol"])

	return records[0]
}

func TestStream_Sentences_SpeaksEachSentenceOnceAndIgnoresSynthesize(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.streamChunk("Hello the")
	c.streamChunk("re. How are")
	c.streamChunk(" you?")
	c.synthesize("Hello there. How are you?", "")
	c.stopStream()

	assert.Equal(t, wantSpeechFormat, decodeData[audioFormat](t, c.readType(typeAudioStart)))
	assert.Equal(t, append(append([]byte{}, pcm...), pcm...), c.readStreamEnd())
	assert.Equal(t, []string{"Hello there.", "How are you?"}, h.upstream.inputs())

	c.send(typePing, nil, nil)
	c.readType(typePong)
}

func TestStream_Sentences_AudioArrivesBeforeStop(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.streamChunk("Hello there. How")
	c.readType(typeAudioStart)
	assert.Equal(t, pcm, c.readPCM(len(pcm)))

	c.stopStream()
	assert.Equal(t, pcm, c.readStreamEnd())
	assert.Equal(t, []string{"Hello there.", "How"}, h.upstream.inputs())
}

func TestStream_MaxChars40_OneChunkMakesOneRequest(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, streamingMaxChars(40), withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.streamChunk("One. Two. Three. Fo")
	c.readType(typeAudioStart)
	c.readPCM(len(pcm))
	assert.Equal(t, []string{"One. Two. Three."}, h.upstream.inputs())

	c.stopStream()
	c.readStreamEnd()
	assert.Equal(t, []string{"One. Two. Three.", "Fo"}, h.upstream.inputs())
}

func TestStream_ChunksCarryingTheNextSentence_MakeThreeRequests(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, streamingMaxChars(40), withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.streamChunk("One. T")
	c.readType(typeAudioStart)
	c.readPCM(len(pcm))
	c.streamChunk("wo. T")
	c.readPCM(len(pcm))
	c.streamChunk("hree. F")
	c.readPCM(len(pcm))
	assert.Equal(t, []string{"One.", "Two.", "Three."}, h.upstream.inputs())

	c.stopStream()
	c.readStreamEnd()
	assert.Equal(t, []string{"One.", "Two.", "Three.", "F"}, h.upstream.inputs())
}

func TestStream_SentenceLongerThanMaxChars_IsSpokenWhole(t *testing.T) {
	const sentence = "This sentence is longer than ten characters."
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, streamingMaxChars(10), withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.streamChunk(sentence + " ")
	c.stopStream()

	c.readType(typeAudioStart)
	assert.Equal(t, pcm, c.readStreamEnd())
	assert.Equal(t, []string{sentence}, h.upstream.inputs())
}

func TestStream_Whole_OneRequestOnlyAfterStop(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.streamChunk("Hello the")
	c.streamChunk("re. How are")
	c.streamChunk(" you?")
	c.synthesize("Hello there. How are you?", "")
	c.send(typePing, nil, nil)
	c.readType(typePong)
	assert.Empty(t, h.upstream.recorded(), "whole mode spoke before synthesize-stop")

	c.stopStream()
	c.readType(typeAudioStart)
	assert.Equal(t, pcm, c.readStreamEnd())
	assert.Equal(t, []string{"Hello there. How are you?"}, h.upstream.inputs())
}

func streamingMode(mode Mode) option {
	return func(s *setup) { s.config.Streaming = mode }
}

var streamingModes = []Mode{ModeSentences, ModeWhole}

// The limit counts runes: "é" is two bytes, so a byte count would refuse the
// text at the limit.
func TestStream_UnspokenText_IsBoundedByMaxInputChars(t *testing.T) {
	for _, mode := range streamingModes {
		for _, letter := range []string{"a", "é"} {
			t.Run(fmt.Sprintf("%s %s one over", mode, letter), func(t *testing.T) {
				h := newHarness(t, withPipeline(oneTTSSlot), streamingMode(mode))
				limit := h.pipeline.Limits().MaxInputChars
				c := h.dial()

				c.startStream("")
				c.streamChunk(strings.Repeat(letter, limit))
				c.streamChunk(letter)

				failed := c.requireStreamRefused(codeInvalid)
				assert.Contains(t, failed.Text, fmt.Sprintf("exceeds %d characters", limit))
				assert.Empty(t, h.upstream.recorded())
				h.requireTTSSlotFreed()
				assert.Equal(t, codeInvalid, h.streamRecord()["outcome"])
			})

			t.Run(fmt.Sprintf("%s %s at the limit", mode, letter), func(t *testing.T) {
				pcm := speechTone(t, 50*time.Millisecond)
				h := newHarness(t, withUpstream(serveSpeech(t, pcm)), streamingMode(mode))
				limit := h.pipeline.Limits().MaxInputChars
				c := h.dial()

				c.startStream("")
				c.streamChunk(strings.Repeat(letter, limit-1))
				c.streamChunk(letter)
				c.stopStream()

				c.readType(typeAudioStart)
				assert.Equal(t, pcm, c.readStreamEnd())
				assert.Equal(t, []string{strings.Repeat(letter, limit)}, h.upstream.inputs())
			})
		}
	}

	// Spoken sentences leave the count, so a stream may carry more text in
	// total than the limit.
	t.Run("sentences spoken across chunks", func(t *testing.T) {
		const twoSentences = "This is one sentence. This is one sentence."
		pcm := speechTone(t, 50*time.Millisecond)
		h := newHarness(t, sentencesMode, withUpstream(serveSpeech(t, pcm)))
		limit := h.pipeline.Limits().MaxInputChars
		require.Greater(t, 3*utf8.RuneCountInString(twoSentences+" "), limit)
		c := h.dial()

		c.startStream("")
		for range 3 {
			c.streamChunk(twoSentences + " ")
		}
		c.stopStream()

		c.readType(typeAudioStart)
		assert.Equal(t, bytes.Repeat(pcm, 3), c.readStreamEnd())
		assert.Equal(t, []string{twoSentences, twoSentences, twoSentences}, h.upstream.inputs())
	})
}

// The splitter would release every sentence of the chunk and keep nothing,
// so the chunk is counted before it reaches the splitter.
func TestStream_ChunkOfSentencesOverTheLimit_IsRefusedUnspoken(t *testing.T) {
	h := newHarness(t, sentencesMode, withUpstream(serveSpeech(t, speechTone(t, 50*time.Millisecond))))
	limit := h.pipeline.Limits().MaxInputChars
	c := h.dial()

	c.startStream("")
	c.streamChunk(strings.Repeat("ab. ", limit))

	failed := c.requireStreamRefused(codeInvalid)
	assert.Contains(t, failed.Text, fmt.Sprintf("exceeds %d characters", limit))
	assert.Empty(t, h.upstream.recorded())
	assert.Equal(t, codeInvalid, h.streamRecord()["outcome"])
}

// The splitter drops leading whitespace, so it must be counted on arrival.
func TestStream_WhitespaceChunkOverTheLimit_IsRefused(t *testing.T) {
	for _, mode := range streamingModes {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t, streamingMode(mode))
			limit := h.pipeline.Limits().MaxInputChars
			c := h.dial()

			c.startStream("")
			c.streamChunk(strings.Repeat(" ", limit+1))

			assert.Contains(t, c.requireStreamRefused(codeInvalid).Text, fmt.Sprintf("exceeds %d characters", limit))
			assert.Equal(t, codeInvalid, h.streamRecord()["outcome"])
		})
	}
}

func TestStream_ChunkOfSentencesAtTheLimit_IsSpoken(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, streamingMaxChars(harnessStreamingMaxChars), withUpstream(serveSpeech(t, pcm)))
	limit := h.pipeline.Limits().MaxInputChars
	text := strings.Repeat("ab. ", limit/4)
	require.Equal(t, limit, utf8.RuneCountInString(text))
	c := h.dial()

	c.startStream("")
	c.streamChunk(text)
	c.stopStream()

	c.readType(typeAudioStart)
	assert.Equal(t, bytes.Repeat(pcm, 3), c.readStreamEnd())
	// Twelve sentences of "ab." joined make 47 runes; a thirteenth would
	// make 51.
	twelve := strings.TrimSpace(strings.Repeat("ab. ", 12))
	assert.Equal(t, []string{twelve, twelve, "ab."}, h.upstream.inputs())
}

// Each sentence takes 2 s, so the third ends at 6 s, after the stream
// deadline at 5 s, and the fourth must not start.
func TestStream_DeadlinePassesDuringASentence_StartsNoFurtherSentence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			streamFor = 5 * time.Second
			speakFor  = 2 * time.Second
		)
		pcm := speechTone(t, 50*time.Millisecond)
		speech := serveSpeech(t, pcm)
		h := newBubbleHarness(t, sentencesMode, streamingMaxChars(1), maxStream(streamFor),
			withUpstream(func(w http.ResponseWriter, r *http.Request) {
				<-time.After(speakFor)
				speech(w, r)
			}))
		start := time.Now()
		c, done := h.servePipe()

		c.startStream("")
		c.streamChunk("One. Two. Three. Four. ")
		c.readType(typeAudioStart)
		for range 3 {
			assert.Equal(t, pcm, c.readPCM(len(pcm)))
		}

		failed := c.requireError(codeTimeout)
		assert.Contains(t, failed.Text, "stream")
		assert.Equal(t, 3*speakFor, time.Since(start))
		c.readType(typeSynthesizeStopped)
		<-done
		assert.Equal(t, []string{"One.", "Two.", "Three."}, h.upstream.inputs())
		record := h.streamRecord()
		assert.Equal(t, codeTimeout, record["outcome"])
		assert.InDelta(t, 3, record["sentences"], 0, "sentences spoken")
	})
}

func TestStream_MalformedStartData_SendsOnlyTheError(t *testing.T) {
	h := newHarness(t, sentencesMode)
	c := h.dial()

	c.send(typeSynthesizeStart, map[string]any{"voice": 7}, nil)

	assert.Contains(t, c.requireError(codeInvalid).Text, "synthesize-start data")
	assert.Empty(t, c.closedWithin(waitTimeout), "no synthesize-stopped for a stream never opened")
	h.requireDrained()
	assert.Equal(t, codeInvalid, h.streamRecord()["outcome"])
}

func TestStream_MalformedChunkData_SendsErrorStoppedAndCloses(t *testing.T) {
	h := newHarness(t, sentencesMode)
	c := h.dial()

	c.startStream("")
	c.send(typeSynthesizeChunk, map[string]any{"text": 7}, nil)

	assert.Contains(t, c.requireStreamRefused(codeInvalid).Text, "synthesize-chunk data")
	assert.Empty(t, h.upstream.recorded())
	assert.Equal(t, codeInvalid, h.streamRecord()["outcome"])
}

// The peer reads audio-stop, then holds synthesize-stopped unread for a
// virtual second; the next idle period must start when that write completes,
// not before it.
func TestStream_HeldSynthesizeStopped_IdleClockStartsAfterTheWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const hold = time.Second
		pcm := speechTone(t, 50*time.Millisecond)
		h := newBubbleHarness(t, withUpstream(serveSpeech(t, pcm)))
		c, peer := h.pipeConnection()
		c.idleSince = time.Now().Add(-time.Hour)
		reader := &client{t: t, conn: peer, in: newReader(peer)}
		require.NoError(t, c.handle(&Event{Type: typeSynthesizeStart}))
		require.NoError(t, c.handle(&Event{Type: typeSynthesizeChunk, Data: []byte(`{"text":"Hello there."}`)}))

		stopped := make(chan error, 1)
		go func() { stopped <- c.handle(&Event{Type: typeSynthesizeStop}) }()
		reader.readType(typeAudioStart)
		reader.readType(typeAudioChunk)
		reader.readType(typeAudioStop)
		<-time.After(hold)
		reader.readType(typeSynthesizeStopped)
		written := time.Now()
		require.NoError(t, <-stopped)

		assert.Equal(t, written.Add(h.server.idleTimeout), c.readDeadline(time.Now()), "the idle period did not start when synthesize-stopped was written")
	})
}

// Each sentence takes 2 s and the stream deadline is at 4 s, so the third
// sentence would start at exactly the deadline; it must be refused.
func TestStream_SentenceAtTheStreamDeadlineInstant_IsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			streamFor = 4 * time.Second
			speakFor  = 2 * time.Second
		)
		pcm := speechTone(t, 50*time.Millisecond)
		speech := serveSpeech(t, pcm)
		h := newBubbleHarness(t, sentencesMode, streamingMaxChars(1), maxStream(streamFor),
			withUpstream(func(w http.ResponseWriter, r *http.Request) {
				<-time.After(speakFor)
				speech(w, r)
			}))
		start := time.Now()
		c, done := h.servePipe()

		c.startStream("")
		c.streamChunk("One. Two. Three. ")
		c.readType(typeAudioStart)
		for range 2 {
			assert.Equal(t, pcm, c.readPCM(len(pcm)))
		}

		failed := c.requireError(codeTimeout)
		assert.Contains(t, failed.Text, "stream")
		assert.Equal(t, streamFor, time.Since(start))
		c.readType(typeSynthesizeStopped)
		<-done
		assert.Equal(t, []string{"One.", "Two."}, h.upstream.inputs())
		record := h.streamRecord()
		assert.Equal(t, codeTimeout, record["outcome"])
		assert.InDelta(t, 2, record["sentences"], 0, "sentences spoken")
	})
}

// failSecondSpeech speaks pcm for the first request and fails every later one.
func failSecondSpeech(t *testing.T, pcm []byte) http.HandlerFunc {
	var calls atomic.Int32
	speech := serveSpeech(t, pcm)

	return func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			speech(w, r)
			return
		}
		serveStatus(http.StatusInternalServerError)(w, r)
	}
}

func TestStream_UpstreamFailsOnSecondSentence_SendsErrorStoppedAndCloses(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, withPipeline(oneTTSSlot), withUpstream(failSecondSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.streamChunk("One. T")
	c.readType(typeAudioStart)
	assert.Equal(t, pcm, c.readPCM(len(pcm)))
	c.streamChunk("wo. T")

	c.requireStreamRefused(codeUpstream)
	h.requireTTSSlotFreed()
	record := h.streamRecord()
	assert.Equal(t, codeUpstream, record["outcome"])
	assert.InDelta(t, 1, record["sentences"], 0, "sentences spoken")
}

func TestStream_EveryTTSSlotHeld_SentenceSendsBusyThenStopped(t *testing.T) {
	h := newHarness(t, sentencesMode, withPipeline(oneTTSSlot))
	_, release, err := h.pipeline.Admit(t.Context(), pipeline.WorkSpeak, time.Now().Add(time.Minute))
	require.NoError(t, err)
	t.Cleanup(release)
	c := h.dial()

	c.startStream("")
	c.streamChunk("Hello there. ")

	assert.Contains(t, c.requireStreamRefused(codeBusy).Text, "too many concurrent requests")
	assert.Empty(t, h.upstream.recorded())
	assert.Equal(t, codeBusy, h.streamRecord()["outcome"])
}

// The stream holds no TTS slot while it waits for text, so an HTTP request
// can take the only one; once that is given back, the stream speaks.
func TestStream_IdleStream_LeavesTheTTSSlotToOthers(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, withPipeline(oneTTSSlot), withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.send(typePing, nil, nil)
	c.readType(typePong)
	_, release, err := h.pipeline.Admit(t.Context(), pipeline.WorkSpeak, time.Now().Add(time.Minute))
	require.NoError(t, err, "an idle stream holds the TTS slot")
	release()

	c.streamChunk("Hello there. ")
	c.readType(typeAudioStart)
	assert.Equal(t, pcm, c.readPCM(len(pcm)))
}

func TestStream_ChunkOrStopWithoutStart_SendsInvalidErrorAndCloses(t *testing.T) {
	for _, eventType := range []string{typeSynthesizeChunk, typeSynthesizeStop} {
		t.Run(eventType, func(t *testing.T) {
			h := newHarness(t)
			c := h.dial()

			c.send(eventType, map[string]string{"text": "Hello there. "}, nil)

			assert.Contains(t, c.requireError(codeInvalid).Text, "only allowed after synthesize-start")
			assert.Empty(t, c.closedWithin(waitTimeout), "nothing follows the error")
			assert.Empty(t, h.upstream.recorded())
		})
	}
}

// The stream deadline comes before the idle deadline, so it ends the read.
func TestStream_OpenLongerThanMaxStreamDuration_SendsTimeoutAndCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const streamFor = 30 * time.Second
		h := newBubbleHarness(t, sentencesMode, maxStream(streamFor))
		start := time.Now()
		c, done := h.servePipe()

		c.startStream("")

		events := c.closedWithin(time.Hour)
		<-done
		assert.Equal(t, streamFor, time.Since(start))
		require.Len(t, events, 2, "an error and synthesize-stopped, then the close")
		assert.Equal(t, codeTimeout, decodeData[errorData](t, events[0]).Code)
		assert.Equal(t, typeSynthesizeStopped, events[1].Type)
		assert.Equal(t, codeTimeout, h.streamRecord()["outcome"])
	})
}

// The pong's write deadline is the stream deadline, which ends the
// connection while no sentence is open; that counts as the stream's timeout.
func TestStream_PongNeverReadNearTheStreamDeadline_EndsAsTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const streamFor = 3 * time.Minute
		h := newBubbleHarness(t, sentencesMode, maxStream(streamFor))
		start := time.Now()
		c, done := h.servePipe()

		c.startStream("")
		for range 3 {
			<-time.After(h.server.idleTimeout - 10*time.Second)
			c.streamChunk("still talking ")
		}
		<-time.After(time.Until(start.Add(streamFor - h.server.writeTimeout/2)))
		c.send(typePing, nil, nil)

		<-done
		assert.Equal(t, streamFor, time.Since(start))
		assert.Equal(t, codeTimeout, h.streamRecord()["outcome"])
	})
}

func TestStream_EventOutOfState_SendsInvalidError(t *testing.T) {
	tests := []struct {
		name    string
		opening func(c *client)
		event   string
		stopped bool
		want    string
	}{
		{name: "second synthesize-start", opening: func(c *client) { c.startStream("") }, event: typeSynthesizeStart, stopped: true, want: "while a synthesis stream is open"},
		{name: "transcribe in a stream", opening: func(c *client) { c.startStream("") }, event: typeTranscribe, stopped: true, want: "while a synthesis stream is open"},
		{name: "audio-start in a stream", opening: func(c *client) { c.startStream("") }, event: typeAudioStart, stopped: true, want: "while a synthesis stream is open"},
		{name: "audio-chunk in a stream", opening: func(c *client) { c.startStream("") }, event: typeAudioChunk, stopped: true, want: "only allowed after audio-start"},
		{name: "audio-stop in a stream", opening: func(c *client) { c.startStream("") }, event: typeAudioStop, stopped: true, want: "only allowed after audio-start"},
		{name: "synthesize-start in a recording", opening: recordingOpened, event: typeSynthesizeStart, want: "while audio is being recorded"},
		{name: "synthesize-chunk in a recording", opening: recordingOpened, event: typeSynthesizeChunk, want: "only allowed after synthesize-start"},
		{name: "synthesize-stop in a recording", opening: recordingOpened, event: typeSynthesizeStop, want: "only allowed after synthesize-start"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, sentencesMode)
			c := h.dial()
			tc.opening(c)

			c.send(tc.event, homeAssistantFormat, nil)

			assert.Contains(t, c.requireError(codeInvalid).Text, tc.want)
			if tc.stopped {
				c.readType(typeSynthesizeStopped)
			}
			assert.Empty(t, c.closedWithin(waitTimeout))
			h.requireSlotFreed()
			assert.Empty(t, h.upstream.recorded())
		})
	}
}

func recordingOpened(c *client) {
	c.send(typeAudioStart, homeAssistantFormat, nil)
}

func TestStream_UnknownVoice_FailsAtTheFirstSentence(t *testing.T) {
	h := newHarness(t, sentencesMode, withPipeline(oneTTSSlot))
	c := h.dial()

	c.startStream("alloy")
	c.send(typePing, nil, nil)
	c.readType(typePong)
	c.streamChunk("Hello there. ")

	assert.Contains(t, c.requireStreamRefused(codeInvalid).Text, `unknown voice "alloy"`)
	assert.Empty(t, h.upstream.recorded())
	h.requireTTSSlotFreed()
}

func TestStream_NamedVoice_SpeaksEverySentenceInIt(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("bob")
	c.streamChunk("One. T")
	c.readType(typeAudioStart)
	c.readPCM(len(pcm))
	c.streamChunk("wo.")
	c.stopStream()
	c.readStreamEnd()

	calls := h.upstream.recorded()
	require.Len(t, calls, 2)
	for _, call := range calls {
		assert.Equal(t, "bob.wav", string(call.files["speaker_ref"]))
	}
	assert.Equal(t, "bob", h.streamRecord()["voice"])
}

func TestStream_NoText_SendsOnlySynthesizeStopped(t *testing.T) {
	h := newHarness(t, sentencesMode)
	c := h.dial()

	c.startStream("")
	c.streamChunk("   ")
	c.stopStream()

	c.readType(typeSynthesizeStopped)
	c.send(typePing, nil, nil)
	c.readType(typePong)
	assert.Empty(t, h.upstream.recorded())
}

func TestStream_TwoStreamsOnOneConnection(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	for _, text := range []string{"One.", "Two."} {
		c.startStream("")
		c.streamChunk(text)
		c.stopStream()
		c.readType(typeAudioStart)
		assert.Equal(t, pcm, c.readStreamEnd())
	}
	assert.Equal(t, []string{"One.", "Two."}, h.upstream.inputs())
}

func TestStream_LogsOneRecordPerStream(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, sentencesMode, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.startStream("")
	c.streamChunk("One. Two. ")
	c.streamChunk("Three. Four")
	c.stopStream()
	c.readType(typeAudioStart)
	c.readStreamEnd()

	records := h.logs.lines(t, "request")
	require.Len(t, records, 1, "one record for the stream, none per sentence")
	assert.Equal(t, typeSynthesizeStart, records[0]["event"])
	assert.Equal(t, "narrator", records[0]["voice"])
	assert.Equal(t, outcomeOK, records[0]["outcome"])
	assert.InDelta(t, 3, records[0]["sentences"], 0, "requests made: One. Two., Three., Four")
	assert.NotEmpty(t, records[0]["request_id"])
}

// The client keeps sending text, so only the stream deadline can end the
// stream, and it ends exactly then.
func TestStream_DeadlinePassesDuringARead_SendsTimeoutStoppedAndCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const streamFor = 3 * time.Minute
		h := newBubbleHarness(t, sentencesMode, maxStream(streamFor))
		start := time.Now()
		c, done := h.servePipe()

		c.startStream("")
		for range 3 {
			<-time.After(h.server.idleTimeout - 10*time.Second)
			c.streamChunk("still talking ")
		}

		require.NoError(t, c.conn.SetReadDeadline(time.Now().Add(time.Hour)))
		event, err := ReadEvent(c.in)
		require.NoError(t, err)
		assert.Equal(t, streamFor, time.Since(start))
		require.Equal(t, typeError, event.Type)
		failed := decodeData[errorData](t, event)
		assert.Equal(t, codeTimeout, failed.Code)
		assert.Contains(t, failed.Text, "stream")
		c.readType(typeSynthesizeStopped)
		<-done
		assert.Equal(t, codeTimeout, h.streamRecord()["outcome"])
	})
}

// A stream that receives nothing for idleTimeout ends like an idle
// connection: no reply, and the stream's admission is given back.
func TestStream_IdleTimeoutInsideAStream_ClosesWithoutReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, sentencesMode)
		c, done := h.servePipe()

		c.startStream("")
		c.send(typePing, nil, nil)
		c.readType(typePong)
		start := time.Now()
		<-done

		assert.Equal(t, h.server.idleTimeout, time.Since(start))
		assert.Empty(t, c.closedWithin(time.Second))
		assert.Equal(t, outcomeClosed, h.streamRecord()["outcome"])
	})
}

// An event the reader had already buffered was not bound by the read
// deadline, so the stream deadline is checked again before it is handled.
func TestServe_BufferedEventPastTheStreamDeadline_SendsTimeout(t *testing.T) {
	h := newHarness(t, sentencesMode)
	c, peer := h.pipeConnection()
	require.NoError(t, c.handle(&Event{Type: typeSynthesizeStart}))
	c.stream.deadline = time.Now().Add(-time.Second)
	var buffered bytes.Buffer
	require.NoError(t, WriteEvent(&buffered, &Event{Type: typePing}))
	c.in = bufio.NewReader(&buffered)

	served := make(chan error, 1)
	go func() {
		err := c.serve()
		c.endStream(outcomeClosed)
		closeQuietly(t, c.conn)
		served <- err
	}()

	reader := &client{t: t, conn: peer, in: newReader(peer)}
	reader.requireError(codeTimeout)
	reader.readType(typeSynthesizeStopped)
	require.ErrorIs(t, <-served, os.ErrDeadlineExceeded)
}

// The client reads audio-start and then nothing, so the next audio-chunk
// write fails at writeTimeout with the sentence still open.
func TestStream_ClientStopsReadingMidSentence_ClosesFreesAndLogsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pcm := speechTone(t, 250*time.Millisecond)
		h := newBubbleHarness(t, sentencesMode, withPipeline(oneTTSSlot), requestTimeout(time.Hour), withUpstream(serveSpeech(t, pcm)))
		start := time.Now()
		c, done := h.servePipe()

		c.startStream("")
		c.streamChunk("Hello there. ")
		c.readType(typeAudioStart)

		<-done
		assert.Equal(t, h.server.writeTimeout, time.Since(start))
		synctest.Wait()
		assert.True(t, h.ttsSlotFree(), "the TTS slot is still held")
		require.NoError(t, h.pipeline.Drain(t.Context()))
		records := h.logs.lines(t, "request")
		require.Len(t, records, 1, "one record for the stream, none for its sentence")
		assert.Equal(t, typeSynthesizeStart, records[0]["event"])
		assert.Equal(t, outcomeClosed, records[0]["outcome"])
	})
}

func TestStream_StalledUpstream_SendsTimeoutStoppedAndFreesSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 30 * time.Second
		h := newBubbleHarness(t, sentencesMode, withPipeline(oneTTSSlot), requestTimeout(timeout),
			withUpstream(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		start := time.Now()
		c, done := h.servePipe()

		c.startStream("")
		c.streamChunk("Hello there. ")

		events := c.closedWithin(time.Hour)
		<-done
		assert.Equal(t, timeout, time.Since(start))
		require.Len(t, events, 2, "an error and synthesize-stopped, then the close")
		assert.Equal(t, codeTimeout, decodeData[errorData](t, events[0]).Code)
		assert.Equal(t, typeSynthesizeStopped, events[1].Type)
		synctest.Wait()
		assert.True(t, h.ttsSlotFree(), "the TTS slot is still held")
		assert.Equal(t, codeTimeout, h.streamRecord()["outcome"])
	})
}

func TestStream_BaseCancelledDuringASentence_SendsShuttingDownAndStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered := make(chan struct{})
		h := newBubbleHarness(t, sentencesMode, withBase(base), requestTimeout(time.Hour), withPipeline(oneTTSSlot),
			withUpstream(func(_ http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
			}))
		c, done := h.servePipe()

		c.startStream("")
		c.streamChunk("Hello there. ")
		<-entered
		cancel()

		events := c.closedWithin(time.Hour)
		<-done
		require.Len(t, events, 2, "an error and synthesize-stopped, then the close")
		assert.Equal(t, codeShuttingDown, decodeData[errorData](t, events[0]).Code)
		assert.Equal(t, typeSynthesizeStopped, events[1].Type)
		synctest.Wait()
		assert.True(t, h.ttsSlotFree(), "the TTS slot is still held")
		assert.Equal(t, codeShuttingDown, h.streamRecord()["outcome"])
	})
}

func TestStream_DrainWaitsAcrossSentences_AndTheSlotIsFreeBetween(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pcm := speechTone(t, 50*time.Millisecond)
		h := newBubbleHarness(t, sentencesMode, withPipeline(oneTTSSlot), withUpstream(serveSpeech(t, pcm)))
		c, done := h.servePipe()

		c.startStream("")
		c.streamChunk("One. T")
		c.readType(typeAudioStart)
		c.readPCM(len(pcm))
		synctest.Wait()
		assert.True(t, h.ttsSlotFree(), "the slot is held between sentences")

		drained := make(chan error, 1)
		go func() { drained <- h.pipeline.Drain(context.Background()) }()
		synctest.Wait()
		requireNotReturned(t, drained, "Drain with a stream open")

		c.streamChunk("wo. ")
		c.readPCM(len(pcm))
		synctest.Wait()
		requireNotReturned(t, drained, "Drain after the second sentence")

		c.stopStream()
		c.readStreamEnd()
		require.NoError(t, <-drained)
		assert.Equal(t, []string{"One.", "Two."}, h.upstream.inputs())

		closeQuietly(t, c.conn)
		<-done
	})
}

func TestShutdown_OpenStream_IsWaitedForUntilItEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pcm := speechTone(t, 50*time.Millisecond)
		h := newBubbleHarness(t, sentencesMode, withUpstream(serveSpeech(t, pcm)))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.startStream("")
		c.send(typePing, nil, nil)
		c.readType(typePong)

		shutdown := shutdownAsync(t.Context(), h.server)
		synctest.Wait()
		requireNotReturned(t, shutdown, "Shutdown with a stream open")

		c.streamChunk("Hello there. ")
		c.readType(typeAudioStart)
		c.readPCM(len(pcm))
		synctest.Wait()
		requireNotReturned(t, shutdown, "Shutdown between sentences")

		c.stopStream()
		c.readStreamEnd()
		c.requireClosed()
		require.NoError(t, <-shutdown)
		require.NoError(t, <-served)
	})
}

func TestShutdown_CloseEndsAnOpenStream_AndDrainCompletes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, sentencesMode)
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.startStream("")
		c.send(typePing, nil, nil)
		c.readType(typePong)

		require.NoError(t, h.server.Close())

		c.requireClosed()
		require.NoError(t, <-served)
		require.NoError(t, h.pipeline.Drain(t.Context()))
		assert.Equal(t, outcomeClosed, h.streamRecord()["outcome"])
	})
}

// Close ends the idle stream; with the base context already cancelled, as
// main does at shutdown, its record names the shutdown.
func TestShutdown_BaseCancelledThenClose_StreamRecordSaysShuttingDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base, cancel := context.WithCancel(t.Context())
		defer cancel()
		h := newBubbleHarness(t, sentencesMode, withBase(base))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.startStream("")
		c.send(typePing, nil, nil)
		c.readType(typePong)

		cancel()
		require.NoError(t, h.server.Close())

		c.requireClosed()
		require.NoError(t, <-served)
		require.NoError(t, h.pipeline.Drain(t.Context()))
		assert.Equal(t, codeShuttingDown, h.streamRecord()["outcome"])
	})
}

// A connection kept for its recording still refuses a stream the client had
// already sent behind it.
func TestShutdown_SynthesizeStartAfterShutdown_IsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withServer(func(s *Server) { s.transcribe = instantTranscriber }))
		listener, served := h.serveInBubble()
		c := listener.dialClient(t)
		c.send(typeAudioStart, homeAssistantFormat, nil)
		c.send(typeAudioChunk, homeAssistantFormat, silence(homeAssistantFormat, 10*time.Millisecond))
		synctest.Wait()
		shutdown := shutdownAsync(t.Context(), h.server)
		synctest.Wait()

		var frames bytes.Buffer
		require.NoError(t, WriteEvent(&frames, &Event{Type: typeAudioStop}))
		require.NoError(t, WriteEvent(&frames, &Event{Type: typeSynthesizeStart}))
		c.sendRaw(frames.Bytes())
		c.requireTranscript(transcriptText)

		assert.Contains(t, c.requireError(codeShuttingDown).Text, "shutting down")
		c.requireClosed()
		require.NoError(t, <-shutdown)
		require.NoError(t, <-served)
	})
}
