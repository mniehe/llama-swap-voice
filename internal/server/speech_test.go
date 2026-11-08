package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/fakeffmpeg"
)

const (
	upstreamHeaderBytes = len(upstreamHeader)
	mp3FrameSync        = "\xff\xf3"
)

func TestServer_HandleSpeech_MapsVoiceToReferenceFile(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	resp := h.speech(speechPath, map[string]any{
		"model": "tts-1", "voice": "narrator", "input": "Hello there", "response_format": "wav", "instructions": "calm",
	})
	require.Equal(t, http.StatusOK, resp.StatusCode, string(readBody(t, resp)))

	call := h.upstream.lastCall()
	want := map[string]string{"input": "Hello there", "lang": "en", "response_format": "wav", "stream": "true"}
	for key, value := range want {
		assert.Equal(t, value, call.fields[key], "upstream field %s", key)
	}
	assert.Equal(t, ttsRoute, call.path)
	assert.Equal(t, narratorRef, string(call.files["speaker_ref"]))
}

func TestServer_HandleSpeech_AliasAndDefaultVoiceShareReference(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	for _, voice := range []string{"bm_fable", ""} {
		resp := h.speech("/audio/speech", map[string]any{"voice": voice, "input": "hi", "response_format": "wav"})
		require.Equal(t, http.StatusOK, resp.StatusCode, "voice %q", voice)

		assert.Equal(t, aliceRef, string(h.upstream.lastCall().files["speaker_ref"]), "voice %q", voice)
	}
}

func TestServer_HandleSpeech_UnknownVoiceIs400(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	for _, voice := range []string{"alloy", "/etc/passwd", "http://example.com/voice.wav"} {
		resp := h.speech(speechPath, map[string]any{"voice": voice, "input": "hi"})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, "voice %q", voice)

		assert.Contains(t, errorMessage(t, resp), "unknown voice", "voice %q", voice)
	}

	assert.Zero(t, h.upstream.callCount(), "upstream called for unknown voices")
}

func TestServer_HandleSpeech_RejectsInvalidInput(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	tests := map[string]map[string]any{
		"empty input":    {"input": "  "},
		"too long":       {"input": strings.Repeat("a", harnessMaxChars+1)},
		"bad format":     {"input": "hi", "response_format": "ogg"},
		"speed too low":  {"input": "hi", "speed": 0.1},
		"speed too high": {"input": "hi", "speed": 4.5},
	}
	for name, body := range tests {
		resp := h.speech(speechPath, body)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode, name)
	}

	assert.Zero(t, h.upstream.callCount(), "upstream called for invalid input")
}

func TestServer_HandleSpeech_WAVPassThroughIsByteEqual(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	resp := h.speech(speechPath, map[string]any{"voice": "alice", "input": "hi", "response_format": "wav"})

	assert.Equal(t, []byte(upstreamAudio), readBody(t, resp))
	assert.Equal(t, contentTypeWAV, resp.Header.Get("Content-Type"))
	assert.Nil(t, h.ffmpeg.Args(), "ffmpeg ran for wav pass-through")
}

func TestServer_HandleSpeech_PCMIsUpstreamSamplesWithoutHeader(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm"})
	body := readBody(t, resp)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "wav", h.upstream.lastCall().fields["response_format"], "llama-server's own pcm is float32")
	assert.Equal(t, contentTypePCM, resp.Header.Get("Content-Type"))
	assert.Equal(t, upstreamAudio[upstreamHeaderBytes:], string(body))
	assert.Nil(t, h.ffmpeg.Args(), "ffmpeg ran at normal speed")
}

func TestServer_HandleSpeech_SpeedStretchesRawPCMThroughFFmpeg(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm", "speed": 1.5})
	body := readBody(t, resp)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	args := h.ffmpeg.Args()
	assert.True(t, containsRun(args, "-f", "s16le", "-ar", "24000", "-ac", "1", "-i", "pipe:0"), "ffmpeg args %v lack raw PCM input", args)
	assert.True(t, containsRun(args, "-filter:a", "atempo=1.5", "-f", "s16le", "-ar", "24000", "-ac", "1", "pipe:1"), "ffmpeg args %v", args)
	assert.Equal(t, upstreamAudio[upstreamHeaderBytes:], string(body), "want the fake ffmpeg's echo of the upstream samples")
}

func TestServer_HandleSpeech_EncodesInProcess(t *testing.T) {
	tests := []struct {
		format      string
		contentType string
		magic       string
	}{
		{"opus", contentTypeOgg, "OggS"},
		{"mp3", contentTypeMP3, mp3FrameSync},
	}
	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			h := newHarness(t, serveWAV(t))

			resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": tt.format})
			body := readBody(t, resp)

			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			assert.Equal(t, tt.contentType, resp.Header.Get("Content-Type"))
			assert.True(t, strings.HasPrefix(string(body), tt.magic), "body starts %q", body[:min(len(body), len(tt.magic))])
			assert.Nil(t, h.ffmpeg.Args(), "ffmpeg ran at normal speed")
			assert.Equal(t, "wav", h.upstream.lastCall().fields["response_format"])
		})
	}
}

func TestServer_HandleSpeech_AACAndFLACAre400(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	for _, format := range []string{"aac", "flac"} {
		resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": format})
		require.Equal(t, http.StatusBadRequest, resp.StatusCode, format)

		assert.Contains(t, errorMessage(t, resp), `unsupported response_format "`+format+`"`)
	}

	assert.Zero(t, h.upstream.callCount(), "upstream called for a rejected format")
}

