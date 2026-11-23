package audio

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// loudGapPeak is 2% of full scale: an omitted span quieter than this is
	// silence for the purpose of the gap test.
	loudGapPeak       = 0.02 * s16Scale
	maxLoudGapSamples = outputRate * 2 / millisPerSecond
	burstSilence      = outputRate / 10
	burstSamples      = outputRate
	burstAmplitude    = 0.2 * math.MaxInt16
	burstSeed         = 11
)

// guardSpeeds are the household speed and a fast one; at both the nominal
// advance per hop is shorter than a window, so no input need be skipped.
var guardSpeeds = []float64{1.15, 1.5}

// span is the half-open input range [start, end).
type span struct {
	start, end int
}

// windowStarts steps a stretcher over the whole input as finish does, prime
// then place until the output length is reached, and returns where each
// window starts in the input, the primed first window included.
func windowStarts(t *testing.T, samples []int16, speed float64) []int {
	t.Helper()

	reader, err := NewStretcher(nil, speed)
	require.NoError(t, err)
	s, ok := reader.(*stretcher)
	require.True(t, ok)
	require.NoError(t, s.appendInput(pcmOf(t, samples)))
	require.GreaterOrEqual(t, s.available(), stretchWindow)

	s.prime()
	starts := []int{0}
	length := s.outputFor(s.available())
	for s.out+stretchWindow <= length && s.place() {
		starts = append(starts, s.segEnd-stretchWindow)
	}

	return starts
}

// uncoveredSpans lists the runs of input before the last window's end that
// no window covers.
func uncoveredSpans(starts []int) []span {
	end := 0
	for _, start := range starts {
		end = max(end, start+stretchWindow)
	}

	covered := make([]bool, end)
	for _, start := range starts {
		for i := start; i < start+stretchWindow; i++ {
			covered[i] = true
		}
	}

	var spans []span
	for i := 0; i < end; i++ {
		if covered[i] {
			continue
		}

		run := span{start: i}
		for i < end && !covered[i] {
			i++
		}
		run.end = i
		spans = append(spans, run)
	}

	return spans
}

func peakOf(samples []int16) int {
	peak := 0
	for _, sample := range samples {
		peak = max(peak, abs(int(sample)))
	}

	return peak
}

// A far jump drops a whole stretch of input. It is right for a strong match,
// where the dropped stretch is a pitch period like its neighbours, and wrong
// for a weak one, where it deletes part of a consonant.
func TestNewStretcher_Speech_WeakMatchesDropNoInput(t *testing.T) {
	speech := samplesOf(t, speechPCM(t))

	for _, speed := range guardSpeeds {
		t.Run(speedName(speed), func(t *testing.T) {
			for _, gap := range uncoveredSpans(windowStarts(t, speech, speed)) {
				omitted := speech[gap.start:gap.end]
				peak := peakOf(omitted)
				t.Logf("uncovered %d..%d: %d samples, peak %.4f of full scale", gap.start, gap.end, len(omitted), float64(peak)/s16Scale)
				if float64(peak) < loudGapPeak {
					continue
				}

				assert.LessOrEqual(t, len(omitted), maxLoudGapSamples, "loud input %d..%d dropped", gap.start, gap.end)
			}
		})
	}
}

// noiseBurst is seeded white noise between two stretches of silence.
func noiseBurst() []int16 {
	rng := rand.New(rand.NewPCG(burstSeed, burstSeed)) //nolint:gosec // G404: reproducible test noise.
	samples := make([]int16, burstSilence+burstSamples+burstSilence)
	for i := burstSilence; i < burstSilence+burstSamples; i++ {
		samples[i] = int16(burstAmplitude * (2*rng.Float64() - 1))
	}

	return samples
}

func TestNewStretcher_NoiseBurst_DropsNoInput(t *testing.T) {
	burst := span{start: burstSilence, end: burstSilence + burstSamples}

	for _, speed := range guardSpeeds {
		t.Run(speedName(speed), func(t *testing.T) {
			for _, gap := range uncoveredSpans(windowStarts(t, noiseBurst(), speed)) {
				overlap := min(gap.end, burst.end) - max(gap.start, burst.start)
				assert.LessOrEqual(t, overlap, 0, "burst input %d..%d dropped", gap.start, gap.end)
			}
		})
	}
}
