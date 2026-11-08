package audio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"strconv"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	stretchToneSeconds     = 2
	stretchLengthTolerance = 0.02
	maxShortfallSamples    = outputRate / 20
	pitchSliceSamples      = outputRate
	tailWindowSamples      = outputRate / 100
	tailCheckSamples       = outputRate / 10
	maxZeroRunSamples      = outputRate / 200
	minTailRMSRatio        = 0.25
	streamChunkBytes       = outputRate / 50 * outputSampleBytes
	streamPauseBytes       = outputRate / 2 * outputSampleBytes
	minStreamedBytes       = outputRate / 5 * outputSampleBytes
	streamTimeout          = 2 * time.Second
	shortContractSamples   = outputRate / 10
	maxRandomChunkBytes    = 9000
	oneByteMaxSamples      = 5000
	randomLengthCount      = 8
	maxRandomLength        = 60000
	chunkSeed              = 7
)

var (
	stretchSpeeds  = []float64{0.25, 0.5, 1.15, 2.0, 4.0}
	slowSpeeds     = []float64{0.5, 0.25}
	oracleSpeeds   = append([]float64{0.2500001, 1.0 / 3, 0.999999, 1.000001, 3.999999}, stretchSpeeds...)
	oracleLengths  = []int{0, 1, 2, 119, 120, 239, 240, 241, 359, 360, 479, 480, 481, 719, 720, 721, 959, 960, 961, 1199, 1200, 1201, 1919, 1920, 1921, 2400, 4800, 9999, 24000}
	errSourceBroke = errors.New("source broke")
	noiseSeed      = [32]byte{1}
)

func speedName(speed float64) string {
	return strconv.FormatFloat(speed, 'f', -1, 64)
}

func samplesOf(t *testing.T, pcm []byte) []int16 {
	t.Helper()

	samples := make([]int16, len(pcm)/outputSampleBytes)
	_, err := binary.Decode(pcm, binary.LittleEndian, samples)
	require.NoError(t, err)

	return samples
}

func pcmOf(t *testing.T, samples []int16) []byte {
	t.Helper()

	pcm, err := binary.Append(nil, binary.LittleEndian, samples)
	require.NoError(t, err)

	return pcm
}

func newStretcher(t *testing.T, r io.Reader, speed float64) io.Reader {
	t.Helper()

	stretcher, err := NewStretcher(r, speed)
	require.NoError(t, err)

	return stretcher
}

func stretch(t *testing.T, pcm []byte, speed float64) []byte {
	t.Helper()

	out, err := io.ReadAll(newStretcher(t, bytes.NewReader(pcm), speed))
	require.NoError(t, err)

	return out
}

func speechPCM(t *testing.T) []byte {
	return fixture(t, "speech.wav")[wavHeaderBytes:]
}

// lengthContract is the stretcher's length contract: at slow speeds the
// output can stop up to 50 ms short of input/speed, where the prototype
// padded with silence.
func lengthContract(want float64) float64 {
	return max(want*stretchLengthTolerance, maxShortfallSamples)
}

func TestNewStretcher_RejectsSpeedsOutsideRange(t *testing.T) {
	for _, speed := range []float64{0, -1, math.NaN(), math.Inf(1), 5e-324, 0.2499, 4.0001} {
		t.Run(speedName(speed), func(t *testing.T) {
			stretcher, err := NewStretcher(bytes.NewReader(speechPCM(t)), speed)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "between 0.25 and 4")
			assert.Nil(t, stretcher)
		})
	}

	for _, speed := range []float64{MinSpeed, 1, MaxSpeed} {
		t.Run(speedName(speed), func(t *testing.T) {
			assert.NotEmpty(t, stretch(t, speechPCM(t), speed))
		})
	}
}

