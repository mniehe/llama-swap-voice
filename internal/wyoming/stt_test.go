package wyoming

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const (
	riffChannelsOffset = 22
	riffRateOffset     = 24
	riffBitsOffset     = 34
	stallTimeout       = 2 * time.Second
	deadlineSlack      = 1500 * time.Millisecond
)

var (
	homeAssistantFormat = audioFormat{Rate: 16000, Width: 2, Channels: 1}
	studioFormat        = audioFormat{Rate: 48000, Width: 2, Channels: 2}
)

// silence returns d of zero samples in format.
func silence(format audioFormat, d time.Duration) []byte {
	frames := int(int64(format.Rate) * int64(d) / int64(time.Second))
	return make([]byte, frames*format.Width*format.Channels)
}

// tone returns d of a 440 Hz sine at a quarter of full scale in format, so
// the decoder sees real audio. Each sample is format.Width bytes, little-endian.
func tone(t *testing.T, format audioFormat, d time.Duration) []byte {
	t.Helper()

	frames := int(int64(format.Rate) * int64(d) / int64(time.Second))
	quarterScale := float64(int64(1)<<(8*format.Width-1)) / 4
	pcm := make([]byte, 0, frames*format.Width*format.Channels)
	for frame := range frames {
		sample := int32(math.Sin(2*math.Pi*440*float64(frame)/float64(format.Rate)) * quarterScale)
		encoded, err := binary.Append(nil, binary.LittleEndian, sample)
		require.NoError(t, err)
		for range format.Channels {
			pcm = append(pcm, encoded[:format.Width]...)
		}
	}

	return pcm
}

// record sends Home Assistant's sequence: transcribe, audio-start, the
// chunks and audio-stop.
func (c *client) record(format audioFormat, chunks ...[]byte) {
	c.t.Helper()

	c.send(typeTranscribe, map[string]string{"language": "en"}, nil)
	c.send(typeAudioStart, format, nil)
	for _, chunk := range chunks {
		c.send(typeAudioChunk, format, chunk)
	}
	c.send(typeAudioStop, nil, nil)
}

func (c *client) requireTranscript(want string) {
	c.t.Helper()

	assert.Equal(c.t, want, decodeData[transcript](c.t, c.readType(typeTranscript)).Text)
}

func (h *harness) uploadedWAV() []byte {
	h.t.Helper()

	calls := h.upstream.recorded()
	require.Len(h.t, calls, 1)
	assert.Equal(h.t, asrRoute, calls[0].path)

	return calls[0].files["file"]
}

func requireMono16k(t *testing.T, wav []byte) {
	t.Helper()

	require.Greater(t, len(wav), wavHeaderBytes)
	assert.Equal(t, "RIFF", string(wav[:4]))
	assert.Equal(t, "WAVE", string(wav[8:12]))
	assert.Equal(t, uint16(1), binary.LittleEndian.Uint16(wav[riffChannelsOffset:]))
	assert.Equal(t, uint32(16000), binary.LittleEndian.Uint32(wav[riffRateOffset:]))
	assert.Equal(t, uint16(16), binary.LittleEndian.Uint16(wav[riffBitsOffset:]))
}

func TestWAVHeaderBytes_MatchesAudioWAVHeader(t *testing.T) {
	header, err := audio.WAVHeader(homeAssistantFormat.Rate, homeAssistantFormat.Width, homeAssistantFormat.Channels, 0)

	require.NoError(t, err)
	assert.Len(t, header, wavHeaderBytes)
}

func TestTranscribe_HomeAssistantFormat_ReturnsTranscript(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	pcm := tone(t, homeAssistantFormat, time.Second)

	c.record(homeAssistantFormat, pcm[:len(pcm)/2], pcm[len(pcm)/2:])

	c.requireTranscript(transcriptText)
	wav := h.uploadedWAV()
	requireMono16k(t, wav)
	assert.Equal(t, pcm, wav[wavHeaderBytes:], "16 kHz mono 16-bit audio reaches the upstream unchanged")
}

