package audio

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	oggBeginningOfStream = 0x02
	oggSequenceOffset    = 18
	oggMaxSegments       = 255
)

// oggWriter pages the packets of one logical stream. Audio packets share a
// page until it holds maxPackets of them. A full page leaves only when the
// next packet arrives, so the end-of-stream page always carries the last
// packet and the true end granule.
type oggWriter struct {
	w          io.Writer
	serial     uint32
	sequence   uint32
	maxPackets int
	packets    int
	granule    uint64
	lacing     []byte
	body       []byte
}

func newOggWriter(w io.Writer, maxPackets int) (*oggWriter, error) {
	var serial [4]byte
	if _, err := rand.Read(serial[:]); err != nil {
		return nil, fmt.Errorf("pick Ogg serial number: %w", err)
	}

	return &oggWriter{w: w, serial: binary.LittleEndian.Uint32(serial[:]), maxPackets: maxPackets}, nil
}

// writeAlone puts a header packet on a page of its own.
func (o *oggWriter) writeAlone(packet []byte, flags byte) error {
	o.add(packet, 0)
	return o.flush(flags)
}

// writePacket adds an audio packet whose end sits at granule.
func (o *oggWriter) writePacket(packet []byte, granule uint64) error {
	if o.packets == o.maxPackets || len(o.lacing)+segmentCount(packet) > oggMaxSegments {
		if err := o.flush(0); err != nil {
			return err
		}
	}

	o.add(packet, granule)
	return nil
}

// finish writes the last page, whose granule tells the decoder where the
// real audio ends inside the final packet.
func (o *oggWriter) finish(granule uint64) error {
	o.granule = granule
	return o.flush(oggEndOfStream)
}

func segmentCount(packet []byte) int {
	return len(packet)/oggLacingContinue + 1
}

func (o *oggWriter) add(packet []byte, granule uint64) {
	for range len(packet) / oggLacingContinue {
		o.lacing = append(o.lacing, oggLacingContinue)
	}
	o.lacing = append(o.lacing, byte(len(packet)%oggLacingContinue))
	o.body = append(o.body, packet...)
	o.granule = granule
	o.packets++
}

func (o *oggWriter) flush(flags byte) error {
	segments := len(o.lacing)
	if segments > oggMaxSegments {
		return fmt.Errorf("%d segments do not fit one Ogg page", segments)
	}

	page := make([]byte, oggHeaderBytes, oggHeaderBytes+segments+len(o.body))
	copy(page, oggMagic)
	page[oggVersionByte] = oggVersion
	page[oggFlagsByte] = flags
	binary.LittleEndian.PutUint64(page[oggGranuleOffset:], o.granule)
	binary.LittleEndian.PutUint32(page[oggSerialOffset:], o.serial)
	binary.LittleEndian.PutUint32(page[oggSequenceOffset:], o.sequence)
	page[oggSegmentCountByte] = byte(segments)
	page = append(page, o.lacing...)
	page = append(page, o.body...)
	binary.LittleEndian.PutUint32(page[oggChecksumOffset:], oggChecksum(page))

	o.sequence++
	o.packets = 0
	o.lacing = o.lacing[:0]
	o.body = o.body[:0]

	_, err := o.w.Write(page)
	return err
}