func TestNewStretcher_Tone_KeepsPitchAndScalesLength(t *testing.T) {
	input := s16le(tone(outputRate, stretchToneSeconds, toneAmplitude))
	inputSamples := len(input) / outputSampleBytes

	for _, speed := range stretchSpeeds {
		t.Run(speedName(speed), func(t *testing.T) {
			out := samplesOf(t, stretch(t, input, speed))

			want := float64(inputSamples) / speed
			slice := min(pitchSliceSamples, len(out)/2)
			start := (len(out) - slice) / 2
			peak := peakFrequency(out[start:start+slice], outputRate)
			t.Logf("speed %g: %d samples, want %.0f (%+.2f%%), peak %.2f Hz over %d samples", speed, len(out), want, 100*(float64(len(out))-want)/want, peak, slice)

			assert.InDelta(t, want, float64(len(out)), lengthContract(want))
			assert.InDelta(t, toneHz, peak, frequencyTolerance)
		})
	}
}

func TestNewStretcher_ShortInputs_StopShortOrPassThrough(t *testing.T) {
	input := s16le(tone(outputRate, stretchToneSeconds, toneAmplitude))

	tests := []struct {
		samples int
		speed   float64
		want    int
	}{
		{samples: stretchWindow, speed: 0.25, want: 2 * stretchWindow},
		{samples: 100, speed: 0.25, want: 100},
		{samples: 100, speed: 0.5, want: 100},
		{samples: 100, speed: 2, want: 50},
		{samples: 0, speed: 0.5, want: 0},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.samples)+"@"+speedName(tt.speed), func(t *testing.T) {
			pcm := input[:tt.samples*outputSampleBytes]

			out := stretch(t, pcm, tt.speed)

			assert.Len(t, out, tt.want*outputSampleBytes)
			if tt.samples < stretchWindow {
				assert.Equal(t, pcm[:len(out)], out, "a reply shorter than one window is not stretched")
			}
		})
	}
}

func TestNewStretcher_SlowSpeeds_StayWithinTheLengthContract(t *testing.T) {
	input := s16le(tone(outputRate, stretchToneSeconds, toneAmplitude))

	for _, speed := range slowSpeeds {
		t.Run(speedName(speed), func(t *testing.T) {
			worst, worstAt := 0, 0
			for samples := stretchWindow; samples <= shortContractSamples; samples++ {
				out := len(stretch(t, input[:samples*outputSampleBytes], speed)) / outputSampleBytes
				want := int(float64(samples) / speed)

				require.LessOrEqual(t, out, want, "%d samples", samples)
				require.InDelta(t, want, out, lengthContract(float64(want)), "%d samples", samples)
				if want-out > worst {
					worst, worstAt = want-out, samples
				}
			}
			t.Logf("speed %g: worst shortfall %d samples (%s) at %d input samples", speed, worst, samplesLength(worst, outputRate), worstAt)
		})
	}
}

func TestNewStretcher_NormalSpeed_IsIdentity(t *testing.T) {
	input := speechPCM(t)

	assert.Equal(t, input, stretch(t, input, 1))
}

// prototypeStretch is the WSOLA prototype that passed the blind listening
// test, kept verbatim.
func prototypeStretch(x []float32, rate int, speed float64) []float32 {
	hop := rate / 100
	window := 2 * hop
	search := rate / 200
	n := int(float64(len(x)) / speed)
	y := make([]float32, n+window)
	copy(y, x[:min(window, len(x))])
	for out := hop; out+window <= n; out += hop {
		expected := int(float64(out) * speed)
		lo := max(0, expected-search)
		hi := min(len(x)-window, expected+search)
		if lo > hi {
			break
		}
		best := lo
		bestScore := -math.MaxFloat64
		for j := lo; j <= hi; j += 2 {
			var dot, energy float64
			for k := 0; k < hop; k += 4 {
				a := float64(y[out+k])
				b := float64(x[j+k])
				dot += a * b
				energy += b * b
			}
			score := dot / math.Sqrt(energy+1e-15)
			if score > bestScore {
				bestScore = score
				best = j
			}
		}
		for k := 0; k < hop; k++ {
			w := float32(k) / float32(hop)
			y[out+k] = y[out+k]*(1-w) + x[best+k]*w
		}
		copy(y[out+hop:out+window], x[best+hop:best+window])
	}
	return y[:n]
}

