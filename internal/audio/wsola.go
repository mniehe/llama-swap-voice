package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/mniehe/llama-swap-voice/internal/fft"
)

// MinSpeed and MaxSpeed bound the speeds NewStretcher accepts.
const (
	MinSpeed = 0.25
	MaxSpeed = 4.0
)

const (
	millisPerSecond     = 1000
	stretchHopMillis    = 10
	stretchSearchMillis = 15
	stretchHop          = outputRate * stretchHopMillis / millisPerSecond
	stretchWindow       = 2 * stretchHop
	stretchSearch       = outputRate * stretchSearchMillis / millisPerSecond
	correlationEpsilon  = 1e-15
	stretchReadBytes    = 4096
	unstretchedSpeed    = 1
	// searchStarts is the most starts one search scores, one per sample of
	// ±stretchSearch; searchBlock is the input their overlaps span.
	searchStarts = 2*stretchSearch + 1
	searchBlock  = searchStarts - 1 + stretchHop
	// dotLanes is how many sums of the dot product run side by side. They
	// must tile the overlap exactly: a remainder makes the blank constant
	// negative, which fails to compile.
	dotLanes = 8
	_        = uint(-(stretchHop % dotLanes))
	// fftMinStarts is the fewest starts the FFT correlator scores; below it
	// the direct scan is faster. The correlator needs the whole search block
	// to fit its transform, or the constant below is negative and fails to
	// compile.
	fftMinStarts = 400
	_            = uint(fft.Size - searchBlock)
	// minMatchCorrelation is the normalised correlation below which a match
	// may not skip input. Voiced speech and steady tones match near 1;
	// consonants and noise match below it.
	minMatchCorrelation = 0.8
)

// NewStretcher time-stretches s16le 24 kHz mono samples by speed, keeping
// the pitch, with WSOLA: 20 ms windows placed every 10 ms of output, each
// taken from within 15 ms of its nominal input position where it best
// continues the previous one. The 30 ms search range must hold a whole
// pitch period, or a low voice's windows splice out of phase and crackle.
// Up to speed 2 only a strong match may skip input; after a weak one, such
// as in a consonant or noise, the window starts no later than the previous
// one ends.
//
// The first output comes once the input reaches one window, 20 ms, or one
// hop of output times speed if that is longer; an input that ends before
// then comes out when it ends. The source is read up to stretchReadBytes
// (85 ms) at a time, so a source that fills each read may have delivered up
// to that much more input by then.
//
// Input of n samples is stretched to floor(n/speed) samples, and the output
// is never longer and at most 10 ms shorter. Input shorter than one window,
// or whose floor(n/speed) is shorter than one hop, is not stretched: the
// output is its first min(n, floor(n/speed)) samples, so a slow speed
// returns all of it, a fast one its start, and fewer than speed samples
// none. Speed 1 returns r unchanged.
func NewStretcher(r io.Reader, speed float64) (io.Reader, error) {
	return newStretcherWith(r, speed, fft.Correlate)
}

// correlateFunc has fft.Correlate's contract: it sets dots to the exact
// cross-correlation of block and ref.
type correlateFunc func(dots, block, ref []float64) error

// newStretcherWith is NewStretcher with the correlator that wide searches
// use, so tests can compare correlators.
func newStretcherWith(r io.Reader, speed float64, correlate correlateFunc) (io.Reader, error) {
	if math.IsNaN(speed) || speed < MinSpeed || speed > MaxSpeed {
		return nil, fmt.Errorf("speed must be between %g and %g, got %g", MinSpeed, MaxSpeed, speed)
	}

	if speed == unstretchedSpeed {
		return r, nil
	}

	return &stretcher{
		src:       r,
		speed:     speed,
		correlate: correlate,
		wide:      new(wideSearch),
		read:      make([]byte, stretchReadBytes),
		raw:       make([]byte, 0, stretchReadBytes+outputSampleBytes),
		tail:      make([]float32, 0, stretchHop),
	}, nil
}

// wideSearch holds a wide search's input and dot products for the
// correlator. Arrays passed to a function value escape to the heap, so
// scoreStarts copies its local arrays here rather than passing them, which
// would allocate them on every search.
type wideSearch struct {
	dots  [searchStarts]float64
	block [searchBlock]float64
	ref   [stretchHop]float64
}

