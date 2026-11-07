package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

const (
	fmtChunkID  = "fmt "
	dataChunkID = "data"

	riffPreambleBytes   = 12
	chunkIDBytes        = 4
	chunkHeaderBytes    = 8
	minFormatBytes      = 16
	extensibleBytes     = 40
	subFormatOffset     = 24
	subFormatTagBytes   = 2
	channelsOffset      = 2
	rateOffset          = 4
	blockAlignOffset    = 12
	bitsOffset          = 14
	wavFormatFloat      = 3
	wavFormatExtensible = 0xFFFE
	bitsPerByte         = 8
	pcm16Bits           = 16
	pcm24Bits           = 24
	float32Bits         = 32
	maxChannels         = 2
	wavChunkFrames      = 4096
	wavUnknownSize      = math.MaxUint32

	pcm16Scale = 1 << 15
	pcm24Scale = 1 << 23
)

// The tail every WAVE_FORMAT_EXTENSIBLE subformat GUID shares after its
// two-byte format tag (KSDATAFORMAT_SUBTYPE_*).
var extensibleGUIDTail = []byte{0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71}

type sampleReader func(sample []byte) float32

type wavFormat struct {
	tag        int
	channels   int
	rate       int
	bits       int
	blockAlign int
}

func decodeWAV(data []byte, out *normaliser) error {
	format, samples, err := parseWAV(data)
	if err != nil {
		return err
	}

	read, err := format.reader()
	if err != nil {
		return err
	}

	if err := out.setRate(format.rate); err != nil {
		return err
	}

	if len(samples)%format.blockAlign != 0 {
		return fmt.Errorf("%w: WAV data ends inside a sample frame", ErrCorrupt)
	}

	chunkBytes := wavChunkFrames * format.blockAlign
	mono := make([]float32, 0, wavChunkFrames)
	for len(samples) > 0 {
		chunk := samples[:min(chunkBytes, len(samples))]
		samples = samples[len(chunk):]

		mono = format.toMono(mono[:0], chunk, read)
		if err := out.write(mono); err != nil {
			return err
		}
	}

	return nil
}

func parseWAV(data []byte) (wavFormat, []byte, error) {
	var format wavFormat
	haveFormat := false
	for offset := riffPreambleBytes; offset+chunkHeaderBytes <= len(data); {
		id := string(data[offset : offset+chunkIDBytes])
		declared := binary.LittleEndian.Uint32(data[offset+chunkIDBytes:])
		size := int(declared)
		body := data[offset+chunkHeaderBytes:]

		switch id {
		case fmtChunkID:
			if size > len(body) {
				return format, nil, fmt.Errorf("%w: WAV fmt chunk is truncated", ErrCorrupt)
			}

			parsed, err := parseFormat(body[:size])
			if err != nil {
				return format, nil, err
			}
			format, haveFormat = parsed, true
		case dataChunkID:
			if !haveFormat {
				return format, nil, fmt.Errorf("%w: WAV data chunk comes before its fmt chunk", ErrCorrupt)
			}

			samples, err := dataChunk(declared, body)
			return format, samples, err
		}

		offset += chunkHeaderBytes + size + size%2
	}

	return format, nil, fmt.Errorf("%w: WAV has no data chunk", ErrCorrupt)
}

// Streamed WAVs declare an unknown data size as 0xFFFFFFFF, so whatever
// follows the header is the data; any other size must be present in full.
func dataChunk(declared uint32, body []byte) ([]byte, error) {
	if declared == wavUnknownSize {
		return body, nil
	}

	size := int(declared)
	if size > len(body) {
		return nil, fmt.Errorf("%w: WAV data chunk declares %d bytes but holds %d", ErrCorrupt, size, len(body))
	}

	return body[:size], nil
}

func parseFormat(body []byte) (wavFormat, error) {
	if len(body) < minFormatBytes {
		return wavFormat{}, fmt.Errorf("%w: WAV fmt chunk is %d bytes", ErrCorrupt, len(body))
	}

	format := wavFormat{
		tag:        int(binary.LittleEndian.Uint16(body)),
		channels:   int(binary.LittleEndian.Uint16(body[channelsOffset:])),
		rate:       int(binary.LittleEndian.Uint32(body[rateOffset:])),
		blockAlign: int(binary.LittleEndian.Uint16(body[blockAlignOffset:])),
		bits:       int(binary.LittleEndian.Uint16(body[bitsOffset:])),
	}
	if format.tag != wavFormatExtensible {
		return format, nil
	}

	if len(body) < extensibleBytes {
		return wavFormat{}, fmt.Errorf("%w: WAV extensible fmt chunk is %d bytes", ErrCorrupt, len(body))
	}

	subFormat := body[subFormatOffset:extensibleBytes]
	if !bytes.Equal(subFormat[subFormatTagBytes:], extensibleGUIDTail) {
		return wavFormat{}, fmt.Errorf("%w: WAV extensible subformat", ErrUnsupported)
	}
	format.tag = int(binary.LittleEndian.Uint16(subFormat))

	return format, nil
}

func (f wavFormat) reader() (sampleReader, error) {
	if f.channels < 1 || f.channels > maxChannels || f.blockAlign != f.channels*f.bits/bitsPerByte {
		return nil, fmt.Errorf("%w: WAV with %d channels and block size %d", ErrUnsupported, f.channels, f.blockAlign)
	}

	switch {
	case f.tag == formatPCM && f.bits == pcm16Bits:
		return pcm16, nil
	case f.tag == formatPCM && f.bits == pcm24Bits:
		return pcm24, nil
	case f.tag == wavFormatFloat && f.bits == float32Bits:
		return float32LE, nil
	}

	return nil, fmt.Errorf("%w: WAV format %d with %d-bit samples", ErrUnsupported, f.tag, f.bits)
}

func (f wavFormat) toMono(dst []float32, chunk []byte, read sampleReader) []float32 {
	sampleWidth := f.bits / bitsPerByte
	for frame := 0; frame < len(chunk); frame += f.blockAlign {
		var sum float32
		for channel := range f.channels {
			sum += read(chunk[frame+channel*sampleWidth:])
		}
		dst = append(dst, sum/float32(f.channels))
	}

	return dst
}

// Shifting each byte to the top of an int32 and back sign-extends the sample
// without a narrowing conversion.
func pcm16(sample []byte) float32 {
	value := (int32(sample[0])<<16 | int32(sample[1])<<24) >> 16
	return float32(value) / pcm16Scale
}

func pcm24(sample []byte) float32 {
	value := (int32(sample[0])<<8 | int32(sample[1])<<16 | int32(sample[2])<<24) >> 8
	return float32(value) / pcm24Scale
}

func float32LE(sample []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(sample))
}
