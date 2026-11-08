package audio

import (
	"io"
	"math"

	shine "github.com/braheezy/shine-mp3/pkg/mp3"
)

const (
	// shine encodes one 576-sample granule per MPEG-2 frame at 24 kHz.
	mp3FrameSamples = 576

	// shine delays its output by this many samples and never emits the last
	// that many it was given, so they are fed as silence at the end.
	shineDelaySamples = 1057

	// shine distorts once its input passes about 26000, so -2 dB keeps even
	// full scale clean; with shine's own +1 dB gain, the output decodes about
	// 1 dB below the input.
	shineInputScale = 0.794328235
)

type mp3Encoder struct {
	shine  *shine.Encoder
	w      io.Writer
	frames *framer
	closed bool
}

func newMP3Encoder(w io.Writer) *mp3Encoder {
	e := &mp3Encoder{shine: shine.NewEncoder(outputRate, outputChannels), w: w}
	e.frames = newFramer(mp3FrameSamples, e.encode)

	return e
}

func (e *mp3Encoder) Write(p []byte) (int, error) {
	if err := e.frames.write(p); err != nil {
		return 0, err
	}

	return len(p), nil
}

func (e *mp3Encoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true

	return e.frames.finish(shineDelaySamples)
}

func (e *mp3Encoder) encode(frame []int16) error {
	for i, sample := range frame {
		frame[i] = int16(math.Round(float64(sample) * shineInputScale))
	}

	return e.shine.Write(e.w, frame)
}
