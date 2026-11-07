package audio

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3"
)

const (
	mp3Channels   = 2
	mp3FrameBytes = mp3Channels * sampleBytes
	mp3ChunkBytes = wavChunkFrames * mp3FrameBytes

	id3HeaderBytes = 10
	id3FlagsByte   = 5
	id3SizeOffset  = 6
	id3FooterFlag  = 0x10
	id3SizeBits    = 7
	id3SizeLimit   = 1 << id3SizeBits
)

// go-mp3 always yields 16-bit stereo, duplicating mono sources.
var mp3Layout = wavFormat{tag: formatPCM, channels: mp3Channels, bits: pcm16Bits, blockAlign: mp3FrameBytes}

func decodeMP3(data []byte, out *normaliser) error {
	if err := checkMP3Start(data); err != nil {
		return err
	}

	// Hiding Seek stops go-mp3 from scanning every frame up front just to
	// report a length nothing here uses.
	source := io.MultiReader(bytes.NewReader(data))

	var decoder *mp3.Decoder
	err := guarded(func() error {
		var err error
		decoder, err = mp3.NewDecoder(source)
		return err
	})
	if err != nil {
		return corrupt(err)
	}

	if err := out.setRate(decoder.SampleRate()); err != nil {
		return err
	}

	chunk := make([]byte, mp3ChunkBytes)
	mono := make([]float32, 0, wavChunkFrames)
	for {
		filled, err := readMP3(decoder, chunk)
		mono = mp3Layout.toMono(mono[:0], chunk[:filled-filled%mp3FrameBytes], pcm16)
		if writeErr := out.write(mono); writeErr != nil {
			return writeErr
		}

		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}
	}
}

func checkMP3Start(data []byte) error {
	audioStart, err := id3End(data)
	if err != nil {
		return err
	}

	if isMPEG25Frame(data[audioStart:]) {
		return fmt.Errorf("%w: MPEG-2.5 audio", ErrUnsupported)
	}

	return nil
}

// go-mp3 allocates whatever size a leading ID3v2 tag claims, without masking
// the syncsafe size bytes or checking the upload holds that much, so the tag
// must be shown to fit before go-mp3 sees it.
func id3End(data []byte) (int, error) {
	if !bytes.HasPrefix(data, []byte(id3Magic)) {
		return 0, nil
	}

	if len(data) < id3HeaderBytes {
		return 0, fmt.Errorf("%w: ID3 tag header is truncated", ErrCorrupt)
	}

	size := 0
	for _, value := range data[id3SizeOffset:id3HeaderBytes] {
		if value >= id3SizeLimit {
			return 0, fmt.Errorf("%w: ID3 tag size is not syncsafe", ErrCorrupt)
		}
		size = size<<id3SizeBits | int(value)
	}

	end := id3HeaderBytes + size
	if data[id3FlagsByte]&id3FooterFlag != 0 {
		end += id3HeaderBytes
	}

	if end > len(data) {
		return 0, fmt.Errorf("%w: ID3 tag claims %d bytes of a %d-byte upload", ErrCorrupt, end, len(data))
	}

	return end, nil
}

func readMP3(decoder *mp3.Decoder, chunk []byte) (int, error) {
	var filled int
	err := guarded(func() error {
		var err error
		filled, err = io.ReadFull(decoder, chunk)
		return err
	})

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return filled, io.EOF
	}

	if err != nil {
		return filled, corrupt(err)
	}

	return filled, nil
}
