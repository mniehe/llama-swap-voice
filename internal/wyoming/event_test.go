package wyoming

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// padded returns a JSON object of exactly size bytes with the given type,
// filled out by a "pad" string.
func padded(t *testing.T, eventType string, size int) string {
	t.Helper()

	base := fmt.Sprintf(`{"type":%q,"pad":""}`, eventType)
	if eventType == "" {
		base = `{"pad":""}`
	}
	require.GreaterOrEqual(t, size, len(base))

	return strings.Replace(base, `"pad":""`, `"pad":"`+strings.Repeat("x", size-len(base))+`"`, 1)
}

func readOne(t *testing.T, frame string) (*Event, error) {
	t.Helper()

	return ReadEvent(bufio.NewReader(strings.NewReader(frame)))
}

func requireJSONEqual(t *testing.T, want string, got json.RawMessage) {
	t.Helper()

	require.NotNil(t, got)
	assert.JSONEq(t, want, string(got))
}

func TestReadEvent_WrittenEvent_RoundTrips(t *testing.T) {
	tests := []struct {
		name  string
		event Event
	}{
		{name: "type only", event: Event{Type: "describe"}},
		{name: "data", event: Event{Type: "transcript", Data: json.RawMessage(`{"text":"héllo"}`)}},
		{name: "payload", event: Event{Type: "audio-chunk", Payload: []byte{0, 1, 2, 0xff}}},
		{name: "data and payload", event: Event{Type: "audio-chunk", Data: json.RawMessage(`{"rate":16000,"width":2,"channels":1}`), Payload: []byte("pcm")}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var wire bytes.Buffer
			require.NoError(t, WriteEvent(&wire, &tc.event))

			got, err := ReadEvent(bufio.NewReader(&wire))
			require.NoError(t, err)

			assert.Equal(t, tc.event.Type, got.Type)
			assert.Equal(t, tc.event.Payload, got.Payload)
			if tc.event.Data == nil {
				assert.Nil(t, got.Data)
			} else {
				requireJSONEqual(t, string(tc.event.Data), got.Data)
			}
			assert.Zero(t, wire.Len(), "bytes left after the event")
		})
	}
}

type countingWriter struct {
	bytes.Buffer
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++
	return c.Buffer.Write(p)
}

func TestWriteEvent_WritesHeaderNewlineDataPayloadInOneWrite(t *testing.T) {
	out := &countingWriter{}
	event := Event{Type: "audio-chunk", Data: json.RawMessage(`{"rate":16000}`), Payload: []byte("pcm")}

	require.NoError(t, WriteEvent(out, &event))

	assert.Equal(t, 1, out.writes)
	header, rest, found := strings.Cut(out.String(), "\n")
	require.True(t, found)
	assert.JSONEq(t, `{"type":"audio-chunk","version":"`+protocolVersion+`","data_length":14,"payload_length":3}`, header)
	assert.Equal(t, `{"rate":16000}pcm`, rest)
}

var errWriteFailed = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errWriteFailed
}

func TestWriteEvent_WriteError_IsReturned(t *testing.T) {
	err := WriteEvent(failingWriter{}, &Event{Type: "describe"})

	assert.ErrorIs(t, err, errWriteFailed)
}

func TestWriteEvent_ZeroLengthsAreOmitted(t *testing.T) {
	var out bytes.Buffer

	require.NoError(t, WriteEvent(&out, &Event{Type: "describe"}))

	assert.JSONEq(t, `{"type":"describe","version":"`+protocolVersion+`"}`, strings.TrimSuffix(out.String(), "\n"))
	assert.True(t, strings.HasSuffix(out.String(), "}\n"))
}

func TestReadEvent_DataLengthMergesOverInlineData(t *testing.T) {
	extra := `{"b":3,"c":4}`
	frame := fmt.Sprintf(`{"type":"x","data":{"a":1,"b":2},"data_length":%d}`+"\n%s", len(extra), extra)

	got, err := readOne(t, frame)

	require.NoError(t, err)
	requireJSONEqual(t, `{"a":1,"b":3,"c":4}`, got.Data)
}

func TestReadEvent_InlineDataAlone(t *testing.T) {
	got, err := readOne(t, `{"type":"ping","data":{"text":"hi"},"version":"1.5.2","extra":true}`+"\n")

	require.NoError(t, err)
	assert.Equal(t, "ping", got.Type)
	requireJSONEqual(t, `{"text":"hi"}`, got.Data)
	assert.Nil(t, got.Payload)
}

func TestReadEvent_NullLengthsAndDataAreAbsent(t *testing.T) {
	got, err := readOne(t, `{"type":"ping","data":null,"data_length":null,"payload_length":null}`+"\n")

	require.NoError(t, err)
	assert.Nil(t, got.Data)
	assert.Nil(t, got.Payload)
}

// The reference reader looks keys up exactly, so a differently cased length
// key is an unknown key there and must not consume bytes here.
func TestReadEvent_KeysMatchExactly(t *testing.T) {
	got, err := readOne(t, `{"type":"x","Data":{"a":1},"DATA_LENGTH":5,"Payload_Length":5}`+"\n")

	require.NoError(t, err)
	assert.Nil(t, got.Data)
	assert.Nil(t, got.Payload)
}

