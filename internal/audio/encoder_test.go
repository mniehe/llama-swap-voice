package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	halfSecondSamples = outputRate / 2
	opusHeadersBytes  = 200
	disconnectRounds  = 100
	maxPagePackets    = 10
	tagsVendorOffset  = len(opusTagsMagic) + 4
)

var errClientGone = errors.New("client went away")

// pageEdgeLengths are input lengths whose packet count, lookahead included,
// lands on or next to a whole number of pages.
var pageEdgeLengths = []int{1, 324, 480, 4164, 4165, 4200, 4320, 4644, 4645, 8964, 8965, 9000, 9444, 9445}

func s16le(samples []float32) []byte {
	return speechWAV(samples)[wavHeaderBytes:]
}

func encode(t *testing.T, format Format, pcm []byte) []byte {
	t.Helper()

	var out bytes.Buffer
	encoder, err := NewEncoder(format, &out)
	require.NoError(t, err)

	_, err = io.Copy(encoder, bytes.NewReader(pcm))
	require.NoError(t, err)
	require.NoError(t, encoder.Close())

	return out.Bytes()
}

func TestNewEncoder_PCM_WritesSamplesUnchanged(t *testing.T) {
	pcm := s16le(tone(outputRate, toneSeconds, toneAmplitude))

	assert.Equal(t, pcm, encode(t, PCM, pcm))
}

func TestNewEncoder_WAV_WritesStreamedHeaderThenSamples(t *testing.T) {
	pcm := s16le(tone(outputRate, toneSeconds, toneAmplitude))

	wav := encode(t, WAV, pcm)

	assert.Equal(t, streamedWAV(speechWAV(tone(outputRate, toneSeconds, toneAmplitude)))[:wavHeaderBytes], wav[:wavHeaderBytes])
	assert.Equal(t, pcm, readPCM(t, bytes.NewReader(wav)))
}

func TestNewEncoder_UnknownFormat_ReturnsError(t *testing.T) {
	for _, format := range []Format{"flac", "aac", ""} {
		_, err := NewEncoder(format, io.Discard)

		require.ErrorIs(t, err, ErrUnsupported, "format %q", format)
	}
}

func oggPages(t *testing.T, stream []byte) []oggPackets {
	t.Helper()

	reader := newOggReader(stream)
	var pages []oggPackets
	for {
		page, err := reader.nextPage()
		if errors.Is(err, io.EOF) {
			return pages
		}
		require.NoError(t, err)
		pages = append(pages, page)
	}
}

func encoderPreSkip(t *testing.T) uint16 {
	t.Helper()

	encoder, err := newOpusEncoder(io.Discard)
	require.NoError(t, err)
	defer func() { require.NoError(t, encoder.Close()) }()

	return encoder.preSkip
}

func TestNewEncoder_Opus_WritesOggOpusTimeline(t *testing.T) {
	samples := tone(outputRate, toneSeconds, toneAmplitude)
	preSkip := encoderPreSkip(t)

	pages := oggPages(t, encode(t, Opus, s16le(samples)))

	require.Greater(t, len(pages), 3)
	require.Len(t, pages[0].packets, 1, "OpusHead sits alone on the first page")
	head, err := parseOpusHead(pages[0].packets[0])
	require.NoError(t, err)
	assert.Equal(t, opusHead{channels: outputChannels, preSkip: int(preSkip), gain: 1}, head)
	assert.Equal(t, uint32(outputRate), binary.LittleEndian.Uint32(pages[0].packets[0][opusPreSkipOffset+2:]), "input sample rate")
	require.Len(t, pages[1].packets, 1, "OpusTags sits alone on the second page")
	assert.Contains(t, string(pages[1].packets[0][tagsVendorOffset:]), "llama-swap-voice")

	audioPages := pages[2:]
	packets := 0
	for i, page := range audioPages {
		packets += len(page.packets)
		assert.LessOrEqual(t, len(page.packets), maxPagePackets, "page %d holds more than 200 ms", i)
		assert.Equal(t, i == len(audioPages)-1, page.last, "only the final page ends the stream")
		if !page.last {
			assert.Equal(t, uint64(packets*opusPacketGranules), page.granule, "page %d granule", i)
		}
	}

	final := audioPages[len(audioPages)-1].granule
	assert.Equal(t, uint64(preSkip)+uint64(len(samples))*opusGranuleScale, final, "final granule is pre-skip plus the input length")
	assert.LessOrEqual(t, final, uint64(packets*opusPacketGranules), "final granule within the encoded packets")
}

func TestNewEncoder_Opus_FinalPageCarriesTheEnd(t *testing.T) {
	pcm := s16le(tone(outputRate, toneSeconds, toneAmplitude))

	for _, samples := range pageEdgeLengths {
		t.Run(strconv.Itoa(samples), func(t *testing.T) {
			pages := oggPages(t, encode(t, Opus, pcm[:samples*outputSampleBytes]))

			var granule uint64
			for i, page := range pages {
				assert.GreaterOrEqual(t, page.granule, granule, "page %d granule decreases", i)
				granule = page.granule
			}

			final := pages[len(pages)-1]
			assert.True(t, final.last, "the final page does not end the stream")
			assert.NotEmpty(t, final.packets, "the final page holds no packet")
		})
	}
}

func TestNewEncoder_Opus_DecodesToInputLength(t *testing.T) {
	samples := tone(outputRate, toneSeconds, toneAmplitude)

	out := normalise(t, encode(t, Opus, s16le(samples)))

	assert.Len(t, out.samples, len(samples)*targetRate/outputRate)
	assert.InDelta(t, toneAmplitude/math.Sqrt2, rms(out.samples), 2*rmsTolerance)
}

