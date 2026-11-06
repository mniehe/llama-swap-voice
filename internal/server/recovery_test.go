package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestServer_Recovered_PanicIs500WithStack(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := capturingServer(buf).recovered(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/models", http.NoBody))

	assert.Equal(t, http.StatusInternalServerError, resp.Code)
	assert.Contains(t, resp.Body.String(), internalErrorText)

	line := decodeLogLine(t, buf)
	assert.Equal(t, "panic recovered", line["msg"])
	assert.Equal(t, "boom", line["panic"])
	assert.Equal(t, "/v1/models", line["path"])
	assert.Contains(t, line["stack"], "runtime/debug.Stack")
}

func TestServer_Recovered_AbortHandlerPropagates(t *testing.T) {
	buf := &bytes.Buffer{}
	handler := capturingServer(buf).recovered(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	assert.PanicsWithValue(t, http.ErrAbortHandler, func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", http.NoBody))
	})
	assert.Empty(t, buf.String(), "an aborted stream is not a crash")
}