// trackedPrototype is prototypeStretch that also reports where its last
// window ends in the output and in the input.
func trackedPrototype(x []float32, speed float64) (y []float32, lastEnd, segEnd int) {
	rate := outputRate
	hop := rate / 100
	window := 2 * hop
	search := rate / 200
	n := int(float64(len(x)) / speed)
	y = make([]float32, n+window)
	copy(y, x[:min(window, len(x))])
	lastEnd, segEnd = window, window
	for out := hop; out+window <= n; out += hop {
		expected := int(float64(out) * speed)
		lo := max(0, expected-search)
		hi := min(len(x)-window, expected+search)
		if lo > hi {
			break
		}
		best := lo
		bestScore := -math.MaxFloat64
		for j := lo; j <= hi; j += 2 {
			var dot, energy float64
			for k := 0; k < hop; k += 4 {
				a := float64(y[out+k])
				b := float64(x[j+k])
				dot += a * b
				energy += b * b
			}
			score := dot / math.Sqrt(energy+1e-15)
			if score > bestScore {
				bestScore = score
				best = j
			}
		}
		for k := 0; k < hop; k++ {
			w := float32(k) / float32(hop)
			y[out+k] = y[out+k]*(1-w) + x[best+k]*w
		}
		copy(y[out+hop:out+window], x[best+hop:best+window])
		lastEnd, segEnd = out+window, best+window
	}
	return y[:n], lastEnd, segEnd
}

func toUnit(samples []int16) []float32 {
	x := make([]float32, len(samples))
	for i, sample := range samples {
		x[i] = float32(sample) / s16Scale
	}

	return x
}

func toSamples(y []float32) []int16 {
	out := make([]int16, len(y))
	for i, sample := range y {
		out[i] = toS16(sample)
	}

	return out
}

// exactOracle is the prototype up to the end of its last window, then the
// input carried on from the sample after that window, up to the prototype's
// length. Inputs shorter than a window, or too short for one hop of output,
// are cut to that length.
func exactOracle(in []int16, speed float64) []int16 {
	n := int(float64(len(in)) / speed)
	if len(in) < stretchWindow || n < stretchHop {
		return in[:min(n, len(in))]
	}

	y, lastEnd, segEnd := trackedPrototype(toUnit(in), speed)
	if lastEnd >= n {
		return toSamples(y)
	}

	out := toSamples(y[:lastEnd])
	return append(out, in[segEnd:min(len(in), segEnd+n-lastEnd)]...)
}

type readMode int

const (
	wholeReads readMode = iota
	oneByteReads
	dataErrReads
	randomChunkReads
	halfReads
	readModeCount
)

type chunkReader struct {
	r   io.Reader
	rng *rand.Rand
}

func (c *chunkReader) Read(p []byte) (int, error) {
	n := 1 + c.rng.IntN(maxRandomChunkBytes)
	return c.r.Read(p[:min(len(p), n)])
}

func readerFor(pcm []byte, mode readMode) io.Reader {
	r := io.Reader(bytes.NewReader(pcm))
	switch mode {
	case oneByteReads:
		return iotest.OneByteReader(r)
	case dataErrReads:
		return iotest.DataErrReader(r)
	case randomChunkReads:
		return &chunkReader{r: r, rng: rand.New(rand.NewPCG(chunkSeed, chunkSeed))} //nolint:gosec // G404: a reproducible read pattern, not a secret.
	case halfReads:
		return iotest.HalfReader(r)
	}

	return r
}

func stretchWith(t *testing.T, pcm []byte, speed float64, mode readMode) ([]int16, error) {
	t.Helper()

	out, err := io.ReadAll(newStretcher(t, readerFor(pcm, mode), speed))
	if err != nil {
		return nil, err
	}

	return samplesOf(t, out), nil
}

