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
	opusVBR        = 1

	// libopus's own recommendation for an output buffer that any single
	// packet fits in.
	opusMaxPacketBytes = 4000
)

// Tests compare these to prove every encoder handle is freed.
var (
	opusCreated   atomic.Int64
	opusDestroyed atomic.Int64
)

// libopus owns one C encoder for 20 ms mono frames at 24 kHz. The pcm and
// packet buffers are C-typed so samples and bytes cross without unsafe.
type libopus struct {
	encoder   *C.OpusEncoder
	lookahead int
	pcm       []C.opus_int16
	packet    []C.uchar
	out       []byte
}

func newLibopus() (*libopus, error) {
	var status C.int
	encoder := C.opus_encoder_create(outputRate, outputChannels, C.OPUS_APPLICATION_AUDIO, &status)
	if status != C.OPUS_OK {
		return nil, opusError("create encoder", status)
	}
	opusCreated.Add(1)

	codec := &libopus{
		encoder: encoder,
		pcm:     make([]C.opus_int16, opusFrameSamples),
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
		{C.OPUS_SET_BITRATE_REQUEST, opusBitrate},
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
