package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	streamedWAV    = "RIFF\xff\xff\xff\xffWAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x80\x3e\x00\x00\x00\x7d\x00\x00\x02\x00\x10\x00data\xff\xff\xff\xff\x01\x02\x03\x04"
	qwenPrefix     = `^language \w+<asr_text>`
	asrWhitelist   = "ogg,mp3,flac,wav,mov,matroska,webm,aac"
	transcriptText = "hello"
)

func transcoding(cfg *Config) {
	cfg.TranscodeASRInput = true
}

func TestServer_HandleTranscription_TextIsBare(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	resp := h.transcribe(map[string]string{"model": "whisper-1", "response_format": "text"}, []byte("clip"))

	assert.Equal(t, transcriptText, string(readBody(t, resp)))
	assert.True(t, strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain"), resp.Header.Get("Content-Type"))

	call := h.upstream.lastCall()
	assert.Equal(t, asrRoute, call.path)
	assert.Equal(t, asrModel, call.fields["model"])
	assert.Equal(t, "clip", string(call.files["file"]))
}

func TestServer_HandleTranscription_JSONDropsType(t *testing.T) {
	for _, format := range []string{"json", "verbose_json", ""} {
		h := newHarness(t, serveTranscript(t, transcriptText))

		fields := map[string]string{"model": "whisper-1", "language": "en", "temperature": "0"}
		if format != "" {
			fields["response_format"] = format
		}
		resp := h.transcribe(fields, []byte("clip"))

		var body map[string]any
		require.NoError(t, json.Unmarshal(readBody(t, resp), &body), "format %q", format)
		assert.Equal(t, map[string]any{"text": transcriptText}, body, "format %q", format)
	}
}

func TestServer_HandleTranscription_StripsPrefixRegex(t *testing.T) {
	h := newHarness(t, serveTranscript(t, "language English<asr_text>hello"), func(cfg *Config) {
		cfg.ASRPrefixRegex = qwenPrefix
	})

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte("clip"))

	assert.Equal(t, transcriptText, string(readBody(t, resp)))
}

func TestServer_HandleTranscription_KeepsTextWithoutPrefixMatch(t *testing.T) {
	h := newHarness(t, serveTranscript(t, "hello <asr_text>"), func(cfg *Config) {
		cfg.ASRPrefixRegex = `language \w+<asr_text>`
	})

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte("clip"))

	assert.Equal(t, "hello <asr_text>", string(readBody(t, resp)))
}

func TestServer_HandleTranscription_TranscodesInput(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText), transcoding)

	resp := h.transcribe(map[string]string{"response_format": "json"}, []byte("ogg bytes"))
	readBody(t, resp)

	assert.True(t, containsRun(h.ffmpeg.Args(), "-ar", "16000", "-ac", "1"), "ffmpeg args %v lack 16 kHz mono", h.ffmpeg.Args())

	call := h.upstream.lastCall()
	assert.Equal(t, transcodedName, call.names["file"])
	assert.Equal(t, "ogg bytes", string(call.files["file"]), "want the ffmpeg output")
}

func TestServer_HandleTranscription_WhitelistsInputFormats(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText), transcoding)

	resp := h.transcribe(map[string]string{"response_format": "json"}, []byte("ogg bytes"))
	readBody(t, resp)

	args := h.ffmpeg.Args()
	flag := slices.Index(args, "-format_whitelist")
	input := slices.Index(args, "-i")
	require.True(t, flag >= 0 && flag+1 < len(args), "ffmpeg args %v lack -format_whitelist", args)
	assert.Equal(t, asrWhitelist, args[flag+1])
	assert.Less(t, flag, input, "ffmpeg args %v put -format_whitelist after -i", args)
}

func TestServer_HandleTranscription_TranscodingDecodesWAV(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText), transcoding)

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(streamedWAV))

	require.Equal(t, http.StatusOK, resp.StatusCode, string(readBody(t, resp)))
	assert.True(t, containsRun(h.ffmpeg.Args(), "-ar", "16000", "-ac", "1"), "ffmpeg args %v, want the WAV decoded to 16 kHz mono", h.ffmpeg.Args())
	assert.Equal(t, transcodedName, h.upstream.lastCall().names["file"], "want the decoded WAV sent upstream")
}

func TestServer_HandleTranscription_TranscodingDecodeFailureIs400(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText), transcoding)
	h.ffmpeg.FailWith(t, "corrupt input packet in stream 0")

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(streamedWAV))

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "corrupt input packet")
	assert.Zero(t, h.upstream.callCount(), "an undecodable WAV reached upstream")
}

func TestServer_HandleTranscription_NoTranscodingForwardsWAV(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte(streamedWAV))

	require.Equal(t, http.StatusOK, resp.StatusCode, string(readBody(t, resp)))
	assert.Nil(t, h.ffmpeg.Args(), "ffmpeg ran, want WAV forwarded untouched")

	call := h.upstream.lastCall()
	assert.Equal(t, "clip.wav", call.names["file"])
	assert.Equal(t, streamedWAV, string(call.files["file"]), "want the uploaded WAV byte for byte")
}

func TestServer_HandleTranscription_OversizedTranscriptIs502(t *testing.T) {
	const oversized = 2 << 20
	h := newHarness(t, serveTranscript(t, strings.Repeat("a", oversized)))

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte("clip"))

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "transcript is larger than")
}

func TestServer_HandleTranscription_Upstream5xxIsJSONError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "asr unavailable", http.StatusServiceUnavailable)
	})

	resp := h.transcribe(map[string]string{"response_format": "text"}, []byte("clip"))

	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "asr unavailable")
}

func TestServer_HandleTranscription_RejectsBadRequests(t *testing.T) {
	h := newHarness(t, serveTranscript(t, transcriptText))

	srt := h.transcribe(map[string]string{"response_format": "srt"}, []byte("clip"))
	assert.Equal(t, http.StatusBadRequest, srt.StatusCode, "srt")

	noFile := h.post("/audio/transcriptions", "multipart/form-data; boundary=x", strings.NewReader("--x--\r\n"))
	assert.Equal(t, http.StatusBadRequest, noFile.StatusCode, "missing file")

	notMultipart := h.post("/audio/transcriptions", "application/json", strings.NewReader("{}"))
	assert.Equal(t, http.StatusBadRequest, notMultipart.StatusCode, "not multipart")

	assert.Zero(t, h.upstream.callCount())
}
