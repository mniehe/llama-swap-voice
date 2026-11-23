package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/pipeline"
)

const (
	requestLogMessage = "request"
	retryLogMessage   = "upstream speech retried"
)

func TestServer_Failure_MapsEachKind(t *testing.T) {
	tests := map[string]struct {
		err     error
		status  int
		message string
		closes  bool
	}{
		"invalid":       {&pipeline.Error{Kind: pipeline.KindInvalid, Message: "input is required"}, http.StatusBadRequest, "input is required", false},
		"busy":          {&pipeline.Error{Kind: pipeline.KindBusy, Message: errBusy.Error()}, http.StatusServiceUnavailable, errBusy.Error(), true},
		"timeout":       {&pipeline.Error{Kind: pipeline.KindTimeout, Message: "context deadline exceeded"}, http.StatusGatewayTimeout, "context deadline exceeded", false},
		"upstream":      {&pipeline.Error{Kind: pipeline.KindUpstream, Message: "upstream sent no audio"}, http.StatusBadGateway, "upstream sent no audio", false},
		"shutting down": {errShuttingDown, http.StatusServiceUnavailable, errShuttingDown.Error(), true},
		"internal":      {&pipeline.Error{Kind: pipeline.KindInternal, Message: internalErrorText}, http.StatusInternalServerError, internalErrorText, false},
		"no kind":       {&pipeline.Error{Message: "kind left unset"}, http.StatusInternalServerError, "kind left unset", false},
		"not pipeline":  {errors.New("bare error"), http.StatusInternalServerError, internalErrorText, false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := &Server{log: discardLogger()}
			resp := httptest.NewRecorder()

			s.failure(resp, httptest.NewRequestWithContext(t.Context(), http.MethodPost, speechPath, http.NoBody), tt.err)

			result := resp.Result()
			defer closeQuietly(t, result.Body)
			assert.Equal(t, tt.status, result.StatusCode)
			assert.Equal(t, tt.message, errorMessage(t, result))
			assert.Equal(t, tt.closes, result.Header.Get("Connection") == "close", "Connection: close")
		})
	}
}

func TestServer_HandleTranscription_LogsUpstreamFailureStatus(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model loading", http.StatusServiceUnavailable)
	})
	logs := &logBuffer{}
	h.logTo(logs)

	resp := h.transcribe(nil, []byte(streamedWAV))

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(readBody(t, resp)))
	line := logs.message(t, requestLogMessage, resp.Header.Get(requestIDHeader))
	require.NotNil(t, line, "no request log line")
	assert.InDelta(t, http.StatusServiceUnavailable, line["upstream_status"], 0)
}

func TestServer_HandleSpeech_LogsRetryOfFailedReply(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "model swapping", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", contentTypeWAV)
		writeOrFail(t, w, upstreamHeader)
	})
	logs := &logBuffer{}
	h.logTo(logs)

	resp := h.speech(speechPath, map[string]any{"input": "hello", "response_format": "wav"})

	require.Equal(t, http.StatusBadGateway, resp.StatusCode, string(readBody(t, resp)))
	id := resp.Header.Get(requestIDHeader)
	assert.NotNil(t, logs.message(t, retryLogMessage, id), "no retry log line")
	line := logs.message(t, requestLogMessage, id)
	require.NotNil(t, line, "no request log line")
	assert.InDelta(t, http.StatusOK, line["upstream_status"], 0)
}
