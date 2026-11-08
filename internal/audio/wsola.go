package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// MinSpeed and MaxSpeed bound the speeds NewStretcher accepts.
const (
	MinSpeed = 0.25
	MaxSpeed = 4.0
)

const (
	millisPerSecond     = 1000
	stretchHopMillis    = 10
	stretchSearchMillis = 5
	stretchHop          = outputRate * stretchHopMillis / millisPerSecond
	stretchWindow       = 2 * stretchHop
	stretchSearch       = outputRate * stretchSearchMillis / millisPerSecond
	searchStride        = 2
	correlationStride   = 4
	correlationEpsilon  = 1e-15
	stretchReadBytes    = 4096
	unstretchedSpeed    = 1
)

// NewStretcher time-stretches s16le 24 kHz mono samples by speed, keeping
// the pitch, with WSOLA: 20 ms windows placed every 10 ms of output, each
// taken from within 5 ms of its nominal input position where it best
// continues the previous one. Output starts after a few tens of milliseconds
// of input rather than at the end. At slow speeds it can stop up to 50 ms
// short of input/speed, and input shorter than one window is not stretched.
// Speed 1 returns r unchanged.
func NewStretcher(r io.Reader, speed float64) (io.Reader, error) {
	if math.IsNaN(speed) || speed < MinSpeed || speed > MaxSpeed {
		return nil, fmt.Errorf("speed must be between %g and %g, got %g", MinSpeed, MaxSpeed, speed)
	}

	if speed == unstretchedSpeed {
		return r, nil
	}

	return &stretcher{
		src:   r,
		speed: speed,
		read:  make([]byte, stretchReadBytes),
		raw:   make([]byte, 0, stretchReadBytes+outputSampleBytes),
		tail:  make([]float32, 0, stretchHop),
	}, nil
}

// stretcher holds the input from the earliest sample a later window can
// still use, and the second half of the last window, which the next one
// crossfades with. Output index out is always the number of samples emitted.
type stretcher struct {
	src     io.Reader
	speed   float64
	read    []byte
	raw     []byte
	decoded []int16

	in     []float32
	base   int
	out    int
	tail   []float32
	segEnd int
	primed bool

	emitted []int16
	pending []byte
	sent    int
	err     error
}

func (s *stretcher) Read(p []byte) (int, error) {
	for s.sent == len(s.pending) {
		if s.err != nil {
			return 0, s.err
		}

		s.pending, s.sent = s.pending[:0], 0
		s.fill()
	}

	n := copy(p, s.pending[s.sent:])
	s.sent += n
	return n, nil
}

func (s *stretcher) fill() {
	n, err := s.src.Read(s.read)
	if decodeErr := s.appendInput(s.read[:n]); decodeErr != nil {
		s.err = decodeErr
		return
	}

	switch {
	case err == nil:
		s.advance()
	case !errors.Is(err, io.EOF):
		s.err = err
	case len(s.raw) > 0:
		s.err = io.ErrUnexpectedEOF
	default:
		s.finish()
		s.err = io.EOF
	}

	pending, err := binary.Append(s.pending, binary.LittleEndian, s.emitted)
	if err != nil {
		s.err = err
		return
	}
	s.pending, s.emitted = pending, s.emitted[:0]
}

// appendInput keeps a byte that ends a read mid-sample for the next one.
func (s *stretcher) appendInput(b []byte) error {
	s.raw = append(s.raw, b...)
	whole := len(s.raw) / outputSampleBytes
	if cap(s.decoded) < whole {
		s.decoded = make([]int16, whole)
	}

	samples := s.decoded[:whole]
	if _, err := binary.Decode(s.raw, binary.LittleEndian, samples); err != nil {
		return err
	}

	for _, sample := range samples {
		s.in = append(s.in, float32(sample)/s16Scale)
	}
	s.raw = s.raw[:copy(s.raw, s.raw[whole*outputSampleBytes:])]

	return nil
}