func TestNewEncoder_MP3_DecodesWithDelayAndKeepsTail(t *testing.T) {
	samples := tone(outputRate, toneSeconds, toneAmplitude)

	out := normalise(t, encode(t, MP3, s16le(samples)))

	delayed := (len(samples) + shineDelaySamples) * targetRate / outputRate
	assert.GreaterOrEqual(t, len(out.samples), delayed, "decoded MP3 lost the input's tail")
	assert.Less(t, len(out.samples), delayed+mp3FrameSamples*targetRate/outputRate+mp3PaddingLimit)
}

// recordingWriter counts the bytes that reached it, so a test can tell an
// encoder that streams from one that holds everything until Close.
type recordingWriter struct {
	mu      sync.Mutex
	written int
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.written += len(p)
	return len(p), nil
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.written
}

func TestNewEncoder_StreamsBeforeClose(t *testing.T) {
	pcm := s16le(tone(outputRate, toneSeconds, toneAmplitude))[:halfSecondSamples*outputSampleBytes]

	for _, format := range []Format{PCM, WAV, MP3, Opus} {
		t.Run(string(format), func(t *testing.T) {
			out := &recordingWriter{}
			encoder, err := NewEncoder(format, out)
			require.NoError(t, err)
			defer func() { assert.NoError(t, encoder.Close()) }()

			_, err = encoder.Write(pcm)
			require.NoError(t, err)

			assert.Greater(t, out.count(), len(pcm)/50, "encoder held back half a second of audio")
		})
	}
}

// failingWriter accepts writes until limit bytes have gone through and then
// fails every write, as a response writer does once the client has gone.
type failingWriter struct {
	limit int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		return 0, errClientGone
	}

	w.limit -= len(p)
	return len(p), nil
}

func TestNewEncoder_Opus_CloseAfterFailedWriteFreesEncoder(t *testing.T) {
	pcm := s16le(tone(outputRate, toneSeconds, toneAmplitude))
	created, destroyed := opusCreated.Load(), opusDestroyed.Load()

	encoder, err := NewEncoder(Opus, &failingWriter{limit: opusHeadersBytes})
	require.NoError(t, err)

	_, err = io.Copy(encoder, bytes.NewReader(pcm))
	require.ErrorIs(t, err, errClientGone)
	require.ErrorIs(t, encoder.Close(), errClientGone)
	require.NoError(t, encoder.Close(), "a second Close is a no-op")

	assert.Equal(t, int64(1), opusCreated.Load()-created)
	assert.Equal(t, int64(1), opusDestroyed.Load()-destroyed)
}

func TestNewEncoder_Opus_HeaderWriteFailureFreesEncoder(t *testing.T) {
	created, destroyed := opusCreated.Load(), opusDestroyed.Load()

	encoder, err := NewEncoder(Opus, &failingWriter{})

	require.ErrorIs(t, err, errClientGone)
	assert.True(t, encoder == nil, "want a nil interface, got %#v", encoder)
	assert.Equal(t, opusCreated.Load()-created, opusDestroyed.Load()-destroyed)
}

type panickingWriter struct{}

func (panickingWriter) Write([]byte) (int, error) {
	panic(errClientGone)
}

func TestNewEncoder_Opus_HeaderWritePanicFreesEncoder(t *testing.T) {
	created, destroyed := opusCreated.Load(), opusDestroyed.Load()

	assert.PanicsWithValue(t, errClientGone, func() {
		_, err := NewEncoder(Opus, panickingWriter{})
		assert.NoError(t, err)
	})

	assert.Equal(t, int64(1), opusCreated.Load()-created)
	assert.Equal(t, opusCreated.Load()-created, opusDestroyed.Load()-destroyed, "libopus encoder leaked")
}

// pacedBody hands out PCM in 20 ms pieces until the request ends, then
// fails the way an upstream body tied to the request context does.
type pacedBody struct {
	ctx   context.Context
	piece []byte
}

func (b *pacedBody) Read(p []byte) (int, error) {
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-time.After(time.Millisecond):
	}

	return copy(p, b.piece), nil
}

type flushingWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f *flushingWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		return n, err
	}

	return n, f.rc.Flush()
}

func streamOpus(t *testing.T) http.HandlerFunc {
	piece := s16le(tone(outputRate, toneSeconds, toneAmplitude))[:opusFrameSamples*outputSampleBytes]

	return func(w http.ResponseWriter, r *http.Request) {
		encoder, err := NewEncoder(Opus, &flushingWriter{w: w, rc: http.NewResponseController(w)})
		if !assert.NoError(t, err) {
			return
		}

		_, copyErr := io.Copy(encoder, &pacedBody{ctx: r.Context(), piece: piece})
		assert.Error(t, errors.Join(copyErr, encoder.Close()), "a disconnect must surface as an error")
	}
}

func disconnectMidStream(t *testing.T, url string) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	_, err = io.ReadFull(resp.Body, make([]byte, oggHeaderBytes))
	require.NoError(t, err, "no Ogg page before the disconnect")
}

func TestNewEncoder_Opus_ClientDisconnectsFreeEveryEncoder(t *testing.T) {
	server := httptest.NewServer(streamOpus(t))
	defer server.Close()

	created, destroyed := opusCreated.Load(), opusDestroyed.Load()
	for range disconnectRounds {
		disconnectMidStream(t, server.URL)
	}
	server.Close()

	assert.Equal(t, int64(disconnectRounds), opusCreated.Load()-created)
	assert.Equal(t, opusCreated.Load()-created, opusDestroyed.Load()-destroyed, "libopus encoders leaked")
}