// stretcher holds the input from the earliest sample a later window can
// still use, and the second half of the last window, which the next one
// crossfades with. Output index out is always the number of samples emitted.
type stretcher struct {
	src       io.Reader
	speed     float64
	correlate correlateFunc
	wide      *wideSearch
	read      []byte
	raw       []byte
	decoded   []int16

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

	best := s.chooseStart(lo, hi)
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

// chooseStart takes the best match in [lo, hi] when it is strong. A weak
// match is no sign that the input it skips repeats what is kept, so the
// start then stays between the previous window's start and its end, which
// skips nothing. Above speed 2 [lo, hi] can lie wholly past the previous
// window, and the weak match stands.
func (s *stretcher) chooseStart(lo, hi int) int {
	var scores [searchStarts]float64
	tailNorm := s.scoreStarts(lo, hi, &scores)
	best, score := bestStart(&scores, lo, lo, hi)
	if score/tailNorm >= minMatchCorrelation {
		return best
	}

	prevStart := s.segEnd - stretchWindow
	if best >= prevStart && best <= s.segEnd {
		return best
	}

	guardedLo, guardedHi := max(lo, prevStart), min(hi, s.segEnd)
	if guardedLo > guardedHi {
		return best
	}

	guarded, _ := bestStart(&scores, lo, guardedLo, guardedHi)
	return guarded
}

// scoreStarts sets scores[start-lo], for each start in [lo, hi], to how well
// the window from start continues the overlap, and returns the overlap's
// norm, which turns a score into a normalised correlation.
//
// Every sample is a multiple of 2^-15 in [-1, 1), so each product is a
// multiple of 2^-30 and a sum of stretchHop of them needs under 38 of
// float64's 53 bits. The sums are therefore exact in any order: the lanes
// of the dot product and the sliding energy give the scores a plain loop
// would.
//
// A wide search takes its dot products from the FFT correlator, which
// returns the same exact sums. Every streaming search is wide; a narrow one
// happens only while finish flushes the end of the input, which for a
// short input can include its first placement. It uses the direct scan,
// which is written out here, not in a function of its own, because the
// race detector instruments every read through a pointer but not reads of
// a local array, and this loop's reads made the race tests much slower
// when it did. Go does not unroll loops, so the lanes are written out too.
func (s *stretcher) scoreStarts(lo, hi int, scores *[searchStarts]float64) (tailNorm float64) {
	var reference [stretchHop]float64
	var block [searchBlock]float64
	stage(reference[:], s.tail)
	stage(block[:], s.in[lo-s.base:hi-s.base+stretchHop])
	tailNorm = math.Sqrt(sumSquares(reference[:]) + correlationEpsilon)

	var energies [searchStarts]float64
	slideEnergies(&block, hi-lo, &energies)
	if hi-lo+1 >= fftMinStarts && s.correlateWide(hi-lo, &block, &reference) {
		for offset := 0; offset <= hi-lo; offset++ {
			scores[offset] = s.wide.dots[offset] / math.Sqrt(energies[offset]+correlationEpsilon)
		}
		return tailNorm
	}

	for offset := 0; offset <= hi-lo; offset++ {
		var l0, l1, l2, l3, l4, l5, l6, l7 float64
		for k := 0; k <= stretchHop-dotLanes; k += dotLanes {
			l0 += reference[k] * block[offset+k]
			l1 += reference[k+1] * block[offset+k+1]
			l2 += reference[k+2] * block[offset+k+2]
			l3 += reference[k+3] * block[offset+k+3]
			l4 += reference[k+4] * block[offset+k+4]
			l5 += reference[k+5] * block[offset+k+5]
			l6 += reference[k+6] * block[offset+k+6]
			l7 += reference[k+7] * block[offset+k+7]
		}

		dot := (l0 + l1) + (l2 + l3) + (l4 + l5) + (l6 + l7)
		scores[offset] = dot / math.Sqrt(energies[offset]+correlationEpsilon)
	}

	return tailNorm
}

// correlateWide sets s.wide.dots[offset], for each offset in [0, last], to
// the dot product of reference with block[offset:offset+stretchHop], and
// reports whether the correlator accepted the lengths.
func (s *stretcher) correlateWide(last int, block *[searchBlock]float64, reference *[stretchHop]float64) bool {
	wide := s.wide
	n := copy(wide.block[:], block[:last+stretchHop])
	wide.ref = *reference
	return s.correlate(wide.dots[:last+1], wide.block[:n], wide.ref[:]) == nil
}

// slideEnergies sets energies[offset], for each offset in [0, last], to the
// energy of block[offset:offset+stretchHop]. Only the first is summed; each
// later one drops the sample that left the window and adds the one that
// entered it.
func slideEnergies(block *[searchBlock]float64, last int, energies *[searchStarts]float64) {
	energies[0] = sumSquares(block[:stretchHop])
	for offset := 1; offset <= last; offset++ {
		outgoing, incoming := block[offset-1], block[offset-1+stretchHop]
		energies[offset] = energies[offset-1] + incoming*incoming - outgoing*outgoing
	}
}

// bestStart returns the first start in [from, to] with the highest score,
// where scores[start-lo] is the score of start.
func bestStart(scores *[searchStarts]float64, lo, from, to int) (start int, score float64) {
	best, bestScore := from, -math.MaxFloat64
	for candidate := from; candidate <= to; candidate++ {
		if candidateScore := scores[candidate-lo]; candidateScore > bestScore {
			best, bestScore = candidate, candidateScore
		}
	}

	return best, bestScore
}

func stage(dst []float64, src []float32) {
	for k, sample := range src {
		dst[k] = float64(sample)
	}
}

func sumSquares(samples []float64) float64 {
	var sum float64
	for _, sample := range samples {
		sum += sample * sample
	}

	return sum
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
