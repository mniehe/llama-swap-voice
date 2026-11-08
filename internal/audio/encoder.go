package audio

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Format is a speech output encoding.
type Format string

const (
	WAV  Format = "wav"
	PCM  Format = "pcm"
	MP3  Format = "mp3"
	Opus Format = "opus"
)

const (
	outputRate        = 24000
	outputChannels    = 1
	outputSampleBytes = 2
)

// NewEncoder returns a writer that takes s16le 24 kHz mono samples and writes
// them to w in format as they arrive. Close writes the end of the stream and
// releases the encoder; it must be called even after a failed Write.
func NewEncoder(format Format, w io.Writer) (io.WriteCloser, error) {
	switch format {
	case PCM:
		return nopWriteCloser{Writer: w}, nil
	case WAV:
		return &wavEncoder{w: w}, nil
	case MP3:
		return newMP3Encoder(w), nil
	case Opus:
		encoder, err := newOpusEncoder(w)
		if err != nil {
			return nil, err
		}
		return encoder, nil
	}

	return nil, fmt.Errorf("%w: output format %q", ErrUnsupported, format)
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}

// wavEncoder sends the same streamed header llama-server does, with unknown
// sizes, because the length is not known until the end.
type wavEncoder struct {
	w       io.Writer
	started bool
}

func (e *wavEncoder) Write(p []byte) (int, error) {
	if err := e.start(); err != nil {
		return 0, err
	}

	return e.w.Write(p)
}

func (e *wavEncoder) Close() error {
	return e.start()
}

func (e *wavEncoder) start() error {
	if e.started {
		return nil
	}
	e.started = true

	_, err := e.w.Write(appendWAVHeader(nil, outputRate, wavUnknownSize, wavUnknownSize))
	return err
}

func appendWAVHeader(dst []byte, rate, riffBytes, dataBytes uint32) []byte {
	dst = append(dst, riffMagic...)
	dst = binary.LittleEndian.AppendUint32(dst, riffBytes)
	dst = append(dst, waveMagic...)
	dst = append(dst, fmtChunkID...)
	dst = binary.LittleEndian.AppendUint32(dst, fmtChunkBytes)
	dst = binary.LittleEndian.AppendUint16(dst, formatPCM)
	dst = binary.LittleEndian.AppendUint16(dst, targetChannels)
	dst = binary.LittleEndian.AppendUint32(dst, rate)
	dst = binary.LittleEndian.AppendUint32(dst, rate*targetChannels*sampleBytes)
	dst = binary.LittleEndian.AppendUint16(dst, targetChannels*sampleBytes)
	dst = binary.LittleEndian.AppendUint16(dst, targetBits)
	dst = append(dst, dataChunkID...)

	return binary.LittleEndian.AppendUint32(dst, dataBytes)
}

// framer gathers s16le bytes into frames of a fixed number of samples for a
// block encoder, carrying a sample split across writes.
type framer struct {
	pending []byte
	filled  int
	samples []int16
	emit    func(frame []int16) error
}

func newFramer(frameSamples int, emit func(frame []int16) error) *framer {
	return &framer{
		pending: make([]byte, frameSamples*outputSampleBytes),
		samples: make([]int16, frameSamples),
		emit:    emit,
	}
}

func (f *framer) write(p []byte) error {
	for len(p) > 0 {
		copied := copy(f.pending[f.filled:], p)
		f.filled += copied
		p = p[copied:]

		if f.filled < len(f.pending) {
			return nil
		}

		if err := f.flush(); err != nil {
			return err
		}
	}

	return nil
}

// finish feeds silence zero samples, then zero-pads the partial last frame.
func (f *framer) finish(silence int) error {
	if err := f.write(make([]byte, silence*outputSampleBytes)); err != nil {
		return err
	}

	if f.filled == 0 {
		return nil
	}

	clear(f.pending[f.filled:])
	return f.flush()
}

func (f *framer) flush() error {
	f.filled = 0
	if _, err := binary.Decode(f.pending, binary.LittleEndian, f.samples); err != nil {
		return err
	}

	return f.emit(f.samples)
}