// firstDifference is the first index where want and got differ, counting a
// length mismatch, or -1.
func firstDifference(want, got []int16) int {
	for i := range min(len(want), len(got)) {
		if want[i] != got[i] {
			return i
		}
	}

	if len(want) != len(got) {
		return min(len(want), len(got))
	}

	return -1
}

func oracleSignals(t *testing.T, length int) map[string][]int16 {
	noise := make([]byte, length*outputSampleBytes)
	_, err := rand.NewChaCha8(noiseSeed).Read(noise)
	require.NoError(t, err)
	random := samplesOf(t, noise)

	square := make([]int16, length)
	for i := range square {
		square[i] = math.MaxInt16
		if (i/37)%2 == 1 {
			square[i] = math.MinInt16
		}
	}

	signals := map[string][]int16{"random": random, "silence": make([]int16, length), "full scale": square}
	if speech := samplesOf(t, speechPCM(t)); length <= len(speech) {
		signals["speech"] = speech[:length]
	}

	return signals
}

func TestTrackedPrototype_MatchesPrototype(t *testing.T) {
	x := toUnit(samplesOf(t, speechPCM(t)))

	for _, speed := range oracleSpeeds {
		tracked, _, _ := trackedPrototype(x, speed)

		assert.Equal(t, prototypeStretch(x, outputRate, speed), tracked, "speed %g", speed)
	}
}

func TestNewStretcher_MatchesExactOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: reproducible test lengths.
	lengths := append([]int{}, oracleLengths...)
	for range randomLengthCount {
		lengths = append(lengths, rng.IntN(maxRandomLength))
	}

	for _, length := range lengths {
		for name, in := range oracleSignals(t, length) {
			pcm := pcmOf(t, in)
			for _, speed := range oracleSpeeds {
				want := exactOracle(in, speed)
				for mode := range readModeCount {
					if mode == oneByteReads && length > oneByteMaxSamples {
						continue
					}

					got, err := stretchWith(t, pcm, speed, mode)
					require.NoError(t, err)
					index := firstDifference(want, got)
					require.Equal(t, -1, index, "%s, %d samples, speed %g, read mode %d: first difference at %d (want %d samples, got %d)", name, length, speed, mode, index, len(want), len(got))
				}
			}
		}
	}
}

func FuzzStretcher(f *testing.F) {
	speech := fixture(f, "speech.wav")[wavHeaderBytes:]
	f.Add([]byte{}, uint16(0), uint8(wholeReads))
	f.Add([]byte{1}, uint16(1000), uint8(oneByteReads))
	f.Add(make([]byte, 2*stretchWindow), uint16(math.MaxUint16), uint8(dataErrReads))
	f.Add(speech[:2*outputRate/10], uint16(0), uint8(randomChunkReads))
	f.Add(speech[:outputRate], uint16(math.MaxUint16/4), uint8(halfReads))
	f.Add(speech, uint16(math.MaxUint16/2), uint8(wholeReads))

	f.Fuzz(func(t *testing.T, data []byte, speedStep uint16, modeByte uint8) {
		speed := MinSpeed + (MaxSpeed-MinSpeed)*float64(speedStep)/math.MaxUint16
		mode := readMode(modeByte) % readModeCount

		got, err := stretchWith(t, data, speed, mode)
		if len(data)%outputSampleBytes != 0 {
			require.ErrorIs(t, err, io.ErrUnexpectedEOF, "odd length %d accepted", len(data))
			return
		}
		require.NoError(t, err)

		want := exactOracle(samplesOf(t, data), speed)
		index := firstDifference(want, got)
		require.Equal(t, -1, index, "%d samples, speed %g, read mode %d: first difference at %d (want %d samples, got %d)", len(data)/outputSampleBytes, speed, mode, index, len(want), len(got))
	})
}

