package upstream

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_URLWithoutHost_ReturnsError(t *testing.T) {
	_, err := New("localhost:8080", "tts", "asr", nil)

	require.Error(t, err)
}

func TestNew_URLWithHost_Succeeds(t *testing.T) {
	client, err := New("http://127.0.0.1:8080", "tts", "asr", nil)

	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:8080/upstream/tts/tts", client.base.JoinPath(routePrefix, "tts", ttsPath).String())
}

func TestDecodeTranscript_TextField_ReturnsText(t *testing.T) {
	text, err := decodeTranscript(strings.NewReader(`{"type":"transcript.text.done","text":"hello"}`))

	require.NoError(t, err)
	assert.Equal(t, "hello", text)
}

func TestDecodeTranscript_MissingText_ReturnsError(t *testing.T) {
	_, err := decodeTranscript(strings.NewReader(`{"type":"error"}`))

	require.Error(t, err)
	assert.Contains(t, err.Error(), `"error"`)
}

func TestDecodeTranscript_NotJSON_ReturnsError(t *testing.T) {
	_, err := decodeTranscript(strings.NewReader(`upstream exploded`))

	require.Error(t, err)
}

func TestStatusError_Error_IncludesCodeAndMessage(t *testing.T) {
	err := &StatusError{Code: 503, Message: "loading model"}

	assert.Equal(t, "upstream returned 503: loading model", err.Error())
}