func TestReadEvent_ConsecutiveEventsShareTheReader(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{\"type\":\"a\",\"payload_length\":2}\nxy{\"type\":\"b\"}\n"))

	first, err := ReadEvent(r)
	require.NoError(t, err)
	second, err := ReadEvent(r)
	require.NoError(t, err)
	_, err = ReadEvent(r)

	assert.Equal(t, "a", first.Type)
	assert.Equal(t, []byte("xy"), first.Payload)
	assert.Equal(t, "b", second.Type)
	assert.ErrorIs(t, err, io.EOF)
}

func TestReadEvent_HeaderLine_AtLimitIsRead(t *testing.T) {
	got, err := readOne(t, padded(t, "ping", maxHeaderBytes)+"\n")

	require.NoError(t, err)
	assert.Equal(t, "ping", got.Type)
}

func TestReadEvent_HeaderLine_OneByteOverIsRejected(t *testing.T) {
	_, err := readOne(t, padded(t, "ping", maxHeaderBytes+1)+"\n")

	assert.ErrorIs(t, err, errTooLarge)
}

func TestReadEvent_HeaderLine_OverLimitWithoutNewlineIsRejected(t *testing.T) {
	_, err := readOne(t, strings.Repeat(" ", maxHeaderBytes+1))

	assert.ErrorIs(t, err, errTooLarge)
}

func TestReadEvent_Data_AtLimitIsRead(t *testing.T) {
	data := padded(t, "", maxDataBytes)

	got, err := readOne(t, fmt.Sprintf(`{"type":"ping","data_length":%d}`+"\n%s", len(data), data))

	require.NoError(t, err)
	requireJSONEqual(t, data, got.Data)
}

func TestReadEvent_Data_OneByteOverIsRejectedBeforeReading(t *testing.T) {
	_, err := readOne(t, fmt.Sprintf(`{"type":"ping","data_length":%d}`+"\n", maxDataBytes+1))

	assert.ErrorIs(t, err, errTooLarge)
}

func TestReadEvent_Payload_AtLimitIsRead(t *testing.T) {
	payload := strings.Repeat("p", maxPayloadBytes)

	got, err := readOne(t, fmt.Sprintf(`{"type":"audio-chunk","payload_length":%d}`+"\n%s", maxPayloadBytes, payload))

	require.NoError(t, err)
	assert.Len(t, got.Payload, maxPayloadBytes)
}

func TestReadEvent_Payload_OneByteOverIsRejectedBeforeReading(t *testing.T) {
	_, err := readOne(t, fmt.Sprintf(`{"type":"audio-chunk","payload_length":%d}`+"\n", maxPayloadBytes+1))

	assert.ErrorIs(t, err, errTooLarge)
}

func TestReadEvent_MalformedFrames(t *testing.T) {
	tests := []struct {
		name  string
		frame string
	}{
		{name: "not JSON", frame: "hello\n"},
		{name: "blank line", frame: "\n"},
		{name: "header is an array", frame: "[1]\n"},
		{name: "missing type", frame: `{"data":{}}` + "\n"},
		{name: "empty type", frame: `{"type":""}` + "\n"},
		{name: "type is a number", frame: `{"type":1}` + "\n"},
		{name: "negative data length", frame: `{"type":"x","data_length":-1}` + "\n"},
		{name: "negative payload length", frame: `{"type":"x","payload_length":-1}` + "\n"},
		{name: "fractional payload length", frame: `{"type":"x","payload_length":1.5}` + "\n" + "ab"},
		{name: "inline data is a string", frame: `{"type":"x","data":"text"}` + "\n"},
		{name: "data bytes are an array", frame: `{"type":"x","data_length":3}` + "\n[1]"},
		{name: "data bytes are null", frame: `{"type":"x","data_length":4}` + "\nnull"},
		{name: "data bytes are not JSON", frame: `{"type":"x","data_length":3}` + "\nabc"},
		{name: "data bytes are blank", frame: `{"type":"x","data_length":1}` + "\n "},
		{name: "type key in another case", frame: `{"Type":"x"}` + "\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readOne(t, tc.frame)

			assert.ErrorIs(t, err, errMalformed)
			assert.Nil(t, got)
		})
	}
}

func TestReadEvent_EndOfStream(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  error
	}{
		{name: "before an event", frame: "", want: io.EOF},
		{name: "inside the header", frame: `{"type":"ping"`, want: io.ErrUnexpectedEOF},
		{name: "inside the data", frame: `{"type":"x","data_length":10}` + "\n{}", want: io.ErrUnexpectedEOF},
		{name: "inside the payload", frame: `{"type":"x","payload_length":10}` + "\nabc", want: io.ErrUnexpectedEOF},
		{name: "before the payload", frame: `{"type":"x","payload_length":3}` + "\n", want: io.ErrUnexpectedEOF},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readOne(t, tc.frame)

			assert.ErrorIs(t, err, tc.want)
		})
	}
}