func TestTranscribe_StereoAt48kHz_ReturnsTranscript(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.record(studioFormat, tone(t, studioFormat, time.Second))

	c.requireTranscript(transcriptText)
	wav := h.uploadedWAV()
	requireMono16k(t, wav)
	assert.InDelta(t, 2*16000, len(wav)-wavHeaderBytes, 2*16, "one second of 16 kHz mono")
}

func TestTranscribe_UnsupportedFormat_SendsInvalidError(t *testing.T) {
	const unsupported = "unsupported audio"
	tests := []struct {
		name   string
		format any
		text   string
	}{
		{name: "width 4", format: audioFormat{Rate: 16000, Width: 4, Channels: 1}, text: unsupported},
		{name: "width 1", format: audioFormat{Rate: 16000, Width: 1, Channels: 1}, text: unsupported},
		{name: "zero channels", format: audioFormat{Rate: 16000, Width: 2, Channels: 0}, text: unsupported},
		{name: "three channels", format: audioFormat{Rate: 16000, Width: 2, Channels: 3}, text: unsupported},
		{name: "rate below 8 kHz", format: audioFormat{Rate: 7999, Width: 2, Channels: 1}, text: unsupported},
		{name: "rate above 48 kHz", format: audioFormat{Rate: 48001, Width: 2, Channels: 1}, text: unsupported},
		{name: "no format", format: nil, text: unsupported},
		{name: "rate is text", format: map[string]any{"rate": "16000", "width": 2, "channels": 1}, text: "audio-start data"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withPipeline(oneASRSlot))
			c := h.dial()

			c.send(typeAudioStart, tc.format, nil)

			assert.Contains(t, c.requireError(codeInvalid).Text, tc.text)
			c.requireClosed()
			assert.Empty(t, h.upstream.recorded())
			assert.True(t, h.slotFree())
		})
	}
}

func TestTranscribe_FormatLimitsAreAccepted(t *testing.T) {
	for _, format := range []audioFormat{{Rate: 8000, Width: 3, Channels: 2}, {Rate: 48000, Width: 2, Channels: 1}} {
		t.Run(fmt.Sprintf("%d Hz %d bytes %d channels", format.Rate, format.Width, format.Channels), func(t *testing.T) {
			h := newHarness(t)
			c := h.dial()

			c.record(format, tone(t, format, 500*time.Millisecond))

			c.requireTranscript(transcriptText)
			wav := h.uploadedWAV()
			requireMono16k(t, wav)
			assert.InDelta(t, 16000, len(wav)-wavHeaderBytes, 2*16, "half a second of 16 kHz mono")
		})
	}
}

func TestTranscribe_Recording_AtTheLimitIsTranscribed(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	limit := h.pipeline.Limits().MaxUploadBytes

	c.send(typeAudioStart, homeAssistantFormat, nil)
	sendRecording(c, limit-wavHeaderBytes)
	c.send(typeAudioStop, nil, nil)

	c.requireTranscript(transcriptText)
	assert.Len(t, h.uploadedWAV(), int(limit), "the whole WAV upload is the limit")
}

func TestTranscribe_Recording_OneByteOverClosesTheConnection(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	c := h.dial()
	limit := h.pipeline.Limits().MaxUploadBytes

	c.send(typeAudioStart, homeAssistantFormat, nil)
	sendRecording(c, limit-wavHeaderBytes)
	c.send(typeAudioChunk, homeAssistantFormat, []byte{0})

	assert.Contains(t, c.requireError(codeInvalid).Text, fmt.Sprintf("longer than %d bytes", limit))
	c.requireClosed()
	assert.Empty(t, h.upstream.recorded())
	h.requireSlotFreed()
}

// sendRecording sends total bytes of silence in chunks of at most the
// payload limit.
func sendRecording(c *client, total int64) {
	c.t.Helper()

	chunk := make([]byte, maxPayloadBytes)
	for total > 0 {
		size := min(total, int64(len(chunk)))
		c.send(typeAudioChunk, homeAssistantFormat, chunk[:size])
		total -= size
	}
}

