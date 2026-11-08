package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	opusFramesPerSecond = 50
	opusFrameSamples    = outputRate / opusFramesPerSecond
	opusPacketGranules  = opusRate / opusFramesPerSecond
	opusGranuleScale    = opusRate / outputRate
	opusPagePackets     = 10
	opusVendor          = "llama-swap-voice"
	opusHeadVersion     = 1
	opusRateOffset      = 12
)

// opusEncoder writes Ogg Opus. libopus encodes at 24 kHz, but the Ogg Opus
// timeline always counts 48 kHz samples, so pre-skip and granule positions
// are scaled up.
type opusEncoder struct {
	codec   *libopus
	ogg     *oggWriter
	frames  *framer
	preSkip uint16
	bytes   uint64
	packets uint64
	closed  bool
}

func newOpusEncoder(w io.Writer) (*opusEncoder, error) {
	codec, err := newLibopus()
	if err != nil {
		return nil, err
	}

	handedOver := false
	defer func() {
		if !handedOver {
			codec.destroy()
		}
	}()

	ogg, err := newOggWriter(w, opusPagePackets)
	if err != nil {
		return nil, err
	}

	preSkip := codec.lookahead * opusGranuleScale
	if preSkip < 0 || preSkip > math.MaxUint16 {
		return nil, fmt.Errorf("libopus lookahead of %d samples does not fit an Opus pre-skip", codec.lookahead)
	}

	e := &opusEncoder{codec: codec, ogg: ogg, preSkip: uint16(preSkip)}
	e.frames = newFramer(opusFrameSamples, e.encode)

	if err := e.writeHeaders(); err != nil {
		return nil, err
	}

	handedOver = true
	return e, nil
}

func (e *opusEncoder) writeHeaders() error {
	head := make([]byte, opusHeadBytes)
	copy(head, opusMagic)
	head[opusVersionByte] = opusHeadVersion
	head[opusChannelsByte] = outputChannels
	binary.LittleEndian.PutUint16(head[opusPreSkipOffset:], e.preSkip)
	binary.LittleEndian.PutUint32(head[opusRateOffset:], outputRate)
	head[opusMappingByte] = opusMappingRTP

	tags := []byte(opusTagsMagic)
	tags = binary.LittleEndian.AppendUint32(tags, uint32(len(opusVendor)))
	tags = append(tags, opusVendor...)
	tags = binary.LittleEndian.AppendUint32(tags, 0)

	if err := e.ogg.writeAlone(head, oggBeginningOfStream); err != nil {
		return err
	}

	return e.ogg.writeAlone(tags, 0)
}

func (e *opusEncoder) Write(p []byte) (int, error) {
	if e.closed {
		return 0, errors.New("write to a closed Opus encoder")
	}

	e.bytes += uint64(len(p))
	if err := e.frames.write(p); err != nil {
		return 0, err
	}

	return len(p), nil
}

// Close encodes the lookahead's worth of silence the encoder still holds
// back, then ends the stream at the true length of the input.
func (e *opusEncoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	defer e.codec.destroy()

	if err := e.frames.finish(e.codec.lookahead); err != nil {
		return err
	}

	samples := e.bytes / outputSampleBytes
	return e.ogg.finish(uint64(e.preSkip) + samples*opusGranuleScale)
}

func (e *opusEncoder) encode(frame []int16) error {
	packet, err := e.codec.encode(frame)
	if err != nil {
		return err
	}

	e.packets++
	return e.ogg.writePacket(packet, e.packets*opusPacketGranules)
}