func (s *stretcher) available() int {
	return s.base + len(s.in)
}

// The output length follows from the input length, which is unknown until
// the input ends, so a window is only placed once every possible length
// would place it the same way: its whole search range is in hand and it
// ends inside the shortest output the input could still produce.
func (s *stretcher) advance() {
	if !s.primed {
		if s.available() < stretchWindow || s.outputFor(s.available()) < stretchHop {
			return
		}
		s.prime()
	}

	for s.canPlace() {
		s.place()
	}

	s.trim()
}

func (s *stretcher) outputFor(inputSamples int) int {
	return int(float64(inputSamples) / s.speed)
}

func (s *stretcher) expected() int {
	return int(float64(s.out) * s.speed)
}

func (s *stretcher) canPlace() bool {
	return s.available() >= s.expected()+stretchSearch+stretchWindow &&
		s.outputFor(s.available()) >= s.out+stretchWindow
}

func (s *stretcher) prime() {
	s.emit(s.in[:stretchHop]...)
	s.tail = append(s.tail[:0], s.in[stretchHop:stretchWindow]...)
	s.out = stretchHop
	s.segEnd = stretchWindow
	s.primed = true
}

// place crossfades the overlap into the best-matching window near the
// expected input position and keeps that window's second half as the next
// overlap. It reports false when no whole window fits in the input.
func (s *stretcher) place() bool {
	expected := s.expected()
	lo := max(0, expected-stretchSearch)
	hi := min(s.available()-stretchWindow, expected+stretchSearch)
	if lo > hi {
		return false
	}

	best := s.bestMatch(lo, hi)
	window := s.in[best-s.base : best-s.base+stretchWindow]
	for k := range stretchHop {
		weight := float32(k) / float32(stretchHop)
		s.emit(s.tail[k]*(1-weight) + window[k]*weight)
	}

	s.tail = append(s.tail[:0], window[stretchHop:]...)
	s.out += stretchHop
	s.segEnd = best + stretchWindow
	return true
}

func (s *stretcher) bestMatch(lo, hi int) int {
	best, bestScore := lo, -math.MaxFloat64
	for start := lo; start <= hi; start += searchStride {
		candidate := s.in[start-s.base:]
		var dot, energy float64
		for k := 0; k < stretchHop; k += correlationStride {
			overlap, sample := float64(s.tail[k]), float64(candidate[k])
			dot += overlap * sample
			energy += sample * sample
		}

		if score := dot / math.Sqrt(energy+correlationEpsilon); score > bestScore {
			best, bestScore = start, score
		}
	}

	return best
}

// trim drops input that neither the next search nor the end-of-input flush
// can reach.
func (s *stretcher) trim() {
	keep := min(s.segEnd, max(0, s.expected()-stretchSearch))
	drop := keep - s.base
	if drop <= 0 {
		return
	}

	s.in = s.in[:copy(s.in, s.in[drop:])]
	s.base = keep
}

// finish places the windows that fit the now-known length, then writes the
// overlap and carries the last window on through the input up to that
// length, so the output ends on input samples instead of silence.
func (s *stretcher) finish() {
	length := s.outputFor(s.available())
	if !s.primed && (s.available() < stretchWindow || length < stretchHop) {
		s.emit(s.in[:min(length, len(s.in))]...)
		return
	}

	if !s.primed {
		s.prime()
	}

	for s.out+stretchWindow <= length {
		if !s.place() {
			break
		}
	}

	remaining := length - s.out
	overlap := s.tail[:min(remaining, len(s.tail))]
	s.emit(overlap...)

	start := s.segEnd - s.base
	s.emit(s.in[start:min(len(s.in), start+remaining-len(overlap))]...)
}

func (s *stretcher) emit(samples ...float32) {
	for _, sample := range samples {
		s.emitted = append(s.emitted, toS16(sample))
	}
}
