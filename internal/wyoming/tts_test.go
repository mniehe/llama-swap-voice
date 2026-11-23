package wyoming

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const (
	// wantChunkBytes is 100 ms of 24 kHz 16-bit mono.
	wantChunkBytes = 4800
	// neverReadPCMBytes is far more than the loopback socket buffers hold
	// for a peer that never reads, so the server's writes block.
	neverReadPCMBytes = 8 << 20
	speedTolerance    = 0.05
	doubleSpeed       = 2.0
)

var wantSpeechFormat = audioFormat{Rate: 24000, Width: 2, Channels: 1}

func oneTTSSlot(cfg *pipeline.Config) {
	cfg.MaxConcurrentTTS = 1
}

func (c *client) synthesize(text, voice string) {
	c.t.Helper()

	data := map[string]any{"text": text}
	if voice != "" {
		data["voice"] = map[string]string{"name": voice, "speaker": "ignored", "language": "xx"}
	}
	c.send(typeSynthesize, data, nil)
}

// readSpeech reads audio-start, the audio-chunk events and audio-stop,
// checking each format and chunk size, and returns the PCM.
func (c *client) readSpeech() []byte {
	c.t.Helper()

	assert.Equal(c.t, wantSpeechFormat, decodeData[audioFormat](c.t, c.readType(typeAudioStart)))

	var pcm []byte
	for {
		event := c.read()
		if event.Type == typeAudioStop {
			return pcm
		}

		require.Equal(c.t, typeAudioChunk, event.Type, "event data %s", event.Data)
		assert.Equal(c.t, wantSpeechFormat, decodeData[audioFormat](c.t, event))
		assert.LessOrEqual(c.t, len(event.Payload), wantChunkBytes)
		assert.NotEmpty(c.t, event.Payload)
		assert.Zero(c.t, len(event.Payload)%speechWidth, "a chunk ends inside a sample")
		pcm = append(pcm, event.Payload...)
	}
}

// speechTone is d of a tone in the TTS reply format.
func speechTone(t *testing.T, d time.Duration) []byte {
	t.Helper()

	return tone(t, wantSpeechFormat, d)
}

func TestSynthesize_NoVoice_StreamsTheDefaultVoiceInChunks(t *testing.T) {
	pcm := speechTone(t, 250*time.Millisecond)
	h := newHarness(t, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.synthesize("Hello there.", "")

	assert.Equal(t, pcm, c.readSpeech())
	calls := h.upstream.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, ttsRoute, calls[0].path)
	assert.Equal(t, "narrator.wav", string(calls[0].files["speaker_ref"]), "the default voice's clip")
}

func TestSynthesize_ChunksAreFullBeforeTheLast(t *testing.T) {
	pcm := speechTone(t, 250*time.Millisecond)
	h := newHarness(t, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.synthesize("Hello there.", "")
	c.readType(typeAudioStart)

	var sizes []int
	for event := c.read(); event.Type == typeAudioChunk; event = c.read() {
		sizes = append(sizes, len(event.Payload))
	}
	assert.Equal(t, []int{wantChunkBytes, wantChunkBytes, len(pcm) - 2*wantChunkBytes}, sizes)
}

func TestSynthesize_NamedVoice_UsesThatVoice(t *testing.T) {
	h := newHarness(t, withUpstream(serveSpeech(t, speechTone(t, 50*time.Millisecond))))
	c := h.dial()

	c.synthesize("Hello there.", "bob")
	c.readSpeech()

	calls := h.upstream.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, "bob.wav", string(calls[0].files["speaker_ref"]))
	h.requireRequestLine(typeSynthesize, "bob", outcomeOK)
}

// requireRequestLine waits for the one request log line and checks it.
func (h *harness) requireRequestLine(event, voice, outcome string) {
	h.t.Helper()

	require.Eventually(h.t, h.logs.logged("request", 1), waitTimeout, 10*time.Millisecond)
	line := h.logs.lines(h.t, "request")[0]
	assert.Equal(h.t, protocolWyoming, line["protocol"])
	assert.Equal(h.t, event, line["event"])
	assert.Equal(h.t, voice, line["voice"])
	assert.Equal(h.t, outcome, line["outcome"])
}