func TestTranscribe_SilentClient_LosesConnectionAtRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withPipeline(oneASRSlot))
		start := time.Now()
		c, done := h.servePipe()

		c.send(typeTranscribe, nil, nil)
		c.send(typeAudioStart, homeAssistantFormat, nil)
		c.send(typeAudioChunk, homeAssistantFormat, silence(homeAssistantFormat, 100*time.Millisecond))
		<-done

		assert.Equal(t, h.pipeline.Limits().RequestTimeout, time.Since(start))
		assert.True(t, h.slotFree())
		h.requireOutcome(codeTimeout)
	})
}

// requireOutcome waits for the one request log line and checks its outcome.
func (h *harness) requireOutcome(want string) {
	h.t.Helper()

	require.Eventually(h.t, h.logs.logged("request", 1), waitTimeout, 10*time.Millisecond)
	assert.Equal(h.t, want, h.logs.lines(h.t, "request")[0]["outcome"])
}

func TestTranscribe_HalfFrame_LosesConnectionAtRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newBubbleHarness(t, withPipeline(oneASRSlot))
		start := time.Now()
		c, done := h.servePipe()

		c.send(typeAudioStart, homeAssistantFormat, nil)
		c.sendRaw([]byte(`{"type":"audio-chunk","data":{"rate":16000,"width":2,"channels":1},"payload_length":3200}` + "\n"))
		c.sendRaw(make([]byte, 1600))
		<-done

		assert.Equal(t, h.pipeline.Limits().RequestTimeout, time.Since(start))
		assert.True(t, h.slotFree())
		h.requireOutcome(codeTimeout)
	})
}

func TestTranscribe_DuplicateAudioStart_SendsInvalidErrorAndFreesSlot(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	c := h.dial()

	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.send(typeAudioStart, homeAssistantFormat, nil)

	assert.Contains(t, c.requireError(codeInvalid).Text, "audio-start is not allowed while audio is being recorded")
	assert.True(t, h.slotFree(), "the slot is free by the time the error arrives")
	c.requireClosed()
	assert.Empty(t, h.upstream.recorded())
}

func TestTranscribe_EventOutOfState_SendsInvalidError(t *testing.T) {
	tests := []struct {
		name   string
		before []string
		event  string
		text   string
	}{
		{name: "audio-chunk before audio-start", event: typeAudioChunk, text: "audio-chunk is only allowed after audio-start"},
		{name: "audio-stop before audio-start", event: typeAudioStop, text: "audio-stop is only allowed after audio-start"},
		{name: "transcribe during a recording", before: []string{typeAudioStart}, event: typeTranscribe, text: "transcribe is not allowed while audio is being recorded"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, withPipeline(oneASRSlot))
			c := h.dial()

			for _, eventType := range tc.before {
				c.send(eventType, homeAssistantFormat, nil)
			}
			c.send(tc.event, homeAssistantFormat, silence(homeAssistantFormat, 10*time.Millisecond))

			assert.Contains(t, c.requireError(codeInvalid).Text, tc.text)
			assert.True(t, h.slotFree())
			c.requireClosed()
		})
	}
}

func TestTranscribe_ChunkInAnotherFormat_SendsInvalidError(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	c := h.dial()

	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.send(typeAudioChunk, studioFormat, silence(studioFormat, 10*time.Millisecond))

	c.requireError(codeInvalid)
	assert.True(t, h.slotFree())
	c.requireClosed()
}

func TestTranscribe_DescribePingAndUnknownDuringRecording_KeepTheRecording(t *testing.T) {
	h := newHarness(t)
	c := h.dial()
	pcm := tone(t, homeAssistantFormat, 500*time.Millisecond)

	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.send(typeAudioChunk, homeAssistantFormat, pcm[:len(pcm)/2])
	c.send(typeDescribe, nil, nil)
	c.readType(typeInfo)
	c.send(typePing, nil, nil)
	c.readType(typePong)
	c.send("wake-word", nil, []byte("ignored"))
	c.send(typeAudioChunk, homeAssistantFormat, pcm[len(pcm)/2:])
	c.send(typeAudioStop, nil, nil)

	c.requireTranscript(transcriptText)
	assert.Equal(t, pcm, h.uploadedWAV()[wavHeaderBytes:])
}

