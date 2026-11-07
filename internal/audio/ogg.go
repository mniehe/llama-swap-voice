package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	oggVersionByte    = 4
	oggFlagsByte      = 5
	oggGranuleOffset  = 6
	oggSerialOffset   = 14
	oggChecksumOffset = 22
	oggChecksumBytes  = 4
	oggVersion        = 0
	oggContinued      = 0x01
	oggEndOfStream    = 0x04
	oggLacingContinue = 255
	oggCRCPolynomial  = 0x04C11DB7
	oggCRCTopBit      = 0x80000000
	oggCRCByteShift   = 24
	oggCRCTableSize   = 256

	// A valid Opus packet holds at most 48 frames of 1275 bytes, so a packet
	// past this is a decoy built to make the reader buffer the whole upload.
	maxOggPacketBytes = 64 << 10
)

var oggCRCTable = buildOggCRCTable()

type oggPage struct {
	continued bool
	last      bool
	granule   uint64
	serial    uint32
	lacing    []byte
	body      []byte
}

// oggPackets holds the packets that end on one page, with the page's granule
// position and whether it ends the logical stream.
type oggPackets struct {
	packets [][]byte
	granule uint64
	last    bool
}

// oggReader yields the packets of a physical Ogg stream that carries a
// single logical stream, one page at a time.
type oggReader struct {
	data    []byte
	serial  uint32
	started bool
	partial []byte
}

func newOggReader(data []byte) *oggReader {
	return &oggReader{data: data}
}

// nextPage returns the packets that end on the next page, or io.EOF once no
// page is left. Chained and multiplexed streams interleave or follow on with
// another serial number; neither is decoded.
func (r *oggReader) nextPage() (oggPackets, error) {
	if len(r.data) == 0 {
		return oggPackets{}, io.EOF
	}

	page, err := r.readPage()
	if err != nil {
		return oggPackets{}, err
	}

	if !r.started {
		r.serial, r.started = page.serial, true
	}

	if page.serial != r.serial {
		return oggPackets{}, fmt.Errorf("%w: chained or multiplexed Ogg", ErrUnsupported)
	}

	packets, err := r.assemble(page)
	if err != nil {
		return oggPackets{}, err
	}

	return oggPackets{packets: packets, granule: page.granule, last: page.last}, nil
}

func (r *oggReader) readPage() (oggPage, error) {
	data := r.data
	if len(data) < oggHeaderBytes || !bytes.HasPrefix(data, []byte(oggMagic)) {
		return oggPage{}, fmt.Errorf("%w: Ogg page header is missing or truncated", ErrCorrupt)
	}

	if data[oggVersionByte] != oggVersion {
		return oggPage{}, fmt.Errorf("%w: Ogg version %d", ErrUnsupported, data[oggVersionByte])
	}

	lacingEnd := oggHeaderBytes + int(data[oggSegmentCountByte])
	if lacingEnd > len(data) {
		return oggPage{}, fmt.Errorf("%w: Ogg segment table is truncated", ErrCorrupt)
	}

	lacing := data[oggHeaderBytes:lacingEnd]
	pageEnd := lacingEnd
	for _, size := range lacing {
		pageEnd += int(size)
	}

	if pageEnd > len(data) {
		return oggPage{}, fmt.Errorf("%w: Ogg page body is truncated", ErrCorrupt)
	}

	if oggChecksum(data[:pageEnd]) != binary.LittleEndian.Uint32(data[oggChecksumOffset:]) {
		return oggPage{}, fmt.Errorf("%w: Ogg page checksum mismatch", ErrCorrupt)
	}

	r.data = data[pageEnd:]
	return oggPage{
		continued: data[oggFlagsByte]&oggContinued != 0,
		last:      data[oggFlagsByte]&oggEndOfStream != 0,
		granule:   binary.LittleEndian.Uint64(data[oggGranuleOffset:]),
		serial:    binary.LittleEndian.Uint32(data[oggSerialOffset:]),
		lacing:    lacing,
		body:      data[lacingEnd:pageEnd],
	}, nil
}

// A lacing value of 255 means the packet continues in the next segment,
// possibly on the next page; anything smaller ends it.
func (r *oggReader) assemble(page oggPage) ([][]byte, error) {
	if !page.continued {
		r.partial = nil
	}

	var packets [][]byte
	body := page.body
	for _, size := range page.lacing {
		if len(r.partial)+int(size) > maxOggPacketBytes {
			return nil, fmt.Errorf("%w: Ogg packet is larger than %d bytes", ErrCorrupt, maxOggPacketBytes)
		}

		r.partial = append(r.partial, body[:size]...)
		body = body[size:]
		if size == oggLacingContinue {
			continue
		}

		packets = append(packets, r.partial)
		r.partial = nil
	}

	return packets, nil
}

func oggChecksum(page []byte) uint32 {
	var crc uint32
	for index, value := range page {
		if index >= oggChecksumOffset && index < oggChecksumOffset+oggChecksumBytes {
			value = 0
		}
		crc = crc<<8 ^ oggCRCTable[byte(crc>>oggCRCByteShift)^value]
	}

	return crc
}

func buildOggCRCTable() [oggCRCTableSize]uint32 {
	var table [oggCRCTableSize]uint32
	for index := range table {
		crc := uint32(index) << oggCRCByteShift
		for range bitsPerByte {
			if crc&oggCRCTopBit != 0 {
				crc = crc<<1 ^ oggCRCPolynomial
				continue
			}
			crc <<= 1
		}
		table[index] = crc
	}

	return table
}