func TestSynthesize_ConfiguredSpeed_ReturnsAboutHalfTheSamples(t *testing.T) {
	pcm := speechTone(t, time.Second)
	h := newHarness(t,
		withUpstream(serveSpeech(t, pcm)),
		withServer(func(s *Server) { s.cfg.Speed = doubleSpeed }),
	)
	c := h.dial()

	c.synthesize("Hello there.", "")

	want := float64(len(pcm)/speechWidth) / doubleSpeed
	assert.InDelta(t, want, float64(len(c.readSpeech())/speechWidth), want*speedTolerance)
}

func TestSynthesize_UnknownVoice_SendsInvalidErrorListingTheVoices(t *testing.T) {
	h := newHarness(t, withPipeline(oneTTSSlot))
	c := h.dial()

	c.synthesize("Hello there.", "alloy")

	failed := c.requireError(codeInvalid)
	assert.Contains(t, failed.Text, `unknown voice "alloy"`)
	assert.Contains(t, failed.Text, "alice, bob, narrator")
	c.requireClosed()
	assert.Empty(t, h.upstream.recorded())
	h.requireTTSSlotFreed()
}

// ttsSlotFree reports whether a TTS slot can be taken now, giving it back.
func (h *harness) ttsSlotFree() bool {
	_, release, err := h.pipeline.Admit(context.Background(), pipeline.WorkSpeak, time.Now().Add(time.Minute))
	if err != nil {
		return false
	}
	release()

	return true
}

func (h *harness) requireTTSSlotFreed() {
	h.t.Helper()

	require.Eventually(h.t, h.ttsSlotFree, waitTimeout, 10*time.Millisecond, "the TTS slot was not released")
}

func TestSynthesize_EveryTTSSlotHeld_SendsBusyError(t *testing.T) {
	h := newHarness(t, withPipeline(oneTTSSlot))
	_, release, err := h.pipeline.Admit(t.Context(), pipeline.WorkSpeak, time.Now().Add(time.Minute))
	require.NoError(t, err)
	t.Cleanup(release)
	c := h.dial()

	c.synthesize("Hello there.", "")

	assert.Contains(t, c.requireError(codeBusy).Text, "too many concurrent requests")
	c.requireClosed()
	assert.Empty(t, h.upstream.recorded())
}

func TestSynthesize_DuringARecording_SendsInvalidError(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	c := h.dial()

	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.synthesize("Hello there.", "")

	assert.Contains(t, c.requireError(codeInvalid).Text, "synthesize is not allowed while audio is being recorded")
	c.requireClosed()
	h.requireSlotFreed()
	assert.Empty(t, h.upstream.recorded())
}

func TestSynthesize_MissingText_SendsInvalidError(t *testing.T) {
	h := newHarness(t, withPipeline(oneTTSSlot))
	c := h.dial()

	c.send(typeSynthesize, map[string]any{"text": "   "}, nil)

	assert.Contains(t, c.requireError(codeInvalid).Text, "input is required")
	c.requireClosed()
	h.requireTTSSlotFreed()
}

func TestSynthesize_TextOverTheLimit_SendsInvalidError(t *testing.T) {
	h := newHarness(t, withPipeline(oneTTSSlot))
	limit := h.pipeline.Limits().MaxInputChars
	c := h.dial()

	c.synthesize(strings.Repeat("a", limit+1), "")

	assert.Contains(t, c.requireError(codeInvalid).Text, fmt.Sprintf("input exceeds %d characters", limit))
	c.requireClosed()
	h.requireTTSSlotFreed()
}

func TestSynthesize_TextAtTheLimit_IsSpoken(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.synthesize(strings.Repeat("a", h.pipeline.Limits().MaxInputChars), "")

	assert.Equal(t, pcm, c.readSpeech())
}

func TestSynthesize_MalformedData_SendsInvalidError(t *testing.T) {
	h := newHarness(t, withPipeline(oneTTSSlot))
	c := h.dial()

	c.send(typeSynthesize, map[string]any{"text": 7}, nil)

	c.requireError(codeInvalid)
	c.requireClosed()
	h.requireTTSSlotFreed()
}

// The loopback socket buffers are far smaller than the reply, so the
// server's writes block on a peer that never reads.
func TestSynthesize_ClientNeverReads_ClosedAtWriteTimeoutAndQueueFreed(t *testing.T) {
	h := newHarness(t,
		withPipeline(oneTTSSlot),
		withUpstream(serveSpeech(t, make([]byte, neverReadPCMBytes))),
		withServer(func(s *Server) { s.writeTimeout = 200 * time.Millisecond }),
	)
	c := h.dial()

	c.synthesize("Hello there.", "")

	h.requireRequestLine(typeSynthesize, "narrator", outcomeClosed)
	c.requireClosed()

	ctx, release, err := h.pipeline.Admit(t.Context(), pipeline.WorkSpeak, time.Now().Add(waitTimeout))
	require.NoError(t, err, "the TTS slot is still held")
	defer release()
	job, err := h.pipeline.PrepareSpeech("", "again")
	require.NoError(t, err)
	speech, err := h.pipeline.Speak(ctx, job, 1)
	require.NoError(t, err, "the FIFO gate is still held")
	assert.NoError(t, speech.PCM.Close())
}

