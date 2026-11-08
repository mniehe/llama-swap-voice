package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/audio"
	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	streamedWAV    = "RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xff\x01\x02\x03\x04"
	streamedPCM    = "\x01\x02\x03\x04"
	qwenPrefix     = `^language \w+<asr_text>`
	transcriptText = "hello"

	fixtureDir        = "../audio/testdata"
	asrRate           = 16000
	fixtureFrames     = 2 * asrRate
	pcmFormat         = 1
	bytesPerSample    = 2
	wavHeaderBytes    = 44
	longWAVSeconds    = 1300
	narrowbandRate    = 8000
	unsupportedPhrase = "unsupported"
	corruptedPhrase   = "corrupted"
	tooLongMessage    = "audio is longer than 20 minutes"
	corruptMessage    = "could not decode audio: corrupted audio"
	unsupportedMsg    = "could not decode audio: unsupported audio format"
	longDecodeTimeout = time.Minute
	slowDecodeTimeout = 2 * time.Second
	slowDecodeCopies  = 570
	flacUpload        = "fLaC\x00\x00\x00\x22junkjunkjunk"

	// A 50-byte MPEG-2 Layer III frame that makes go-mp3 v0.3.4 index out of range.
	panickingMP3 = "\xff\xf3 0000000\xd800000000000000000000000000000000000000000"
)

var asrFormat = wavInfo{format: pcmFormat, channels: 1, rate: asrRate, bits: 16}

type wavInfo struct {
	format   uint16
	channels uint16
	rate     uint32
	bits     uint16
	data     []byte
}

func parseWAV(t *testing.T, data []byte) wavInfo {
	t.Helper()

	require.GreaterOrEqual(t, len(data), wavHeaderBytes, "too short for a WAV header")
	require.Equal(t, "RIFF", string(data[0:4]))
	require.Equal(t, "WAVE", string(data[8:12]))
	require.Equal(t, "data", string(data[36:40]))

	return wavInfo{
		format:   binary.LittleEndian.Uint16(data[20:22]),
		channels: binary.LittleEndian.Uint16(data[22:24]),
		rate:     binary.LittleEndian.Uint32(data[24:28]),
		bits:     binary.LittleEndian.Uint16(data[34:36]),
		data:     data[wavHeaderBytes:],
	}
}

func (w wavInfo) layout() wavInfo {
	return wavInfo{format: w.format, channels: w.channels, rate: w.rate, bits: w.bits}
}

func silentWAV(rate, seconds uint32) []byte {
	dataBytes := rate * seconds * bytesPerSample
	header := make([]byte, wavHeaderBytes, wavHeaderBytes+int(dataBytes))
	copy(header, "RIFF")
	binary.LittleEndian.PutUint32(header[4:], wavHeaderBytes-8+dataBytes)
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], pcmFormat)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], rate)
	binary.LittleEndian.PutUint32(header[28:], rate*bytesPerSample)
	binary.LittleEndian.PutUint16(header[32:], bytesPerSample)
	binary.LittleEndian.PutUint16(header[34:], 8*bytesPerSample)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], dataBytes)

	return append(header, make([]byte, dataBytes)...)
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(fixtureDir, name))
	require.NoError(t, err)

	return data
}

func TestServer_HandleTranscription_TextIsBare(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	resp := h.transcribe(map[string]string{"model": "whisper-1", "response_format": "text"}, []byte(streamedWAV))

	assert.Equal(t, transcriptText, string(readBody(t, resp)))
	assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain"), resp.Header.Get("Content-Type"))

	call := h.upstream.lastCall()
	assert.Equal(t, asrRoute, call.path)
	assert.Equal(t, asrModel, call.fields["model"])
	assert.Equal(t, transcodedName, call.names["file"])
}

func TestServer_HandleTranscription_JSONDropsType(t *testing.T) {
	for _, format := range []string{"json", "verbose_json", ""} {
		h := newHarness(t, serveTranscript(t, transcriptText))

		fields := map[string]string{"model": "whisper-1", "language": "en", "temperature": "0"}
		if format != "" {
			fields["response_format"] = format
		}
		resp := h.transcribe(fields, []byte(streamedWAV))

		var body map[string]any
		require.NoError(t, json.Unmarshal(readBody(t, resp), &body), "format %q", format)
		assert.Equal(t, map[string]any{"text": transcriptText}, body, "format %q", format)
	}
}

