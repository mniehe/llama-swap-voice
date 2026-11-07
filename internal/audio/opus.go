package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/pion/opus"
)

const (
	opusRate          = 48000
	opusTagsMagic     = "OpusTags"
	opusHeadBytes     = 19
	opusVersionByte   = 8
	opusChannelsByte  = 9
	opusPreSkipOffset = 10
	opusGainOffset    = 16
	opusMappingByte   = 18
	opusMajorVersion  = 0xF0
	opusMappingRTP    = 0
	opusGainQ8        = 256
	decibelsPerBel    = 20
	decibelBase       = 10

	// 120 ms at 48 kHz, the longest packet Opus allows.
	opusMaxPacketFrames = 5760
)

type opusHead struct {
	channels int
	preSkip  int
	gain     float32
}

// opusStream trims the stream to what the encoder meant to play: the
// pre-skip comes off the front, and the end-of-stream page keeps only as many
// frames as its granule position advances, which cuts the padding off the
// end whatever the granule origin.
type opusStream struct {
	decoder  opus.Decoder
	head     opusHead
	pcm      []float32
	mono     []float32
	page     []float32
	granule  uint64
	headroom int
	skipping int
	ended    bool
}

func decodeOpus(data []byte, out *normaliser) error {
	reader := newOggReader(data)
	head, err := readOpusHeaders(reader)
	if err != nil {
		return err
	}

	stream, err := newOpusStream(head)
	if err != nil {
		return err
	}

	if err := out.setRate(opusRate); err != nil {
		return err
	}

	for {
		page, err := reader.nextPage()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		if err := stream.decodePage(page, out); err != nil {
			return err
		}
	}
}

func readOpusHeaders(reader *oggReader) (opusHead, error) {
	packets, err := headerPackets(reader)
	if err != nil {
		return opusHead{}, err
	}

	head, err := parseOpusHead(packets[0])
	if err != nil {
		return opusHead{}, err
	}

	if !bytes.HasPrefix(packets[1], []byte(opusTagsMagic)) {
		return opusHead{}, fmt.Errorf("%w: Opus stream lacks OpusTags", ErrCorrupt)
	}

	return head, nil
}

// OpusHead sits alone on the first page and OpusTags ends before the audio
// starts, so the first two packets are always the headers.
func headerPackets(reader *oggReader) ([][]byte, error) {
	const headerCount = 2

	var packets [][]byte
	for len(packets) < headerCount {
		page, err := reader.nextPage()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: Opus stream ends inside its headers", ErrCorrupt)
		}

		if err != nil {
			return nil, err
		}
		packets = append(packets, page.packets...)
	}

	if len(packets) > headerCount {
		return nil, fmt.Errorf("%w: Opus audio shares a page with its headers", ErrCorrupt)
	}

	return packets, nil
}

func parseOpusHead(packet []byte) (opusHead, error) {
	if len(packet) < opusHeadBytes || !bytes.HasPrefix(packet, []byte(opusMagic)) {
		return opusHead{}, fmt.Errorf("%w: OpusHead is truncated", ErrCorrupt)
	}

	if packet[opusVersionByte]&opusMajorVersion != 0 {
		return opusHead{}, fmt.Errorf("%w: OpusHead version %d", ErrUnsupported, packet[opusVersionByte])
	}

	channels := int(packet[opusChannelsByte])
	if packet[opusMappingByte] != opusMappingRTP || channels < 1 || channels > maxChannels {
		return opusHead{}, fmt.Errorf("%w: Opus with %d channels in mapping family %d", ErrUnsupported, channels, packet[opusMappingByte])
	}

	gainQ8 := (int32(packet[opusGainOffset])<<16 | int32(packet[opusGainOffset+1])<<24) >> 16
	gain := math.Pow(decibelBase, float64(gainQ8)/opusGainQ8/decibelsPerBel)

	return opusHead{
		channels: channels,
		preSkip:  int(binary.LittleEndian.Uint16(packet[opusPreSkipOffset:])),
		gain:     float32(gain),
	}, nil
}

func newOpusStream(head opusHead) (*opusStream, error) {
	decoder, err := opus.NewDecoderWithOutput(opusRate, head.channels)
	if err != nil {
		return nil, corrupt(err)
	}

	return &opusStream{
		decoder:  decoder,
		head:     head,
		pcm:      make([]float32, opusMaxPacketFrames*head.channels),
		headroom: head.preSkip + opusMaxPacketFrames,
		skipping: head.preSkip,
	}, nil
}

// decodePage decodes and emits one page. Granule positions are not trusted
// to bound the work: every decoded frame is charged against the limit before
// any trimming, and positions that fail to advance are rejected.
func (s *opusStream) decodePage(page oggPackets, out *normaliser) error {
	if s.ended {
		return fmt.Errorf("%w: Ogg page after the end of the stream", ErrCorrupt)
	}
	s.ended = page.last

	if len(page.packets) == 0 {
		return nil
	}

	if page.granule <= s.granule {
		return fmt.Errorf("%w: Ogg granule position %d does not advance past %d", ErrCorrupt, page.granule, s.granule)
	}

	if err := s.decodePackets(page.packets, out); err != nil {
		return err
	}

	kept := uint64(len(s.page))
	if page.last {
		kept = page.granule - s.granule
	}

	if kept > uint64(len(s.page)) {
		return fmt.Errorf("%w: final granule position wants %d frames of the %d decoded", ErrCorrupt, kept, len(s.page))
	}
	s.granule = page.granule

	return s.emit(out, kept)
}

func (s *opusStream) decodePackets(packets [][]byte, out *normaliser) error {
	for _, packet := range packets {
		var frames int
		err := guarded(func() error {
			var err error
			frames, err = s.decoder.DecodeToFloat32(packet, s.pcm)
			return err
		})
		if err != nil {
			return corrupt(err)
		}

		if err := out.charge(frames, s.headroom); err != nil {
			return err
		}

		s.mono = downmix(s.mono, s.pcm[:frames*s.head.channels], s.head.channels)
		s.page = append(s.page, s.mono...)
	}

	return nil
}

func (s *opusStream) emit(out *normaliser, kept uint64) error {
	audio := s.page[:kept]
	s.page = s.page[:0]

	skipped := min(s.skipping, len(audio))
	s.skipping -= skipped
	audio = audio[skipped:]

	for index := range audio {
		audio[index] *= s.head.gain
	}

	return out.write(audio)
}