func TestSynthesize_UpstreamFailure_SendsUpstreamError(t *testing.T) {
	h := newHarness(t, withPipeline(oneTTSSlot), withUpstream(serveStatus(http.StatusInternalServerError)))
	c := h.dial()

	c.synthesize("Hello there.", "")

	c.requireError(codeUpstream)
	c.requireClosed()
	h.requireTTSSlotFreed()
	h.requireRequestLine(typeSynthesize, "narrator", codeUpstream)
}

// serveSpeechThenDrop declares a reply twice as long as pcm, sends pcm and
// drops the connection, so the reply fails after audio has started.
func serveSpeechThenDrop(t *testing.T, pcm []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		header, err := audio.WAVHeader(speechRate, speechWidth, speechChannels, 2*len(pcm))
		if !assert.NoError(t, err) {
			return
		}

		conn, buf, err := http.NewResponseController(w).Hijack()
		if !assert.NoError(t, err) {
			return
		}
		defer func() { assert.NoError(t, conn.Close()) }()

		_, err = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: audio/wav\r\nContent-Length: %d\r\n\r\n", len(header)+2*len(pcm))
		assert.NoError(t, err)
		_, err = buf.Write(append(header, pcm...))
		assert.NoError(t, err)
		assert.NoError(t, buf.Flush())
	}
}

// The 250 ms reply fills two whole chunks; the 50 ms left over is never
// sent, because the read that returns it also reports the dropped upstream.
func TestSynthesize_ReplyFailsAfterAudioStart_SendsErrorAndCloses(t *testing.T) {
	pcm := speechTone(t, 250*time.Millisecond)
	h := newHarness(t, withPipeline(oneTTSSlot), withUpstream(serveSpeechThenDrop(t, pcm)))
	c := h.dial()

	c.synthesize("Hello there.", "")

	events := c.closedWithin(waitTimeout)
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	require.Equal(t, []string{typeAudioStart, typeAudioChunk, typeAudioChunk, typeError}, types)
	assert.Equal(t, pcm[:2*wantChunkBytes], append(events[1].Payload, events[2].Payload...))
	failed := decodeData[errorData](t, events[3])
	assert.Equal(t, codeUpstream, failed.Code)
	assert.Contains(t, failed.Text, "upstream audio: ")
	h.requireTTSSlotFreed()
	h.requireRequestLine(typeSynthesize, "narrator", codeUpstream)
}

// serveSpeechInTwoParts sends the WAV header and first, flushes, and sends
// rest only once read closes.
func serveSpeechInTwoParts(t *testing.T, first, rest []byte, read <-chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header, err := audio.WAVHeader(speechRate, speechWidth, speechChannels, len(first)+len(rest))
		if !assert.NoError(t, err) {
			return
		}

		w.Header().Set("Content-Type", "audio/wav")
		_, err = w.Write(append(header, first...))
		assert.NoError(t, err)
		assert.NoError(t, http.NewResponseController(w).Flush())

		select {
		case <-read:
		case <-r.Context().Done():
			return
		}
		_, err = w.Write(rest)
		assert.NoError(t, err)
	}
}

// The upstream holds the rest of the reply until the client has read the
// first chunk, so a server that waited for the whole reply would never send
// it.
func TestSynthesize_FirstChunkIsSentBeforeTheUpstreamEnds(t *testing.T) {
	pcm := speechTone(t, 250*time.Millisecond)
	first, rest := pcm[:wantChunkBytes], pcm[wantChunkBytes:]
	read := make(chan struct{})
	h := newHarness(t, withUpstream(serveSpeechInTwoParts(t, first, rest, read)))
	c := h.dial()

	c.synthesize("Hello there.", "")
	c.readType(typeAudioStart)
	got := c.readType(typeAudioChunk).Payload
	assert.Equal(t, first, got)
	close(read)

	for event := c.read(); event.Type != typeAudioStop; event = c.read() {
		require.Equal(t, typeAudioChunk, event.Type, "event data %s", event.Data)
		got = append(got, event.Payload...)
	}
	assert.Equal(t, pcm, got)
}