func TestTranscribe_TwoRequestsOnOneConnection(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	c := h.dial()

	c.record(homeAssistantFormat, tone(t, homeAssistantFormat, 200*time.Millisecond))
	c.requireTranscript(transcriptText)
	c.record(homeAssistantFormat, tone(t, homeAssistantFormat, 200*time.Millisecond))
	c.requireTranscript(transcriptText)

	c.send(typePing, nil, nil)
	c.readType(typePong)
	assert.Len(t, h.upstream.recorded(), 2)
}

func TestTranscribe_NoSlotFree_SendsBusyError(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	_, release, err := h.pipeline.Admit(t.Context(), pipeline.WorkTranscribe, time.Now().Add(time.Minute))
	require.NoError(t, err)
	t.Cleanup(release)
	c := h.dial()

	c.send(typeAudioStart, homeAssistantFormat, nil)

	assert.Contains(t, c.requireError(codeBusy).Text, "too many concurrent requests")
	c.requireClosed()
}

func TestTranscribe_AfterDrain_SendsShuttingDownError(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot))
	require.NoError(t, h.pipeline.Drain(t.Context()))
	c := h.dial()

	c.send(typeAudioStart, homeAssistantFormat, nil)

	c.requireError(codeShuttingDown)
	c.requireClosed()
	assert.True(t, h.slotFree())
}

func TestTranscribe_UpstreamFailure_SendsUpstreamError(t *testing.T) {
	h := newHarness(t, withPipeline(oneASRSlot), withUpstream(serveStatus(http.StatusInternalServerError)))
	c := h.dial()

	c.record(homeAssistantFormat, tone(t, homeAssistantFormat, 200*time.Millisecond))

	c.requireError(codeUpstream)
	assert.True(t, h.slotFree())
	c.requireClosed()
}

func TestTranscribe_TranscriberError_SendsInternalError(t *testing.T) {
	h := newHarness(t,
		withPipeline(oneASRSlot),
		withServer(func(s *Server) {
			s.transcribe = func(context.Context, io.Reader) (string, error) { return "", errors.New("boom") }
		}),
	)
	c := h.dial()

	c.record(homeAssistantFormat, tone(t, homeAssistantFormat, 100*time.Millisecond))

	failed := c.requireError(codeInternal)
	assert.Equal(t, internalErrorText, failed.Text)
	c.requireClosed()
	h.requireSlotFreed()
	h.requireOutcome(codeInternal)
}

func TestTranscribe_StalledUpstream_SendsTimeoutErrorAndFreesSlot(t *testing.T) {
	h := newHarness(t,
		requestTimeout(stallTimeout),
		withPipeline(oneASRSlot),
		withUpstream(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }),
	)
	c := h.dial()

	c.record(homeAssistantFormat, tone(t, homeAssistantFormat, 100*time.Millisecond))

	events := c.closedWithin(stallTimeout + deadlineSlack)
	require.Len(t, events, 1, "one error event, then the close")
	require.Equal(t, typeError, events[0].Type)
	assert.Equal(t, codeTimeout, decodeData[errorData](t, events[0]).Code)
	h.requireSlotFreed()
	h.requireOutcome(codeTimeout)
}

