package audio

/*
#cgo pkg-config: opus
#include <opus.h>

static int lsv_opus_set(OpusEncoder *encoder, int request, opus_int32 value) {
	return opus_encoder_ctl(encoder, request, value);
}

static int lsv_opus_get(OpusEncoder *encoder, int request, opus_int32 *value) {
	return opus_encoder_ctl(encoder, request, value);
}
*/
import "C"

import (
	"fmt"
	"sync/atomic"
)

const (
	opusBitrate    = 24000
	opusComplexity = 10
	// recordingBitrate keeps wideband speech near-transparent for later
	// analysis while a week of recordings stays small.
	recordingBitrate = 32000
	opusVBR          = 1

	// libopus's own recommendation for an output buffer that any single
	// packet fits in.
	opusMaxPacketBytes = 4000
)

// Tests compare these to prove every encoder handle is freed.
var (
	opusCreated   atomic.Int64
	opusDestroyed atomic.Int64
)

// libopus owns one C encoder for 20 ms mono frames at one of Opus's native
// rates. The pcm and packet buffers are C-typed so samples and bytes cross
// without unsafe.
type libopus struct {
	encoder   *C.OpusEncoder
	bitrate   uint32
	lookahead int
	pcm       []C.opus_int16
	packet    []C.uchar
	out       []byte
}

func newLibopus(rate, bitrate uint32) (*libopus, error) {
	var status C.int
	encoder := C.opus_encoder_create(C.opus_int32(rate), outputChannels, C.OPUS_APPLICATION_AUDIO, &status)
	if status != C.OPUS_OK {
		return nil, opusError("create encoder", status)
	}
	opusCreated.Add(1)

	codec := &libopus{
		encoder: encoder,
		bitrate: bitrate,
		pcm:     make([]C.opus_int16, rate/opusFramesPerSecond),
		packet:  make([]C.uchar, opusMaxPacketBytes),
		out:     make([]byte, opusMaxPacketBytes),
	}
	err := codec.configure()
	if err != nil {
		codec.destroy()
		return nil, err
	}

	return codec, nil
}

func (c *libopus) configure() error {
	settings := []struct {
		request C.int
		value   C.opus_int32
	}{
		{C.OPUS_SET_BITRATE_REQUEST, C.opus_int32(c.bitrate)},
		{C.OPUS_SET_COMPLEXITY_REQUEST, opusComplexity},
		{C.OPUS_SET_VBR_REQUEST, opusVBR},
	}
	for _, setting := range settings {
		if status := C.lsv_opus_set(c.encoder, setting.request, setting.value); status != C.OPUS_OK {
			return opusError("configure encoder", status)
		}
	}

	var lookahead C.opus_int32
	if status := C.lsv_opus_get(c.encoder, C.OPUS_GET_LOOKAHEAD_REQUEST, &lookahead); status != C.OPUS_OK {
		return opusError("read lookahead", status)
	}
	c.lookahead = int(lookahead)

	return nil
}

// configuredBitrate reads back the target bitrate libopus is encoding at.
func (c *libopus) configuredBitrate() (int, error) {
	var bitrate C.opus_int32
	if status := C.lsv_opus_get(c.encoder, C.OPUS_GET_BITRATE_REQUEST, &bitrate); status != C.OPUS_OK {
		return 0, opusError("read bitrate", status)
	}

	return int(bitrate), nil
}

// encode returns a packet that stays valid until the next call.
func (c *libopus) encode(frame []int16) ([]byte, error) {
	for i, sample := range frame {
		c.pcm[i] = C.opus_int16(sample)
	}

	size := C.opus_encode(c.encoder, &c.pcm[0], C.int(len(frame)), &c.packet[0], C.opus_int32(len(c.packet)))
	if size < 0 {
		return nil, opusError("encode", size)
	}

	packet := c.out[:size]
	for i := range packet {
		packet[i] = byte(c.packet[i])
	}

	return packet, nil
}

func (c *libopus) destroy() {
	if c.encoder == nil {
		return
	}

	C.opus_encoder_destroy(c.encoder)
	c.encoder = nil
	opusDestroyed.Add(1)
}

func opusError(action string, status C.int) error {
	return fmt.Errorf("libopus: %s: %s", action, C.GoString(C.opus_strerror(status)))
}
