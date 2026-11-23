package audio

import (
	"bytes"
	"io"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	spliceToneSeconds   = 4
	spliceToneScale     = 6000
	spliceMaxHarmonicHz = 4000
	splicePhaseStep     = 0.7
	spliceEdgeFraction  = 8
	minSpliceSNRDecibel = 60
	decibelsPerDecade   = 10
	benchSpeechSeconds  = 60
	benchSpeed          = 1.15
)

var (
	// splicePeriods are pitch periods in samples from 60 Hz to 258 Hz, odd
	// and even, on both sides of the old 240-sample search span.
	splicePeriods = []int{400, 343, 320, 300, 282, 260, 241, 240, 200, 161, 120, 93}
	spliceSpeeds  = []float64{1.15, 0.85, 1.5}
)

// harmonicTone is a steady voiced tone whose period is a whole number of
// samples: every harmonic below 4 kHz at amplitude 1/k, with spread phases
// so the waveform has no symmetry a misaligned splice could hide behind.
func harmonicTone(period int) []float32 {
	f0 := float64(outputRate) / float64(period)
	samples := make([]float32, spliceToneSeconds*outputRate)
	for i := range samples {
		var sum float64
		for k := 1; float64(k)*f0 < spliceMaxHarmonicHz; k++ {
			harmonic := float64(k)
			phase := 2*math.Pi*harmonic*f0*float64(i)/outputRate + splicePhaseStep*harmonic*harmonic
			sum += math.Sin(phase) / harmonic
		}
		samples[i] = float32(spliceToneScale * sum / s16Scale)
	}

	return samples
}

// spliceSNR compares each sample of the middle 75% of a stretched periodic
// signal with the sample one period later. A splice that joins the waveform
// out of phase breaks the periodicity and lowers the ratio; exact splices
// leave only rounding.
func spliceSNR(samples []int16, period int) float64 {
	edge := len(samples) / spliceEdgeFraction
	var signal, noise float64
	for i := edge; i+period < len(samples)-edge; i++ {
		a, b := float64(samples[i]), float64(samples[i+period])
		signal += a*a + b*b
		noise += (a - b) * (a - b)
	}

	return decibelsPerDecade * math.Log10(signal/noise)
}

func TestNewStretcher_LowPitchedTones_SpliceCleanly(t *testing.T) {
	for _, period := range splicePeriods {
		input := s16le(harmonicTone(period))
		for _, speed := range spliceSpeeds {
			t.Run(strconv.Itoa(period)+"@"+speedName(speed), func(t *testing.T) {
				out := samplesOf(t, stretch(t, input, speed))

				snr := spliceSNR(out, period)
				t.Logf("period %d samples (%.1f Hz), speed %g: %.1f dB", period, float64(outputRate)/float64(period), speed, snr)

				assert.GreaterOrEqual(t, snr, float64(minSpliceSNRDecibel))
			})
		}
	}
}

func BenchmarkStretcher_Speech60s(b *testing.B) {
	speech := fixture(b, "speech.wav")[wavHeaderBytes:]
	clipSeconds := len(speech) / outputSampleBytes / outputRate
	input := bytes.Repeat(speech, benchSpeechSeconds/clipSeconds)
	b.SetBytes(int64(len(input)))

	for b.Loop() {
		stretcher, err := NewStretcher(bytes.NewReader(input), benchSpeed)
		require.NoError(b, err)
		_, err = io.Copy(io.Discard, stretcher)
		require.NoError(b, err)
	}
}
