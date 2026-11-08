package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	maxFormatChunkBytes  = 256
	maxSkippedChunkBytes = 1 << 20
)

// WAVReader yields the raw s16le samples of a streamed 24 kHz mono WAV. Data
// that ends inside a sample is reported as ErrCorrupt.
type WAVReader struct {
	pcm  io.Reader
	read int64
}

// NewWAVReader consumes the WAV header and returns as soon as the data chunk
// starts, so samples can be read while the rest of the stream is in flight.
// A data size of 0xFFFFFFFF means the samples run to the end of the stream.
func NewWAVReader(r io.Reader) (*WAVReader, error) {
	var preamble [riffPreambleBytes]byte
	if _, err := io.ReadFull(r, preamble[:]); err != nil {
		return nil, headerError(err)
	}

	if string(preamble[:chunkIDBytes]) != riffMagic || string(preamble[riffHeaderBytes:]) != waveMagic {
		return nil, fmt.Errorf("%w: not a WAV stream", ErrCorrupt)
	}

	haveFormat := false
	for {
		var header [chunkHeaderBytes]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, headerError(err)
		}

		size := binary.LittleEndian.Uint32(header[chunkIDBytes:])
		switch string(header[:chunkIDBytes]) {
		case fmtChunkID:
			if err := readStreamFormat(r, size); err != nil {
				return nil, err
			}
			haveFormat = true
		case dataChunkID:
			if !haveFormat {
				return nil, fmt.Errorf("%w: WAV data chunk comes before its fmt chunk", ErrCorrupt)
			}

			return &WAVReader{pcm: dataReader(r, size)}, nil
		default:
			if err := skipChunk(r, size); err != nil {
				return nil, err
			}
		}
	}
}

func (w *WAVReader) Read(p []byte) (int, error) {
	n, err := w.pcm.Read(p)
	w.read += int64(n)
	if errors.Is(err, io.EOF) && w.read%outputSampleBytes != 0 {
		return n, fmt.Errorf("%w: WAV data ends inside a sample", ErrCorrupt)
	}

	return n, err
}

func readStreamFormat(r io.Reader, size uint32) error {
	if size > maxFormatChunkBytes {
		return fmt.Errorf("%w: WAV fmt chunk declares %d bytes", ErrCorrupt, size)
	}

	body := make([]byte, size+size%2)
	if _, err := io.ReadFull(r, body); err != nil {
		return headerError(err)
	}

	format, err := parseFormat(body[:size])
	if err != nil {
		return err
	}

	if format.tag != formatPCM || format.bits != pcm16Bits || format.channels != outputChannels ||
		format.rate != outputRate || format.blockAlign != outputSampleBytes {
		return fmt.Errorf("%w: WAV stream is format %d, %d-bit, %d channels at %d Hz; want 16-bit mono PCM at %d Hz",
			ErrUnsupported, format.tag, format.bits, format.channels, format.rate, outputRate)
	}

	return nil
}

func skipChunk(r io.Reader, size uint32) error {
	if size > maxSkippedChunkBytes {
		return fmt.Errorf("%w: WAV chunk before the data declares %d bytes", ErrCorrupt, size)
	}

	if _, err := io.CopyN(io.Discard, r, int64(size)+int64(size%2)); err != nil {
		return headerError(err)
	}

	return nil
}

func headerError(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: WAV stream ends inside its header", ErrCorrupt)
	}

	return fmt.Errorf("read WAV header: %w", err)
}

func dataReader(r io.Reader, size uint32) io.Reader {
	if size == wavUnknownSize {
		return r
	}

	return &sizedReader{r: r, left: int64(size)}
}

// sizedReader stops at the declared data size and reports a stream that
// ends before it as truncated.
type sizedReader struct {
	r    io.Reader
	left int64
}

func (s *sizedReader) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}

	if int64(len(p)) > s.left {
		p = p[:s.left]
	}

	n, err := s.r.Read(p)
	s.left -= int64(n)
	if errors.Is(err, io.EOF) && s.left > 0 {
		return n, io.ErrUnexpectedEOF
	}

	return n, err
}
