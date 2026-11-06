package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getJSON(t *testing.T, h *harness, path string, into any) {
	t.Helper()

	resp := h.get(path)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(into), "GET %s", path)
}

func TestServer_HandleModels_ListsBothModels(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	for _, path := range []string{"/v1/models", "/models"} {
		var list modelList
		getJSON(t, h, path, &list)

		ids := make([]string, 0, len(list.Data))
		for _, m := range list.Data {
			ids = append(ids, m.ID)
		}

		assert.Equal(t, []string{ttsModel, asrModel}, ids, path)
		assert.Equal(t, listObject, list.Object, path)
	}
}

func TestServer_HandleVoices_ListsConfiguredNames(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	for _, path := range []string{"/v1/audio/voices", "/audio/voices"} {
		var body map[string][]string
		getJSON(t, h, path, &body)

		assert.Equal(t, []string{"alice", "bm_fable", "narrator"}, body["voices"], path)
	}
}

func TestServer_HandleHealth_ReportsOK(t *testing.T) {
	h := newHarness(t, serveWAV(t))

	var body map[string]string
	getJSON(t, h, "/health", &body)

	assert.Equal(t, healthOK, body["status"])
}
