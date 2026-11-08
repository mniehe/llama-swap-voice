package audio

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

const (
	targetRate      = 16000
	targetChannels  = 1
	targetBits      = 16
	sampleBytes     = targetBits / 8
	wavHeaderBytes  = 44
	riffHeaderBytes = 8
	fmtChunkBytes   = 16
	formatPCM       = 1
	s16Scale        = 32768
	s16Ceiling      = 32767.0 / s16Scale
	s16Floor        = -1
	minSourceRate   = 8000
	maxSourceRate   = 48000
)

// normaliser counts decoded source frames against the duration limit and
// resamples them into the WAV it is building.
type normaliser struct {
	ctx         context.Context
	maxDuration time.Duration
	maxFrames   int
	frames      int
	charged     int
	resampler   *resampler
	pcm         []int16
	wav         []byte
}

func newNormaliser(ctx context.Context, limits Limits) *normaliser {
	return &normaliser{
		ctx:         ctx,
		maxDuration: limits.MaxDuration,
		wav:         make([]byte, wavHeaderBytes),
	}
}

func (n *normaliser) setRate(rate int) error {
	if rate < minSourceRate || rate > maxSourceRate {
		return fmt.Errorf("%w: sample rate %d Hz", ErrUnsupported, rate)
	}

	n.maxFrames = int(n.maxDuration * time.Duration(rate) / time.Second)
	n.resampler = newResampler(rate, targetRate)
	return nil
}

// charge counts frames a decoder produced before any trimming, so a stream
// that trims away what it decodes still pays for decoding it. headroom covers
// what an honest stream decodes but never plays, such as Opus pre-skip.
func (n *normaliser) charge(frames, headroom int) error {
	if err := n.ctx.Err(); err != nil {
		return err
	}

	n.charged += frames
	if n.charged > n.maxFrames+headroom {
		return ErrTooLong
	}

	return nil
}

func (n *normaliser) write(mono []float32) error {
	if err := n.ctx.Err(); err != nil {
		return err
	}

	n.frames += len(mono)
	if n.frames > n.maxFrames {
		return ErrTooLong
	}

	n.pcm = n.resampler.push(n.pcm[:0], mono)
	return n.appendPCM()
}

func (n *normaliser) appendPCM() error {
	wav, err := binary.Append(n.wav, binary.LittleEndian, n.pcm)
	if err != nil {
		return err
	}

	n.wav = wav
	return nil
}

func (n *normaliser) finish() ([]byte, error) {
	if n.frames == 0 {
		return nil, fmt.Errorf("%w: upload holds no audio", ErrCorrupt)
	}

	if n.resampler != nil {
		n.pcm = n.resampler.flush(n.pcm[:0])
		if err := n.appendPCM(); err != nil {
			return nil, err
		}
	}

	size := len(n.wav) - wavHeaderBytes
	if size > math.MaxUint32-wavHeaderBytes {
		return nil, ErrTooLong
	}

	dataBytes := uint32(size)
	appendWAVHeader(n.wav[:0], targetRate, dataBytes+wavHeaderBytes-riffHeaderBytes, dataBytes)

	return n.wav, nil
}

func toS16(sample float32) int16 {
	if math.IsNaN(float64(sample)) {
		return 0
	}

	clamped := max(s16Floor, min(sample, s16Ceiling))
	return int16(math.Round(float64(clamped) * s16Scale))
}

func downmix(dst, interleaved []float32, channels int) []float32 {
	dst = dst[:0]
	for start := 0; start+channels <= len(interleaved); start += channels {
		var sum float32
		for _, sample := range interleaved[start : start+channels] {
			sum += sample
		}
		dst = append(dst, sum/float32(channels))
	}

	return dst
}
