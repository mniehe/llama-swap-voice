package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/upstream"
)

const (
	upstreamHeaderBytes = len(upstreamHeader)
	mp3FrameSync        = "\xff\xf3"
	queueFull           = `{"error":{"message":"queue full"}}`
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

func TestServer_HandleSpeech_QwenTTSSendsVoiceNameAsJSON(t *testing.T) {
	h := newBackendHarness(t, upstream.QwenTTS, serveWAV(t), func(cfg *Config) {
		cfg.Voices["robbie"] = filepath.Join(t.TempDir(), "never-created.wav")
	})

	resp := h.speech(speechPath, map[string]any{"voice": "robbie", "input": "hello"})
	body := readBody(t, resp)

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.True(t, strings.HasPrefix(string(body), mp3FrameSync), "want MP3 audio")
	call := h.upstream.lastCall()
	assert.Equal(t, qwenTTSRoute, call.path)
	assert.Equal(t, contentTypeJSON, call.contentType)
	assert.Equal(t, map[string]any{"voice": "robbie", "input": "hello", "response_format": "wav"}, call.json)
	assert.Empty(t, call.fields, "want no multipart fields")
	assert.Empty(t, call.files, "want no multipart files")
}

func answerEnvelope(t *testing.T, code int, retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set(retryAfterHeader, retryAfter)
		}
		w.Header().Set("Content-Type", contentTypeJSON)
		w.WriteHeader(code)
		writeOrFail(t, w, queueFull)
	}
}

func TestServer_HandleSpeech_UpstreamErrorEnvelopeIsDecoded(t *testing.T) {
	tests := []struct {
		code       int
		retryAfter string
	}{
		{http.StatusServiceUnavailable, "5"},
		{http.StatusBadRequest, ""},
		{http.StatusRequestEntityTooLarge, ""},
		{http.StatusInternalServerError, ""},
	}

	for _, backend := range []upstream.Backend{upstream.LlamaCPP, upstream.QwenTTS} {
		for _, tt := range tests {
			t.Run(string(backend)+"/"+strconv.Itoa(tt.code), func(t *testing.T) {
				h := newBackendHarness(t, backend, answerEnvelope(t, tt.code, tt.retryAfter))

				resp := h.speech(speechPath, map[string]any{"input": "hi"})

				require.Equal(t, tt.code, resp.StatusCode)
				assert.Equal(t, tt.retryAfter, resp.Header.Get(retryAfterHeader))
				message := errorMessage(t, resp)
				assert.Contains(t, message, "queue full")
				assert.NotContains(t, message, `{"error"`)
				assert.Equal(t, 1, h.upstream.callCount())
			})
		}
	}
}

func dropConnection(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := http.NewResponseController(w).Hijack()
		if !assert.NoError(t, err) {
			return
		}
		assert.NoError(t, conn.Close())
	}
}

func answerStatus(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, http.StatusText(code), code)
	}
}

// inTurn answers the first request with first and every later one with rest.
func inTurn(first, rest http.HandlerFunc) http.HandlerFunc {
	var calls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			first(w, r)
			return
		}
		rest(w, r)
	}
}

func TestServer_HandleSpeech_RetriesOnceBeforeAudio(t *testing.T) {
	tests := map[string]struct {
		respond  func(t *testing.T) http.HandlerFunc
		wantCode int
	}{
		"dropped connection then audio": {func(t *testing.T) http.HandlerFunc { return inTurn(dropConnection(t), serveWAV(t)) }, http.StatusOK},
		"502 then audio":                {func(t *testing.T) http.HandlerFunc { return inTurn(answerStatus(http.StatusBadGateway), serveWAV(t)) }, http.StatusOK},
		"502 twice":                     {func(*testing.T) http.HandlerFunc { return answerStatus(http.StatusBadGateway) }, http.StatusBadGateway},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tt.respond(t))

			resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm"})
			body := readBody(t, resp)

			require.Equal(t, tt.wantCode, resp.StatusCode, string(body))
			assert.Len(t, h.upstream.callsTo(ttsRoute), 2)
			if tt.wantCode == http.StatusOK {
				assert.Equal(t, upstreamAudio[upstreamHeaderBytes:], string(body))
			}
		})
	}
}

func TestServer_HandleSpeech_RetryIsLoggedWithRequestID(t *testing.T) {
	h := newHarness(t, inTurn(answerStatus(http.StatusBadGateway), serveWAV(t)))
	logs := &logBuffer{}
	h.logTo(logs)

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	readBody(t, resp)

	line := logs.find(t, slog.LevelInfo, resp.Header.Get(requestIDHeader))
	assert.Equal(t, "upstream speech retried", line["msg"])
}

func TestServer_HandleSpeech_StreamBrokenAfterAudioIsNotRetried(t *testing.T) {
	const promisedBytes = 100000
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentTypeWAV)
		w.Header().Set("Content-Length", strconv.Itoa(promisedBytes))
		writeOrFail(t, w, upstreamAudio)
	})
	logs := &logBuffer{}
	h.logTo(logs)

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm"})
	require.Equal(t, http.StatusOK, resp.StatusCode)

	_, err := io.ReadAll(resp.Body)
	assert.Error(t, err, "a broken upstream stream ended cleanly, want a transport error")
	assert.Equal(t, 1, h.upstream.callCount())
	logs.find(t, slog.LevelError, resp.Header.Get(requestIDHeader))
}

func TestServer_HandleSpeech_DeadlineDuringFirstAttemptIsNotRetried(t *testing.T) {
	h := newHarness(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}, func(cfg *Config) { cfg.RequestTimeout = shortDeadline })

	resp := h.speech(speechPath, map[string]any{"input": "hi", "response_format": "pcm"})

	assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
	assert.Equal(t, 1, h.upstream.callCount())
}

func redirectToSelf(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, r.URL.Path, http.StatusTemporaryRedirect)
}

func TestServer_HandleSpeech_UpstreamRedirectIs502WithoutFollowing(t *testing.T) {
	h := newHarness(t, redirectToSelf)

	resp := h.speech(speechPath, map[string]any{"input": "hi"})

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	assert.Contains(t, errorMessage(t, resp), "upstream returned 307")
	assert.Equal(t, 1, h.upstream.callCount())
}
