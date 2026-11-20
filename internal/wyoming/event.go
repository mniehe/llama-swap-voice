// Package wyoming serves the Wyoming protocol over TCP: describe, ping and
// transcription, each request handed to the shared pipeline.
package wyoming

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
)

// The reference reader caps a header line at 64 KiB, asyncio's readline
// limit, and bounds neither data nor payload; each limit here is ours.
const (
	maxHeaderBytes  = 8 << 10
	maxDataBytes    = 256 << 10
	maxPayloadBytes = 1 << 20
)

// protocolVersion is the Wyoming release whose framing this package follows
// (OHF-Voice/wyoming pyproject.toml:3 at 844d383). Readers ignore it.
const protocolVersion = "1.11.0"

const newline = '\n'

var (
	errTooLarge  = errors.New("wyoming frame exceeds a limit")
	errMalformed = errors.New("malformed wyoming frame")
)

// Event is one Wyoming frame: a type, an optional JSON object and an
// optional binary payload.
type Event struct {
	Type    string
	Data    json.RawMessage
	Payload []byte
}

const (
	keyType          = "type"
	keyData          = "data"
	keyDataLength    = "data_length"
	keyPayloadLength = "payload_length"
)

type header struct {
	Type          string          `json:"type"`
	Data          json.RawMessage `json:"data,omitempty"`
	DataLength    int64           `json:"data_length,omitempty"`
	PayloadLength int64           `json:"payload_length,omitempty"`
	Version       string          `json:"version,omitempty"`
}

// ReadEvent reads one event: a JSON header line, then data_length bytes of
// JSON merged over the header's inline data, then payload_length bytes.
// It returns io.EOF only when the stream ends between events.
func ReadEvent(r *bufio.Reader) (*Event, error) {
	line, err := readHeaderLine(r)
	if err != nil {
		return nil, err
	}

	head, err := parseHeader(line)
	if err != nil {
		return nil, err
	}

	data, err := readData(r, head)
	if err != nil {
		return nil, err
	}

	payload, err := readExactly(r, head.PayloadLength)
	if err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}

	return &Event{Type: head.Type, Data: data, Payload: payload}, nil
}

// parseHeader looks keys up exactly, as the reference reader does; a struct
// decode would also match "Data_Length" and read bytes the client meant as
// the next event.
func parseHeader(line []byte) (*header, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return nil, fmt.Errorf("%w: header: %w", errMalformed, err)
	}

	head := &header{Data: fields[keyData]}
	err := errors.Join(
		decodeField(fields, keyType, &head.Type),
		decodeField(fields, keyDataLength, &head.DataLength),
		decodeField(fields, keyPayloadLength, &head.PayloadLength),
	)
	if err != nil {
		return nil, err
	}

	if err := checkHeader(head); err != nil {
		return nil, err
	}

	return head, nil
}

// decodeField leaves dst unchanged when the key is absent or null.
func decodeField(fields map[string]json.RawMessage, key string, dst any) error {
	raw, found := fields[key]
	if !found {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: header %s: %w", errMalformed, key, err)
	}

	return nil
}

func checkHeader(head *header) error {
	switch {
	case head.Type == "":
		return fmt.Errorf("%w: header has no type", errMalformed)
	case head.DataLength < 0 || head.PayloadLength < 0:
		return fmt.Errorf("%w: negative length", errMalformed)
	case head.DataLength > maxDataBytes:
		return fmt.Errorf("%w: data of %d bytes is over %d", errTooLarge, head.DataLength, maxDataBytes)
	case head.PayloadLength > maxPayloadBytes:
		return fmt.Errorf("%w: payload of %d bytes is over %d", errTooLarge, head.PayloadLength, maxPayloadBytes)
	}

	return nil
}

// readHeaderLine returns the line without its newline. It never holds more
// than maxHeaderBytes plus the newline, however long the client's line is.
func readHeaderLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice(newline)
		if len(line)+len(chunk) > maxHeaderBytes+1 {
			return nil, fmt.Errorf("%w: header line is over %d bytes", errTooLarge, maxHeaderBytes)
		}
		line = append(line, chunk...)
		if err != nil && len(line) > maxHeaderBytes {
			return nil, fmt.Errorf("%w: header line is over %d bytes", errTooLarge, maxHeaderBytes)
		}

		switch {
		case err == nil:
			return line[:len(line)-1], nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) == 0:
			return nil, io.EOF
		case errors.Is(err, io.EOF):
			return nil, fmt.Errorf("header: %w", io.ErrUnexpectedEOF)
		default:
			return nil, err
		}
	}
}

func readData(r *bufio.Reader, head *header) (json.RawMessage, error) {
	inline, err := object(head.Data, nullAllowed)
	if err != nil {
		return nil, fmt.Errorf("inline data: %w", err)
	}

	if head.DataLength == 0 {
		return inline, nil
	}

	raw, err := readExactly(r, head.DataLength)
	if err != nil {
		return nil, fmt.Errorf("data: %w", err)
	}

	extra, err := object(raw, nullRefused)
	if err != nil {
		return nil, fmt.Errorf("data: %w", err)
	}

	if inline == nil {
		return extra, nil
	}

	return merge(inline, extra)
}

type nullData int

const (
	nullRefused nullData = iota
	nullAllowed
)

// object accepts a JSON object, or null as nil when allowed. The reference
// reader treats a missing or null inline data as empty, but merges separate
// data bytes with dict.update, which fails on anything but an object.
func object(raw json.RawMessage, null nullData) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if null == nullAllowed && (len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))) {
		return nil, nil
	}

	if !bytes.HasPrefix(trimmed, []byte("{")) || !json.Valid(trimmed) {
		return nil, fmt.Errorf("%w: data is not a JSON object", errMalformed)
	}

	return trimmed, nil
}

// merge lays the top-level keys of extra over inline, as the reference
// reader's dict.update does.
func merge(inline, extra json.RawMessage) (json.RawMessage, error) {
	var fields, more map[string]json.RawMessage
	if err := json.Unmarshal(inline, &fields); err != nil {
		return nil, fmt.Errorf("%w: inline data: %w", errMalformed, err)
	}
	if err := json.Unmarshal(extra, &more); err != nil {
		return nil, fmt.Errorf("%w: data: %w", errMalformed, err)
	}
	maps.Copy(fields, more)

	var merged bytes.Buffer
	encoder := json.NewEncoder(&merged)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(fields); err != nil {
		return nil, fmt.Errorf("%w: merged data: %w", errMalformed, err)
	}

	return bytes.TrimSuffix(merged.Bytes(), []byte{newline}), nil
}

func readExactly(r io.Reader, length int64) ([]byte, error) {
	if length == 0 {
		return nil, nil
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}

	return buf, nil
}

// WriteEvent writes the header line, data and payload in one Write, so a
// write deadline covers the whole frame.
func WriteEvent(w io.Writer, e *Event) error {
	head := header{
		Type:          e.Type,
		DataLength:    int64(len(e.Data)),
		PayloadLength: int64(len(e.Payload)),
		Version:       protocolVersion,
	}

	var line bytes.Buffer
	encoder := json.NewEncoder(&line)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(&head); err != nil {
		return fmt.Errorf("encoding %s header: %w", e.Type, err)
	}

	frame := make([]byte, 0, line.Len()+len(e.Data)+len(e.Payload))
	frame = append(frame, line.Bytes()...)
	frame = append(frame, e.Data...)
	frame = append(frame, e.Payload...)

	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("writing %s: %w", e.Type, err)
	}

	return nil
}
