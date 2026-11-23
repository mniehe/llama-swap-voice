package wyoming

import (
	"bufio"
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// FuzzReadEvent feeds arbitrary bytes to the event reader. Every event it
// returns must respect the limits and survive a write and a second read.
func FuzzReadEvent(f *testing.F) {
	seeds := []string{
		"{\"type\":\"describe\"}\n",
		"{\"type\":\"ping\",\"data\":{\"text\":\"hi\"}}\n",
		"{\"type\":\"x\",\"data\":{\"a\":1},\"data_length\":7}\n{\"b\":2}",
		"{\"type\":\"audio-chunk\",\"data\":{\"rate\":16000,\"width\":2,\"channels\":1},\"payload_length\":4}\n\x00\x01\x02\x03",
		"{\"type\":\"a\",\"payload_length\":1}\nz{\"type\":\"b\"}\n",
		"{\"type\":\"x\",\"data_length\":-1}\n",
		"{\"type\":\"x\",\"payload_length\":1048577}\n",
		"{\"type\":\"x\",\"data\":null,\"data_length\":4}\nnull",
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		r := bufio.NewReader(bytes.NewReader(input))
		for {
			event, err := ReadEvent(r)
			if err != nil {
				return
			}
			checkEvent(t, event)
		}
	})
}

func checkEvent(t *testing.T, event *Event) {
	require.NotEmpty(t, event.Type)
	require.LessOrEqual(t, len(event.Payload), maxPayloadBytes)

	var wire bytes.Buffer
	require.NoError(t, WriteEvent(&wire, event))

	// The writer has no limits of its own: an event merged from inline and
	// separate data, or whose type re-encodes longer, may not read back.
	header, _, _ := bytes.Cut(wire.Bytes(), []byte("\n"))
	if len(header) > maxHeaderBytes || len(event.Data) > maxDataBytes {
		return
	}

	again, err := ReadEvent(bufio.NewReader(&wire))
	require.NoError(t, err, "a written event did not read back: %q", wire.String())
	require.Equal(t, event.Type, again.Type)
	require.Equal(t, event.Payload, again.Payload)
	require.Equal(t, decoded(t, event.Data), decoded(t, again.Data))
}

func decoded(t *testing.T, data json.RawMessage) any {
	if data == nil {
		return nil
	}

	var value any
	require.NoError(t, json.Unmarshal(data, &value))

	return value
}