func TestSynthesize_StalledUpstream_SendsTimeoutErrorAndFreesSlot(t *testing.T) {
	h := newHarness(t,
		requestTimeout(stallTimeout),
		withPipeline(oneTTSSlot),
		withUpstream(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }),
	)
	c := h.dial()

	c.synthesize("Hello there.", "")

	events := c.closedWithin(stallTimeout + deadlineSlack)
	require.Len(t, events, 1, "one error event, then the close")
	require.Equal(t, typeError, events[0].Type)
	assert.Equal(t, codeTimeout, decodeData[errorData](t, events[0]).Code)
	h.requireTTSSlotFreed()
	h.requireRequestLine(typeSynthesize, "narrator", codeTimeout)
}

func TestSynthesize_TwoRequestsOnOneConnection(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, withPipeline(oneTTSSlot), withUpstream(serveSpeech(t, pcm)))
	c := h.dial()

	c.synthesize("One.", "")
	assert.Equal(t, pcm, c.readSpeech())
	c.synthesize("Two.", "alice")
	assert.Equal(t, pcm, c.readSpeech())

	c.send(typePing, nil, nil)
	c.readType(typePong)
	assert.Len(t, h.upstream.recorded(), 2)
}

// The peer holds audio-stop until the request has ended, so that write spans
// the hold; the next idle period must start after it, not when the request
// ended.
func TestSynthesize_HeldAudioStop_IdleClockStartsAfterTheWrite(t *testing.T) {
	pcm := speechTone(t, 50*time.Millisecond)
	h := newHarness(t, withUpstream(serveSpeech(t, pcm)))
	c, peer := h.pipeConnection()
	c.idleSince = time.Now().Add(-time.Hour)
	reader := &client{t: t, conn: peer, in: newReader(peer)}

	synthesized := make(chan error, 1)
	go func() {
		synthesized <- c.handle(&Event{Type: typeSynthesize, Data: []byte(`{"text":"Hello there."}`)})
	}()
	reader.readType(typeAudioStart)
	reader.readType(typeAudioChunk)
	require.Eventually(t, h.logs.logged("request", 1), waitTimeout, time.Millisecond)
	heldUntil := time.Now()
	reader.readType(typeAudioStop)

	select {
	case err := <-synthesized:
		require.NoError(t, err)
	case <-time.After(waitTimeout):
		require.FailNow(t, "synthesize did not return")
	}
	after := time.Now()

	deadline := c.readDeadline(after)
	assert.False(t, deadline.Before(heldUntil.Add(h.server.idleTimeout)), "the idle period started before audio-stop was written")
	assert.False(t, deadline.After(after.Add(h.server.idleTimeout)), "the idle period starts in the future")
	assert.True(t, h.ttsSlotFree(), "the slot is free once audio-stop is written")
}

func TestSynthesize_BaseCancelledDuringSpeech_SendsShuttingDownError(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	h := newHarness(t,
		withBase(base),
		requestTimeout(time.Hour),
		withPipeline(oneTTSSlot),
		withUpstream(func(_ http.ResponseWriter, r *http.Request) {
			close(entered)
			<-r.Context().Done()
		}),
	)
	c := h.dial()

	c.synthesize("Hello there.", "")
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		require.FailNow(t, "the synthesis never reached the upstream")
	}
	cancel()

	events := c.closedWithin(waitTimeout)
	require.Len(t, events, 1, "one error event, then the close")
	assert.Equal(t, codeShuttingDown, decodeData[errorData](t, events[0]).Code)
	h.requireTTSSlotFreed()
	h.requireRequestLine(typeSynthesize, "narrator", codeShuttingDown)
}

// Once the request has ended, no chunk is written, even with PCM in hand.
// The peer never reads, so a chunk write would block until writeTimeout.
func TestSendPCM_RequestEnded_SendsNoChunk(t *testing.T) {
	h := newHarness(t, withServer(func(s *Server) { s.writeTimeout = 100 * time.Millisecond }))
	c, _ := h.pipeConnection()
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	cancel()

	err := c.sendPCM(ctx, bytes.NewReader(speechTone(t, 50*time.Millisecond)))

	var failed *pipeline.Error
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, pipeline.KindTimeout, failed.Kind)
	assert.Equal(t, "upstream audio: context canceled", failed.Message)
}