func TestServer_HandleTranscription_StripsPrefixRegex(t *testing.T) {
	h := newHarness(t, serveTranscript(t, "language English<asr_text>hello"), func(cfg *Config) {
		cfg.ASRPrefixRegex = qwenPrefix
	})

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(streamedWAV))

	assert.Equal(t, transcriptText, string(readBody(t, resp)))
}

func TestServer_HandleTranscription_KeepsTextWithoutPrefixMatch(t *testing.T) {
	h := newHarness(t, serveTranscript(t, "hello <asr_text>"), func(cfg *Config) {
		cfg.ASRPrefixRegex = `language \w+<asr_text>`
	})

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(streamedWAV))

	assert.Equal(t, "hello <asr_text>", string(readBody(t, resp)))
}

func TestServer_HandleTranscription_OpusBecomes16kMonoWAV(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	resp := h.transcribe(map[string]string{"response_format": "text"}, readFixture(t, "speech.opus"))

	require.Equal(t, http.StatusOK, resp.StatusCode, string(readBody(t, resp)))

	call := h.upstream.lastCall()
	assert.Equal(t, transcodedName, call.names["file"])

	wav := parseWAV(t, call.files["file"])
	assert.Equal(t, asrFormat, wav.layout())
	assert.Len(t, wav.data, fixtureFrames*bytesPerSample, "want exactly 2 s at 16 kHz after pre-skip and end trimming")
}

func TestServer_HandleTranscription_ReencodesWAV(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(streamedWAV))

	require.Equal(t, http.StatusOK, resp.StatusCode, string(readBody(t, resp)))

	call := h.upstream.lastCall()
	assert.Equal(t, transcodedName, call.names["file"], "want the normalised WAV, not the upload")

	wav := parseWAV(t, call.files["file"])
	assert.Equal(t, asrFormat, wav.layout())
	assert.Equal(t, streamedPCM, string(wav.data), "16 kHz mono s16 samples survive re-encoding unchanged")
}

func TestServer_HandleTranscription_M4AIs400Unsupported(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	resp := h.transcribe(map[string]string{"response_format": "text"}, readFixture(t, "speech.m4a"))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	message := errorMessage(t, resp)
	assert.Contains(t, message, unsupportedPhrase)
	assert.Equal(t, unsupportedMsg, message)
	assert.Zero(t, h.upstream.callCount(), "an M4A upload reached upstream")
}

func TestServer_HandleTranscription_FLACIs400Unsupported(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(flacUpload))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), unsupportedPhrase)
	assert.Zero(t, h.upstream.callCount(), "a FLAC upload reached upstream")
}

// Decoding 20 minutes takes about a second, and over ten under the race
// detector, so the request gets more than the harness's usual timeout.
func TestServer_HandleTranscription_LongNarrowbandWAVIs400(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText), func(cfg *Config) {
		cfg.RequestTimeout = longDecodeTimeout
	})

	resp := h.transcribe(map[string]string{"response_format": "text"}, silentWAV(narrowbandRate, longWAVSeconds))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, tooLongMessage, errorMessage(t, resp))
	assert.Zero(t, h.upstream.callCount(), "a 1300 s WAV reached upstream")
}

func TestServer_HandleTranscription_DecoderPanicIs400(t *testing.T) {
	logs := &logBuffer{}
	h := newHarness(t, serveTranscript(t, transcriptText))
	h.logTo(logs)

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(panickingMP3))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	message := errorMessage(t, resp)
	assert.Contains(t, message, corruptedPhrase)
	assert.Equal(t, corruptMessage, message)
	assert.Zero(t, h.upstream.callCount(), "an undecodable MP3 reached upstream")

	line := logs.find(t, slog.LevelError, resp.Header.Get(requestIDHeader))
	assert.Contains(t, line["error"], "index out of range")
	assert.Contains(t, line["stack"], "goroutine")
	assert.Contains(t, line["stack"], "go-mp3")
}

