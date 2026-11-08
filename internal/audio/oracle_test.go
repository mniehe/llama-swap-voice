package audio

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	frequencyTolerance = 2.0
	lengthTolerance    = 20 * time.Millisecond
	scanLowHz          = 100
	scanHighHz         = 2000
	scanStepHz         = 1.0
	refineStepHz       = 0.05
	toneThreshold      = s16Scale / 10
	peakToleranceDB    = 0.4

	// shine adds 1.02 dB on top of the -2 dB input scale.
	mp3NetGainDB = -0.98
)

func requireTool(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not on PATH, so the oracle cannot run: %v", name, err)
	}

	return path
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	return path
}

func ffmpegDecode(t *testing.T, mp3 []byte) []int16 {
	t.Helper()

	ffmpeg := requireTool(t, "ffmpeg")
	in := writeTemp(t, "reply.mp3", mp3)
	cmd := exec.CommandContext(t.Context(), ffmpeg, "-nostdin", "-loglevel", "error", "-i", in,
		"-f", "s16le", "-ac", strconv.Itoa(outputChannels), "-ar", strconv.Itoa(outputRate), "pipe:1")
	raw, err := cmd.Output()
	require.NoError(t, err, "ffmpeg could not decode the MP3")

	samples := make([]int16, len(raw)/outputSampleBytes)
	_, err = binary.Decode(raw, binary.LittleEndian, samples)
	require.NoError(t, err)

	return samples
}

func opusdecDecode(t *testing.T, ogg []byte) (samples []int16, rate int) {
	t.Helper()

	opusdec := requireTool(t, "opusdec")
	in := writeTemp(t, "reply.opus", ogg)
	out := filepath.Join(t.TempDir(), "reply.wav")
	report, err := exec.CommandContext(t.Context(), opusdec, in, out).CombinedOutput()
	t.Logf("opusdec:\n%s", report)
	require.NoError(t, err, "opusdec could not decode the stream")

	wav, err := os.ReadFile(out)
	require.NoError(t, err)
	format, data, err := parseWAV(wav)
	require.NoError(t, err)
	require.Equal(t, pcm16Bits, format.bits)
	require.Equal(t, outputChannels, format.channels)

	samples = make([]int16, len(data)/outputSampleBytes)
	_, err = binary.Decode(data, binary.LittleEndian, samples)
	require.NoError(t, err)

	return samples, format.rate
}

func goertzelPower(samples []int16, rate int, hz float64) float64 {
	coefficient := 2 * math.Cos(2*math.Pi*hz/float64(rate))
	var previous, beforePrevious float64
	for _, sample := range samples {
		current := float64(sample) + coefficient*previous - beforePrevious
		beforePrevious, previous = previous, current
	}

	return previous*previous + beforePrevious*beforePrevious - coefficient*previous*beforePrevious
}

func peakFrequency(samples []int16, rate int) float64 {
	best, bestPower := 0.0, -1.0
	scan := func(low, high, step float64) {
		for hz := low; hz <= high; hz += step {
			if power := goertzelPower(samples, rate, hz); power > bestPower {
				best, bestPower = hz, power
			}
		}
	}

	scan(scanLowHz, scanHighHz, scanStepHz)
	scan(best-scanStepHz, best+scanStepHz, refineStepHz)

	return best
}

// toneLength is the time from the first to the last sample loud enough to
// belong to the tone, which ignores codec delay and trailing padding.
func toneLength(samples []int16, rate int) time.Duration {
	threshold := int16(toneThreshold)
	first, last := -1, -1
	for i, sample := range samples {
		if sample > threshold || sample < -threshold {
			if first < 0 {
				first = i
			}
			last = i
		}
	}

	return time.Duration(last-first+1) * time.Second / time.Duration(rate)
}

func samplesLength(count, rate int) time.Duration {
	return time.Duration(count) * time.Second / time.Duration(rate)
}

