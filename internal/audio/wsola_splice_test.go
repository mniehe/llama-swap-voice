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
	// highTonePeriods are 6 kHz, 4.8 kHz and 3 kHz, the sibilant range.
	highTonePeriods = []int{4, 5, 8}
	// sinePhases put the 4-sample tone on its zero crossings at 0 and off
	// them at 0.7 radians.
	sinePhases = []float64{0, 0.7}
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

// sineTone is a pure tone whose period is a whole number of samples,
// starting at the given phase in radians.
func sineTone(period int, startPhase float64) []float32 {
	samples := make([]float32, spliceToneSeconds*outputRate)
	for i := range samples {
		phase := 2*math.Pi*float64(i)/float64(period) + startPhase
		samples[i] = float32(toneAmplitude * math.Sin(phase))
	}

	return samples
}

func TestNewStretcher_LowPitchedTones_SpliceCleanly(t *testing.T) {
	assertSplicesCleanly(t, harmonicTone, splicePeriods)
}

// A correlation that skips samples is blind to tones near its own sampling
// rate, so high tones catch a matcher that does not use every sample. Which
// tone a skipping matcher gets wrong depends on where the samples fall on the
// waveform, so each tone is tried at two phases.
func TestNewStretcher_HighTones_SpliceCleanly(t *testing.T) {
	for _, startPhase := range sinePhases {
		t.Run("phase "+strconv.FormatFloat(startPhase, 'f', -1, 64), func(t *testing.T) {
			tone := func(period int) []float32 { return sineTone(period, startPhase) }
			assertSplicesCleanly(t, tone, highTonePeriods)
		})
	}
}

func assertSplicesCleanly(t *testing.T, tone func(period int) []float32, periods []int) {
	t.Helper()

	for _, period := range periods {
		input := s16le(tone(period))
		for _, speed := range spliceSpeeds {
			t.Run(strconv.Itoa(period)+"@"+speedName(speed), func(t *testing.T) {
				t.Parallel()
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
