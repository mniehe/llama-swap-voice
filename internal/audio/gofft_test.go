package audio

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"testing"

	gofft "github.com/go-fft/fft"
	"github.com/stretchr/testify/assert"
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

// hashSpeeds are the speeds whose output on the 60 s speech input must be
// the same with either correlator.
var hashSpeeds = []float64{0.25, 1.10, 1.15, 1.20}

// goFFTCorrelator is fft.Correlate's contract built on go-fft's real plan,
// for comparison only. It holds its buffers, so one value serves one
// stretcher.
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

func TestGoFFTCorrelator_MatchesExactOracle(t *testing.T) {
	t.Parallel()
	runExactOracle(t, goFFTCorrelate)
}

func TestGoFFTCorrelator_Speech60s_MatchesInternal(t *testing.T) {
	input := speech60s(t)

	for _, speed := range hashSpeeds {
		t.Run(speedName(speed), func(t *testing.T) {
			t.Parallel()
			want := stretchAll(t, input, speed, internalCorrelator)
			got := stretchAll(t, input, speed, goFFTCorrelate)
			t.Logf("speed %g: internal %x, go-fft %x", speed, sha256.Sum256(want), sha256.Sum256(got))

			assert.Equal(t, sha256.Sum256(want), sha256.Sum256(got))
		})
	}
}

func stretchAll(t *testing.T, input []byte, speed float64, correlator newCorrelator) []byte {
	t.Helper()

	stretcher, err := newStretcherWith(bytes.NewReader(input), speed, correlator())
	require.NoError(t, err)
	out, err := io.ReadAll(stretcher)
	require.NoError(t, err)

	return out
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