func longestZeroRun(samples []int16) int {
	longest, run := 0, 0
	for _, sample := range samples {
		if sample != 0 {
			run = 0
			continue
		}

		run++
		longest = max(longest, run)
	}

	return longest
}

func TestNewStretcher_SlowSpeech_EndsWithTheInputsLastSamples(t *testing.T) {
	input := samplesOf(t, speechPCM(t))
	inputTail := rms(input[len(input)-tailWindowSamples:])
	require.Positive(t, inputTail, "the input must not end in silence")

	for _, speed := range slowSpeeds {
		t.Run(speedName(speed), func(t *testing.T) {
			out := samplesOf(t, stretch(t, speechPCM(t), speed))
			require.Greater(t, len(out), tailCheckSamples)

			outputTail := rms(out[len(out)-tailWindowSamples:])
			zeroRun := longestZeroRun(out[len(out)-tailCheckSamples:])
			t.Logf("speed %g: tail RMS input %.5f, output %.5f (%.0f%%), longest zero run in the last 100 ms %d samples", speed, inputTail, outputTail, 100*outputTail/inputTail, zeroRun)

			assert.GreaterOrEqual(t, outputTail, minTailRMSRatio*inputTail)
			assert.LessOrEqual(t, zeroRun, maxZeroRunSamples)
		})
	}
}

// pausingReader hands out pcm in small chunks and stops after pauseAt bytes
// until release is closed.
type pausingReader struct {
	pcm     []byte
	sent    int
	pauseAt int
	paused  chan struct{}
	release chan struct{}
}

func (r *pausingReader) Read(p []byte) (int, error) {
	if r.sent == r.pauseAt {
		close(r.paused)
		<-r.release
	}

	if r.sent == len(r.pcm) {
		return 0, io.EOF
	}

	end := min(r.sent+streamChunkBytes, len(r.pcm), r.sent+len(p))
	if r.sent < r.pauseAt {
		end = min(end, r.pauseAt)
	}

	n := copy(p, r.pcm[r.sent:end])
	r.sent += n
	return n, nil
}

func TestNewStretcher_StreamsBeforeInputEnds(t *testing.T) {
	source := &pausingReader{
		pcm:     s16le(tone(outputRate, stretchToneSeconds, toneAmplitude)),
		pauseAt: streamPauseBytes,
		paused:  make(chan struct{}),
		release: make(chan struct{}),
	}
	stretcher := newStretcher(t, source, 1.15)

	var streamed atomic.Int64
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, streamChunkBytes)
		for {
			n, err := stretcher.Read(buf)
			streamed.Add(int64(n))
			if err != nil {
				done <- err
				return
			}
		}
	}()

	select {
	case <-source.paused:
	case <-time.After(streamTimeout):
		require.FailNow(t, "the stretcher never read 500 ms of input")
	}

	reached := assert.Eventually(t, func() bool { return streamed.Load() >= minStreamedBytes }, streamTimeout, time.Millisecond)
	t.Logf("output before the input resumed: %d bytes (%s)", streamed.Load(), samplesLength(int(streamed.Load())/outputSampleBytes, outputRate))
	close(source.release)

	require.ErrorIs(t, <-done, io.EOF)
	assert.True(t, reached, "want at least 200 ms of output while the input is paused at 500 ms")
}

type failingReader struct {
	pcm []byte
	err error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if len(r.pcm) == 0 {
		return 0, r.err
	}

	n := copy(p, r.pcm)
	r.pcm = r.pcm[n:]
	return n, nil
}

func TestNewStretcher_SourceErrors_AreReturned(t *testing.T) {
	tests := map[string]struct {
		source io.Reader
		want   error
	}{
		"source error": {&failingReader{pcm: speechPCM(t), err: errSourceBroke}, errSourceBroke},
		"half sample":  {bytes.NewReader(speechPCM(t)[:outputRate+1]), io.ErrUnexpectedEOF},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := io.ReadAll(newStretcher(t, tt.source, 1.5))

			assert.ErrorIs(t, err, tt.want)
		})
	}
}