func TestNewEncoder_Opus_OpusinfoReportsEncoderLookahead(t *testing.T) {
	opusinfo := requireTool(t, "opusinfo")
	stream := writeTemp(t, "speech.opus", encode(t, Opus, fixture(t, "speech.wav")[wavHeaderBytes:]))

	report, err := exec.CommandContext(t.Context(), opusinfo, stream).CombinedOutput()
	t.Logf("opusinfo:\n%s", report)
	require.NoError(t, err)

	assert.NotContains(t, string(report), "WARNING")
	assert.Contains(t, string(report), "Pre-skip: "+strconv.Itoa(int(encoderPreSkip(t)))+"\n")
	assert.Contains(t, string(report), "Original sample rate: 24000 Hz")
}

func TestNewEncoder_Opus_PageEdgeLengthsDecodeExactly(t *testing.T) {
	opusinfo := requireTool(t, "opusinfo")
	pcm := s16le(tone(outputRate, toneSeconds, toneAmplitude))

	for _, samples := range pageEdgeLengths {
		t.Run(strconv.Itoa(samples), func(t *testing.T) {
			ogg := encode(t, Opus, pcm[:samples*outputSampleBytes])
			stream := writeTemp(t, "edge.opus", ogg)

			report, err := exec.CommandContext(t.Context(), opusinfo, stream).CombinedOutput()
			t.Logf("opusinfo:\n%s", report)
			assert.NotContains(t, string(report), "WARNING")
			require.NoError(t, err)

			decoded, rate := opusdecDecode(t, ogg)
			require.Equal(t, outputRate, rate)
			assert.Len(t, decoded, samples)
		})
	}
}

func TestNewEncoder_Opus_ToneDecodesAt440Hz(t *testing.T) {
	input := tone(outputRate, toneSeconds, toneAmplitude)

	samples, rate := opusdecDecode(t, encode(t, Opus, s16le(input)))

	peak := peakFrequency(samples, rate)
	t.Logf("opus: %d samples at %d Hz, peak %.2f Hz, tone %s", len(samples), rate, peak, toneLength(samples, rate))
	assert.InDelta(t, toneHz, peak, frequencyTolerance)
	assert.InDelta(t, samplesLength(len(input), outputRate), samplesLength(len(samples), rate), float64(lengthTolerance))
	assert.InDelta(t, samplesLength(len(input), outputRate), toneLength(samples, rate), float64(lengthTolerance))
}

func TestNewEncoder_MP3_ToneDecodesAt440Hz(t *testing.T) {
	input := tone(outputRate, toneSeconds, toneAmplitude)

	samples := ffmpegDecode(t, encode(t, MP3, s16le(input)))

	peak := peakFrequency(samples, outputRate)
	t.Logf("mp3: %d samples, peak %.2f Hz, tone %s", len(samples), peak, toneLength(samples, outputRate))
	assert.InDelta(t, toneHz, peak, frequencyTolerance)
	assert.InDelta(t, samplesLength(len(input), outputRate), toneLength(samples, outputRate), float64(lengthTolerance))
	assert.GreaterOrEqual(t, len(samples), len(input)+shineDelaySamples, "decoded MP3 is shorter than its delayed input")
	assert.Less(t, len(samples), len(input)+shineDelaySamples+mp3FrameSamples, "decoded MP3 carries more than one frame of padding")
}

func TestNewEncoder_MP3_LoudTonesDoNotClip(t *testing.T) {
	for _, inputPeak := range []int{17828, 29000, math.MaxInt16} {
		t.Run(strconv.Itoa(inputPeak), func(t *testing.T) {
			input := tone(outputRate, toneSeconds, float64(inputPeak)/s16Scale)

			samples := ffmpegDecode(t, encode(t, MP3, s16le(input)))

			clipped, peak := 0, 0
			for _, sample := range samples {
				level := abs(int(sample))
				if level >= math.MaxInt16 {
					clipped++
				}
				peak = max(peak, level)
			}

			t.Logf("mp3: %d of %d decoded samples clipped, peak %d", clipped, len(samples), peak)
			assert.Zero(t, clipped, "decoded samples at full scale")
			gain := 20 * math.Log10(float64(peak)/float64(inputPeak))
			assert.InDelta(t, mp3NetGainDB, gain, peakToleranceDB, "decoded peak %d", peak)
		})
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}

	return value
}