func TestServer_HandleTranscription_CorruptDetailIsLoggedNotSent(t *testing.T) {
	logs := &logBuffer{}
	h := newHarness(t, serveTranscript(t, transcriptText))
	h.logTo(logs)
	truncated := silentWAV(narrowbandRate, 1)
	truncated = truncated[:len(truncated)-bytesPerSample]

	resp := h.transcribe(map[string]string{"response_format": "text"}, truncated)

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, corruptMessage, errorMessage(t, resp))

	line := logs.find(t, slog.LevelWarn, resp.Header.Get(requestIDHeader))
	assert.Contains(t, line["error"], "WAV")
}

func TestServer_DecodeFailure_ExpiredContextIs504(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/audio/transcriptions", http.NoBody)

	for _, err := range []error{audio.ErrCorrupt, audio.ErrUnsupported, audio.ErrTooLong, context.DeadlineExceeded} {
		w := httptest.NewRecorder()

		h.service.decodeFailure(w, r, err)

		assert.Equal(t, http.StatusGatewayTimeout, w.Code, "expired context with %v", err)
	}
}

// The upload decodes honestly to almost 20 minutes, which takes seconds, so
// the request deadline passes mid-decode and the 504 rides on the write slack.
func TestServer_HandleTranscription_SlowDecodeIs504(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText), func(cfg *Config) {
		cfg.RequestTimeout = slowDecodeTimeout
	})
	upload := bytes.Repeat(readFixture(t, "speech.mp3"), slowDecodeCopies)

	start := time.Now()
	resp := h.transcribe(map[string]string{"response_format": "text"}, upload)

	assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "decoding audio")
	assert.Less(t, time.Since(start), slowDecodeTimeout+responseWriteSlack)
	assert.Zero(t, h.upstream.callCount(), "a timed-out decode reached upstream")
}

func TestServer_HandleTranscription_OversizedTranscriptIs502(t *testing.T) {
	const oversized = 2 << 20
	h := newHarness(t, serveTranscript(t, strings.Repeat("a", oversized)))

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(streamedWAV))

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "transcript is larger than")
}

func TestServer_HandleTranscription_Upstream5xxIsJSONError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "asr unavailable", http.StatusServiceUnavailable)
	})

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(streamedWAV))

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "asr unavailable")
}

func TestServer_HandleTranscription_RejectsBadRequests(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	srt := h.transcribe(map[string]string{"response_format": "srt"}, []byte(streamedWAV))
	assert.Equal(t, http.StatusBadRequest, srt.StatusCode, "srt")

	noFile := h.post("/audio/transcriptions", "multipart/form-data; boundary=x", strings.NewReader("--x--\r\n"))
	assert.Equal(t, http.StatusBadRequest, noFile.StatusCode, "missing file")

	notMultipart := h.post("/audio/transcriptions", "application/json", strings.NewReader("{}"))
	assert.Equal(t, http.StatusBadRequest, notMultipart.StatusCode, "not multipart")

	assert.Zero(t, h.upstream.callCount())
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

// find returns the first JSON log line at level that carries the request id.
func (b *logBuffer) find(t *testing.T, level slog.Level, requestID string) map[string]any {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	require.NotEmpty(t, requestID, "response has no request id")
	scanner := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	scanner.Buffer(nil, len(b.buf.Bytes())+1)
	for scanner.Scan() {
		var line map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line))
		if line["level"] == level.String() && line["request_id"] == requestID {
			return line
		}
	}

	require.Fail(t, "no log line", "want level %s for request %s in:\n%s", level, requestID, b.buf.String())
	return nil
}

// logTo rebuilds the harness's service around a logger that writes to logs,
// so a test can assert on what the server logged.
func (h *harness) logTo(logs *logBuffer) {
	h.t.Helper()

	client, err := upstream.New(h.upstream.server.URL, ttsModel, asrModel, &http.Client{})
	require.NoError(h.t, err)

	h.service, err = New(&h.cfg, client, slog.New(slog.NewJSONHandler(logs, nil)))
	require.NoError(h.t, err)

	h.server = httptest.NewServer(h.service.Handler())
	h.t.Cleanup(h.server.Close)
}
