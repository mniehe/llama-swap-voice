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

func TestServer_HandleSpeech_WAVIsReencodedWithStreamedHeader(t *testing.T) {
	fixture := readFixture(t, speechFixture)
	h := newHarness(t, serveBody(t, fixture))

	resp := h.speech(speechPath, map[string]any{"voice": "alice", "input": "hi", "response_format": "wav"})
	body := readBody(t, resp)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, contentTypeWAV, resp.Header.Get("Content-Type"))
	require.Greater(t, len(body), wavHeaderBytes)
	streamedHeader := streamedSpeech(t, 0)
	assert.Equal(t, streamedHeader, body[:wavHeaderBytes], "want the streamed header with unknown sizes")
	assert.Equal(t, fixture[wavHeaderBytes:], body[wavHeaderBytes:])
	assert.Len(t, decodeWAV(t, body), fixtureSamples(t)*bytesPerSample)
}

func TestServer_HandleSpeech_PCMIsUpstreamSamplesWithoutHeader(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm"})
	body := readBody(t, resp)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "wav", h.upstream.lastCall().fields["response_format"], "llama-server's own pcm is float32")
	assert.Equal(t, contentTypePCM, resp.Header.Get("Content-Type"))
	assert.Equal(t, upstreamAudio[upstreamHeaderBytes:], string(body))
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
	h := newHarness(t, serveBody(t, streamedSpeech(t, fixtureSamples(t))))

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "mp3", "speed": 1.15})
	body := readBody(t, resp)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.True(t, strings.HasPrefix(string(body), mp3FrameSync), "want MP3 frames")
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
	for _, format := range []string{"wav", "pcm", "mp3", "opus"} {
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
	for _, format := range []string{"wav", "pcm", "mp3", "opus"} {
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
}

func TestServer_HandleSpeech_ClientCancelStopsUpstream(t *testing.T) {
	wav := streamedSpeech(t, fixtureSamples(t))
	upstreamDone := make(chan time.Time, 1)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, string(wav[:wavHeaderBytes+pausedAudioBytes]))
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

	first := make([]byte, earlyBytes)
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

func TestSpeechPCM_SpeedOutsideStretcherRangeIsInternalError(t *testing.T) {
	pcm, status, err := speechPCM(strings.NewReader(upstreamAudio), 0)

	require.Error(t, err)
	assert.Nil(t, pcm)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Contains(t, err.Error(), "between 0.25 and 4")
}
