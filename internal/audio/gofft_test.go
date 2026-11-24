package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"math/rand/v2"
	"strconv"
	"testing"

	gofft "github.com/go-fft/fft"
	"github.com/stretchr/testify/require"

	"github.com/mniehe/llama-swap-voice/internal/fft"
)

const (
	// goFFTDotScale turns a lag into units of 2^-30: IRFFT already divides
	// by the transform length, so no further gain needs undoing.
	goFFTDotScale = 0x1p30
	goFFTDotStep  = 0x1p-30
	goFFTBins     = fft.Size/2 + 1
)

// goFFTCorrelator is fft.Correlate's contract built on go-fft's real plan,
// for comparison only. It holds its buffers, so one value serves one
// stretcher.
//
// Production uses internal/fft on purpose. go-fft measured about three
// times faster in BenchmarkStretcher_Speech60s, but internal/fft is fast
// enough for now, needs no dependency, and its exactness is proven
// (internal/fft/PROOF.md). go-fft's exactness is only tested, and its
// kernels, routes and fused multiply-adds vary by CPU.
type goFFTCorrelator struct {
	plan          *gofft.RealPlan
	block, ref    [fft.Size]float64
	lags          [fft.Size]float64
	blockSpectrum [goFFTBins]complex128
	refSpectrum   [goFFTBins]complex128
}

func goFFTCorrelate() correlateFunc {
	correlator := &goFFTCorrelator{plan: gofft.NewRealPlan(fft.Size)}
	return correlator.correlate
}

func (c *goFFTCorrelator) correlate(dots, block, ref []float64) error {
	if len(block) > fft.Size || len(ref) > len(block) || len(dots) < len(block)-len(ref)+1 {
		return fmt.Errorf("%w: block %d, reference %d, dots %d, size %d", fft.ErrLength, len(block), len(ref), len(dots), fft.Size)
	}
	if len(ref) == 0 {
		clear(dots[:len(block)+1])
		return nil
	}

	clear(c.block[copy(c.block[:], block):])
	clear(c.ref[copy(c.ref[:], ref):])
	c.plan.RFFT(c.blockSpectrum[:], c.block[:])
	c.plan.RFFT(c.refSpectrum[:], c.ref[:])
	for k := range c.blockSpectrum {
		c.blockSpectrum[k] *= cmplx.Conj(c.refSpectrum[k])
	}
	c.plan.IRFFT(c.lags[:], c.blockSpectrum[:])

	for k := range len(block) - len(ref) + 1 {
		dots[k] = float64(int64(math.Round(c.lags[k]*goFFTDotScale))) * goFFTDotStep
	}

	return nil
}

// goFFTStretcher is NewStretcher with the go-fft adapter as its correlator.
func goFFTStretcher(r io.Reader, speed float64) (io.Reader, error) {
	return newStretcherWith(r, speed, goFFTCorrelate())
}

// goFFTPatterns are s16 inputs for the per-lag test: random samples, a
// single impulse, full-scale samples of alternating sign, whose products
// cancel from one lag to the next, and constant full scale, the largest
// sums.
var goFFTPatterns = map[string]func(rng *rand.ChaCha8, length int) []int16{
	"random": func(rng *rand.ChaCha8, length int) []int16 {
		samples := make([]int16, length)
		for i := range samples {
			samples[i] = int16(rng.Uint64() >> randomSampleShift) //nolint:gosec // G115: the shift leaves 16 bits.
		}
		return samples
	},
	"impulse": func(_ *rand.ChaCha8, length int) []int16 {
		samples := make([]int16, length)
		samples[length/2] = math.MaxInt16
		return samples
	},
	"alternating": func(_ *rand.ChaCha8, length int) []int16 {
		samples := make([]int16, length)
		for i := range samples {
			samples[i] = math.MaxInt16
			if i%2 == 1 {
				samples[i] = math.MinInt16
			}
		}
		return samples
	},
	"full scale": func(_ *rand.ChaCha8, length int) []int16 {
		samples := make([]int16, length)
		for i := range samples {
			samples[i] = math.MinInt16
		}
		return samples
	},
}

// randomSampleShift keeps the top 16 bits of a random uint64.
const randomSampleShift = 48

// The stretcher falls back to the exact direct scan when a correlator
// fails, so its output cannot show a broken adapter. This compares every
// dot product the adapter returns with an exact integer sum.
func TestGoFFTCorrelator_EveryLagIsExact(t *testing.T) {
	t.Parallel()

	blockLengths := []int{fftMinStarts - 1 + stretchHop, searchBlock}
	for name, pattern := range goFFTPatterns {
		for _, blockLength := range blockLengths {
			t.Run(name+"/"+strconv.Itoa(blockLength), func(t *testing.T) {
				t.Parallel()
				rng := rand.NewChaCha8(noiseSeed)
				block, ref := pattern(rng, blockLength), pattern(rng, stretchHop)
				dots := make([]float64, blockLength-stretchHop+1)

				require.NoError(t, goFFTCorrelate()(dots, unitSamples(block), unitSamples(ref)))

				for lag, want := range exactDots(block, ref) {
					require.Equal(t, math.Float64bits(want), math.Float64bits(dots[lag]), "lag %d: want %g, got %g", lag, want, dots[lag])
				}
			})
		}
	}
}

func TestGoFFTCorrelator_ShortDots_ReturnErrLength(t *testing.T) {
	var block [searchBlock]float64
	var ref [stretchHop]float64
	dots := make([]float64, searchBlock-stretchHop)

	err := goFFTCorrelate()(dots, block[:], ref[:])

	require.ErrorIs(t, err, fft.ErrLength)
}

// exactDots are the dot products of ref with each window of block, summed
// as integers and scaled to the samples' units of 2^-15 squared.
func exactDots(block, ref []int16) []float64 {
	dots := make([]float64, len(block)-len(ref)+1)
	for lag := range dots {
		var sum int64
		for i, sample := range ref {
			sum += int64(block[lag+i]) * int64(sample)
		}
		dots[lag] = float64(sum) * goFFTDotStep
	}

	return dots
}

func unitSamples(samples []int16) []float64 {
	unit := make([]float64, len(samples))
	for i, sample := range samples {
		unit[i] = float64(sample) / s16Scale
	}

	return unit
}

// BenchmarkCorrelator_Call times one correlation of the size a streaming
// search runs: a 960-sample block against the 240-sample reference.
func BenchmarkCorrelator_Call(b *testing.B) {
	samples := make([]int16, searchBlock+stretchHop)
	_, err := binary.Decode(fixture(b, "speech.wav")[wavHeaderBytes:], binary.LittleEndian, samples)
	require.NoError(b, err)
	var block [searchBlock]float64
	var ref [stretchHop]float64
	for i := range block {
		block[i] = float64(samples[i]) / s16Scale
	}
	for i := range ref {
		ref[i] = float64(samples[searchBlock+i]) / s16Scale
	}

	for _, bench := range benchCorrelators {
		b.Run("correlator="+bench.name, func(b *testing.B) {
			correlate := bench.correlator()
			var dots [searchStarts]float64
			for b.Loop() {
				require.NoError(b, correlate(dots[:], block[:], ref[:]))
			}
		})
	}
}
