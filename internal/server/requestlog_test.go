package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func capturingServer(buf *bytes.Buffer) *Server {
	return &Server{log: slog.New(slog.NewJSONHandler(buf, nil))}
}

func decodeLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line), "log output %q", buf.String())

	return line
}

func TestServer_Logged_RecordsRequest(t *testing.T) {
	buf := &bytes.Buffer{}
	s := capturingServer(buf)
	handler := s.logged(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := record(r)
		rec.voice = "alice"
		rec.format = "mp3"
		rec.upstreamStatus = http.StatusOK
		w.WriteHeader(http.StatusAccepted)
		writeOrFail(t, w, "audio")
	}))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodPost, speechPath, http.NoBody))

	id := resp.Header().Get(requestIDHeader)
	require.NotEmpty(t, id)

	line := decodeLogLine(t, buf)
	assert.Equal(t, "request", line["msg"])
	assert.Equal(t, id, line["request_id"])
	assert.Equal(t, http.MethodPost, line["method"])
	assert.Equal(t, speechPath, line["path"])
	assert.InDelta(t, http.StatusAccepted, line["status"], 0)
	assert.Equal(t, "alice", line["voice"])
	assert.Equal(t, "mp3", line["format"])
	assert.InDelta(t, http.StatusOK, line["upstream_status"], 0)
	assert.GreaterOrEqual(t, line["ttfb_ms"], float64(0))
	assert.GreaterOrEqual(t, line["duration_ms"], float64(0))
}

func TestServer_Logged_NoBodyHasNegativeTTFB(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := capturingServer(buf).logged(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/health", http.NoBody))

	line := decodeLogLine(t, buf)
	assert.InDelta(t, http.StatusNoContent, line["status"], 0)
	assert.InDelta(t, -1, line["ttfb_ms"], 0)
}

func TestRecord_NoRecordInContext_ReturnsEmpty(t *testing.T) {
	rec := record(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))

	assert.Equal(t, &requestRecord{}, rec)
}