// The peer holds the transcript until the request has ended, so the reply
// write spans that hold; the next idle period must start after the write,
// not when the request ended.
func TestAudioStop_HeldReply_IdleClockStartsAfterTheWrite(t *testing.T) {
	h := newHarness(t, withServer(func(s *Server) {
		s.transcribe = func(context.Context, io.Reader) (string, error) { return transcriptText, nil }
	}))
	c, peer := h.pipeConnection()
	c.idleSince = time.Now().Add(-time.Hour)
	require.NoError(t, c.handle(audioStartEvent(t)))
	require.NoError(t, c.handle(&Event{Type: typeAudioChunk, Data: audioStartEvent(t).Data, Payload: []byte{0, 0}}))

	stopped := make(chan error, 1)
	go func() { stopped <- c.handle(&Event{Type: typeAudioStop}) }()
	require.Eventually(t, h.logs.logged("request", 1), waitTimeout, time.Millisecond)
	heldUntil := time.Now()
	(&client{t: t, conn: peer, in: newReader(peer)}).requireTranscript(transcriptText)

	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(waitTimeout):
		require.FailNow(t, "audio-stop did not return")
	}
	after := time.Now()

	deadline := c.readDeadline(after)
	assert.False(t, deadline.Before(heldUntil.Add(h.server.idleTimeout)), "the idle period started before the reply was written")
	assert.False(t, deadline.After(after.Add(h.server.idleTimeout)), "the idle period starts in the future")
}

func TestTranscribe_BaseCancelledDuringTranscription_SendsShuttingDownError(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	var once sync.Once
	h := newHarness(t,
		withBase(base),
		requestTimeout(time.Hour),
		withPipeline(oneASRSlot),
		withUpstream(func(_ http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(entered) })
			<-r.Context().Done()
		}),
	)
	c := h.dial()

	c.record(homeAssistantFormat, tone(t, homeAssistantFormat, 100*time.Millisecond))
	select {
	case <-entered:
	case <-time.After(waitTimeout):
		require.FailNow(t, "the transcription never reached the upstream")
	}
	cancel()

	events := c.closedWithin(waitTimeout)
	require.Len(t, events, 1, "one error event, then the close")
	require.Equal(t, typeError, events[0].Type)
	assert.Equal(t, codeShuttingDown, decodeData[errorData](t, events[0]).Code)
	h.requireSlotFreed()
	h.requireOutcome(codeShuttingDown)
}

func TestTranscribe_EmptyRecording_SendsInvalidError(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.record(homeAssistantFormat)

	c.requireError(codeInvalid)
	c.requireClosed()
}

func TestTranscribe_BaseCancelled_ClosesConnectionAndFreesSlot(t *testing.T) {
	base, cancel := context.WithCancel(t.Context())
	h := newHarness(t, withPipeline(oneASRSlot), withBase(base))
	c := h.dial()

	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.send(typePing, nil, nil)
	c.readType(typePong)
	cancel()

	c.requireClosed()
	h.requireSlotFreed()
	h.requireOutcome(codeShuttingDown)
}

func TestTranscribe_LogsOneRequestLine(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.record(homeAssistantFormat, tone(t, homeAssistantFormat, 200*time.Millisecond))
	c.requireTranscript(transcriptText)

	lines := h.logs.lines(t, "request")
	require.Len(t, lines, 1)
	assert.Equal(t, "wyoming", lines[0]["protocol"])
	assert.Equal(t, typeTranscribe, lines[0]["event"])
	assert.Empty(t, lines[0]["voice"])
	assert.Equal(t, outcomeOK, lines[0]["outcome"])
	assert.NotEmpty(t, lines[0]["request_id"])
	assert.Contains(t, lines[0], "duration_ms")
}

func TestTranscribe_FailedRequestLogsItsCode(t *testing.T) {
	h := newHarness(t)
	c := h.dial()

	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.send(typeAudioStart, homeAssistantFormat, nil)
	c.requireError(codeInvalid)
	c.requireClosed()

	h.requireOutcome(codeInvalid)
}

func TestErrorCode_EveryKind(t *testing.T) {
	tests := map[pipeline.Kind]string{
		pipeline.KindInvalid:      "invalid",
		pipeline.KindBusy:         "busy",
		pipeline.KindTimeout:      "timeout",
		pipeline.KindUpstream:     "upstream",
		pipeline.KindShuttingDown: "shutting-down",
		pipeline.KindInternal:     "internal",
		pipeline.Kind(0):          "internal",
	}

	for kind, want := range tests {
		assert.Equal(t, want, errorCode(kind), "kind %d", kind)
	}
}