func TestServer_HandleSpeech_SpeedStretchesBeforeEncoding(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "mp3", "speed": 1.15})
	body := readBody(t, resp)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.True(t, containsRun(h.ffmpeg.Args(), "-filter:a", "atempo=1.15"), "ffmpeg args %v", h.ffmpeg.Args())
	assert.True(t, strings.HasPrefix(string(body), mp3FrameSync), "want MP3 frames, not ffmpeg output")
}

func TestServer_HandleSpeech_WAVWithSpeedIsStretched(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "wav", "speed": 0.9})
	body := readBody(t, resp)

	args := h.ffmpeg.Args()
	assert.True(t, containsRun(args, "-filter:a", "atempo=0.9"), "ffmpeg args %v", args)
	assert.Equal(t, contentTypeWAV, resp.Header.Get("Content-Type"))
	assert.Equal(t, upstreamAudio, string(body), "want a streamed WAV header around the fake ffmpeg's echo")
}

func TestServer_HandleSpeech_NonWAVUpstreamIsJSONError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeJSON)
		writeOrFail(t, w, `{"error":"not audio"}`)
	})

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "opus"})

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "not a WAV stream")
}

func TestServer_HandleSpeech_HeaderOnlyUpstreamIsJSONError(t *testing.T) {
	h := newHarness(t, serveBody(t, []byte(upstreamHeader)))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm"})

	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Equal(t, "upstream sent no audio", errorMessage(t, resp))
}

func TestServer_HandleSpeech_HalfSampleUpstreamIsJSONError(t *testing.T) {
	for _, format := range []string{"pcm", "mp3", "opus"} {
		t.Run(format, func(t *testing.T) {
			h := newHarness(t, serveBody(t, []byte(upstreamHeader+"\x01")))

			resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": format})

			require.Equal(t, http.StatusBadGateway, resp.StatusCode, "want no 200 for half a sample")
			assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
			assert.Contains(t, errorMessage(t, resp), "inside a sample")
		})
	}
}

func TestServer_HandleSpeech_TrailingHalfSampleAbortsStream(t *testing.T) {
	for _, format := range []string{"pcm", "mp3", "opus"} {
		t.Run(format, func(t *testing.T) {
			h := newHarness(t, serveBody(t, []byte(upstreamHeader+"\x01\x02\x03")))

			resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": format})
			require.Equal(t, http.StatusOK, resp.StatusCode, "want 200 once a whole sample arrived")

			_, err := io.ReadAll(resp.Body)
			assert.Error(t, err, "a stream ending inside a sample ended cleanly, want a transport error")
		})
	}
}

func TestServer_HandleSpeech_Upstream5xxIsJSONError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model crashed", http.StatusInternalServerError)
	})

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "opus"})

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Equal(t, contentTypeJSON, resp.Header.Get("Content-Type"))
	assert.Contains(t, errorMessage(t, resp), "model crashed")
	assert.Nil(t, h.ffmpeg.Args(), "ffmpeg ran after upstream failure")
}

func TestServer_HandleSpeech_ClientCancelStopsUpstreamAndFFmpeg(t *testing.T) {
	upstreamDone := make(chan time.Time, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, upstreamAudio)
		assert.NoError(t, http.NewResponseController(w).Flush())
		select {
		case <-r.Context().Done():
			upstreamDone <- time.Now()
		case <-time.After(stuckTimeout):
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	payload, err := json.Marshal(map[string]any{"input": "a long story", "response_format": "pcm", "speed": 1.5})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+speechPath, bytes.NewReader(payload))
	require.NoError(t, err)
	resp := h.do(req)

	first := make([]byte, len(upstreamAudio)-upstreamHeaderBytes)
	_, err = io.ReadFull(resp.Body, first)
	require.NoError(t, err, "no streamed bytes before cancel")

	cancelledAt := time.Now()
	cancel()

	select {
	case doneAt := <-upstreamDone:
		assert.LessOrEqual(t, doneAt.Sub(cancelledAt), waitTimeout, "upstream request ended too long after cancel")
	case <-time.After(waitTimeout):
		require.Fail(t, "upstream request still open 1s after client cancel")
	}

	_, ok := h.ffmpeg.WaitFor(waitTimeout, fakeffmpeg.EventSIGTERM, fakeffmpeg.EventEOF)
	require.True(t, ok, "ffmpeg saw neither SIGTERM nor EOF within 1s; events %v", h.ffmpeg.Events())
}

func TestServer_HandleSpeech_FFmpegFailureIsJSONError(t *testing.T) {
	h := newHarness(t, serveWAV(t))
	h.ffmpeg.FailWith(t, "Invalid argument")

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "mp3", "speed": 1.5})

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "Invalid argument")
}

func TestServer_HandleSpeech_TruncatedUpstreamIsClientError(t *testing.T) {
	const promisedBytes = 100000
	truncated := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		w.Header().Set("Content-Length", strconv.Itoa(promisedBytes))
		writeOrFail(t, w, upstreamAudio+strings.Repeat("a", 5000))
	}

	for _, format := range []string{"wav", "pcm", "mp3", "opus"} {
		t.Run(format, func(t *testing.T) {
			h := newHarness(t, truncated)

			resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": format})
			require.Equal(t, http.StatusOK, resp.StatusCode, "want 200 before the stream broke")

			_, err := io.ReadAll(resp.Body)
			assert.Error(t, err, "truncated stream ended cleanly, want a transport error")
		})
	}
}
