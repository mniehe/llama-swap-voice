package fft

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	sampleScale = 32768
	maxSample   = math.MaxInt16
	minSample   = math.MinInt16
	// wsolaBlock and wsolaRef are the lengths internal/audio correlates.
	wsolaBlock = 960
	wsolaRef   = 240
	// provenBound is PROOF.md's bound on |real(h)·2^20 - K| for the
	// general contract, in integer units, rounded up.
	provenBound = 0.017604150813
	// dotUnits and upshift restate the contract, 2^30 integer units per
	// unit dot product and a 2^20 upshift after an unnormalised 1024-point
	// inverse, without the package's constants, so the oracle shares no
	// scaling with the code under test.
	dotUnits = 0x1p30
	upshift  = 0x1p20
)

// deviation tracks the largest |real(h)·2^20 - K| any test observed.
var deviation struct {
	sync.Mutex
	worst float64
}

func observe(d float64) {
	deviation.Lock()
	defer deviation.Unlock()
	deviation.worst = max(deviation.worst, d)
}

func TestMain(m *testing.M) {
	code := m.Run()
	if testing.Verbose() {
		fmt.Printf("largest observed |real(h)·2^20 - K|: %.6g integer units; proven bound %g\n", deviation.worst, provenBound)
	}
	os.Exit(code)
}

func toUnit(samples []int16) []float64 {
	out := make([]float64, len(samples))
	for i, s := range samples {
		out[i] = float64(s) / sampleScale
	}
	return out
}

func oracle(block, ref []int16) []int64 {
	dots := make([]int64, len(block)-len(ref)+1)
	for k := range dots {
		var sum int64
		for i, r := range ref {
			sum += int64(block[k+i]) * int64(r)
		}
		dots[k] = sum
	}
	return dots
}

// checkExact correlates block with ref and requires every lag to equal the
// integer oracle, a zero dot to be +0 as the direct sum gives, and the
// unrounded transform output to lie within PROOF.md's bound.
func checkExact(t *testing.T, block, ref []int16) {
	t.Helper()

	want := oracle(block, ref)
	dots := make([]float64, len(want))
	require.NoErrorf(t, Correlate(dots, toUnit(block), toUnit(ref)), "block %d, reference %d", len(block), len(ref))

	// An empty reference uses no transform output.
	if len(ref) > 0 {
		var scaled [Size]float64
		correlate(&scaled, toUnit(block), toUnit(ref))
		worst := 0.0
		for k := range len(want) {
			worst = max(worst, math.Abs(scaled[k]*upshift-float64(want[k])))
		}
		observe(worst)
		require.LessOrEqualf(t, worst, provenBound, "block %d, reference %d: unrounded error over the proven bound", len(block), len(ref))
	}

	// Lags are compared in plain Go, not one assertion each, because the
	// exhaustive tests check millions of them.
	for k, w := range want {
		wantDot := float64(w) / dotUnits
		if dots[k] != wantDot || (dots[k] == 0 && math.Signbit(dots[k])) {
			require.Failf(t, "inexact lag", "block %d, reference %d, lag %d: got %v (K %v), want K %d", len(block), len(ref), k, dots[k], dots[k]*dotUnits, w)
		}
	}
}

func filled(n int, v int16) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func alternating(n int, first int16) []int16 {
	out := make([]int16, n)
	for i := range out {
		if i%2 == 0 {
			out[i] = first
		} else {
			out[i] = ^first
		}
	}
	return out
}

func seeded(a, b uint64) *rand.Rand {
	return rand.New(rand.NewPCG(a, b)) //nolint:gosec // G404: reproducible test signals, not a secret.
}

func randomPCM(rng *rand.Rand, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(rng.IntN(1<<16) + minSample) //nolint:gosec // G115: IntN(1<<16)+MinInt16 lies in the int16 range.
	}
	return out
}

func TestCorrelate_FullScalePatterns_AreExact(t *testing.T) {
	patterns := map[string][2][]int16{
		"max by min":           {filled(Size, maxSample), filled(Size, minSample)},
		"min by min":           {filled(Size, minSample), filled(Size, minSample)},
		"max by max wsola":     {filled(wsolaBlock, maxSample), filled(wsolaRef, maxSample)},
		"min by max wsola":     {filled(wsolaBlock, minSample), filled(wsolaRef, maxSample)},
		"constant min wsola":   {filled(wsolaBlock, minSample), filled(wsolaRef, minSample)},
		"dc":                   {filled(wsolaBlock, 1000), filled(wsolaRef, 1000)},
		"alternating":          {alternating(Size, maxSample), alternating(Size/2, minSample)},
		"alternating by dc":    {alternating(wsolaBlock, minSample), filled(wsolaRef, maxSample)},
		"silence":              {make([]int16, wsolaBlock), make([]int16, wsolaRef)},
		"silence by full":      {make([]int16, Size), filled(1, minSample)},
		"full ref full length": {filled(Size, maxSample), alternating(Size, minSample)},
	}
	for name, p := range patterns {
		t.Run(name, func(t *testing.T) {
			checkExact(t, p[0], p[1])
		})
	}
}

// Cancellation: a reference whose halves cancel against a constant block
// sums large terms to exactly zero at every lag.
func TestCorrelate_Cancellation_IsExactZero(t *testing.T) {
	ref := append(filled(wsolaRef/2, maxSample), filled(wsolaRef/2, -maxSample)...)
	block := append(filled(wsolaBlock/2, minSample), filled(wsolaBlock/2, maxSample)...)
	checkExact(t, filled(wsolaBlock, minSample), ref)
	checkExact(t, block, ref)
}

// An impulse at block position p and reference position q correlates only
// at lag p-q, which pins the sign of the lag at both ends of the range.
func TestCorrelate_ImpulsePairs_PinTheLag(t *testing.T) {
	for _, lengths := range [][2]int{{wsolaBlock, wsolaRef}, {Size, 1}, {Size, Size}, {Size, Size / 2}} {
		lags := lengths[0] - lengths[1]
		for _, lag := range []int{0, lags} {
			block, ref := make([]int16, lengths[0]), make([]int16, lengths[1])
			ref[0], block[lag] = minSample, maxSample
			checkExact(t, block, ref)

			dots := make([]float64, lags+1)
			require.NoError(t, Correlate(dots, toUnit(block), toUnit(ref)))
			for k, d := range dots {
				require.Equalf(t, k == lag, d != 0, "lengths %v, impulse at lag %d: dot %v at lag %d", lengths, lag, d, k)
			}
		}
	}
}

func TestCorrelate_EveryLength_IsExact(t *testing.T) {
	rng := seeded(1, 2)
	for blockLen := wsolaRef; blockLen <= Size; blockLen++ {
		block := randomPCM(rng, blockLen)
		for _, refLen := range []int{1, 2, wsolaRef, blockLen / 2, blockLen - 1, blockLen} {
			checkExact(t, block, randomPCM(rng, refLen))
		}
	}
	for _, blockLen := range []int{wsolaRef, wsolaBlock, Size} {
		block := randomPCM(rng, blockLen)
		for refLen := 1; refLen <= blockLen; refLen++ {
			checkExact(t, block, randomPCM(rng, refLen))
		}
	}
}

func TestCorrelate_RandomPCM_IsExact(t *testing.T) {
	rng := seeded(3, 4)
	for range 200 {
		checkExact(t, randomPCM(rng, wsolaBlock), randomPCM(rng, wsolaRef))
	}
	for range 50 {
		checkExact(t, randomPCM(rng, Size), randomPCM(rng, Size/2))
	}
}

// An empty reference makes every dot product the empty sum, +0, at each of
// len(block)+1 lags.
func TestCorrelate_EmptyReference_ZeroesEveryLag(t *testing.T) {
	for _, blockLen := range []int{0, Size - 1, Size} {
		t.Run(strconv.Itoa(blockLen), func(t *testing.T) {
			dots := make([]float64, blockLen+2)
			for k := range dots {
				dots[k] = 42
			}
			block := toUnit(filled(blockLen, maxSample))

			require.NoError(t, Correlate(dots, block, nil))

			for k := range blockLen + 1 {
				require.Zerof(t, dots[k], "lag %d", k)
				require.Falsef(t, math.Signbit(dots[k]), "lag %d is -0", k)
			}
			require.InDelta(t, 42, dots[blockLen+1], 0, "the element past the last lag changed")
		})
	}
}

func TestCorrelate_LengthsOutsideContract_ReturnErrLength(t *testing.T) {
	cases := map[string][3]int{
		"block over size":      {1, Size + 1, 1},
		"reference over block": {1, 4, 5},
		"dots too short":       {wsolaBlock - wsolaRef, wsolaBlock, wsolaRef},
	}
	for name, lengths := range cases {
		t.Run(name, func(t *testing.T) {
			dots := make([]float64, lengths[0])
			dots[0] = -1
			err := Correlate(dots, make([]float64, lengths[1]), make([]float64, lengths[2]))
			require.ErrorIs(t, err, ErrLength)
			require.InDelta(t, -1, dots[0], 0, "dots changed on error")
		})
	}
}

func FuzzCorrelate(f *testing.F) {
	f.Add(uint16(wsolaBlock), uint16(wsolaRef), []byte{0xff, 0x7f, 0x00, 0x80})
	f.Add(uint16(Size), uint16(0), []byte{0xff, 0x7f})
	f.Add(uint16(Size), uint16(Size), []byte{0x00, 0x80})
	f.Add(uint16(0), uint16(0), []byte{})
	f.Add(uint16(1), uint16(1), []byte{0x00, 0x80})

	// Lengths in range map to themselves, so each seed runs as written.
	f.Fuzz(func(t *testing.T, blockLen, refLen uint16, data []byte) {
		lb := int(blockLen) % (Size + 1)
		lr := int(refLen) % (lb + 1)
		samples := make([]int16, lb+lr)
		for i := range samples {
			if 2*i+1 < len(data) {
				samples[i] = int16(binary.LittleEndian.Uint16(data[2*i:])) //nolint:gosec // G115: reinterprets 16 bits as a PCM sample.
			} else if len(data) > 0 {
				samples[i] = int16(data[i%len(data)]) << 8
			}
		}
		checkExact(t, samples[:lb], samples[lb:])
	})
}

func BenchmarkCorrelate(b *testing.B) {
	rng := seeded(5, 6)
	block, ref := toUnit(randomPCM(rng, wsolaBlock)), toUnit(randomPCM(rng, wsolaRef))
	dots := make([]float64, wsolaBlock-wsolaRef+1)
	for b.Loop() {
		if err := Correlate(dots, block, ref); err != nil {
			require.NoError(b, err)
		}
	}
}

// directScan is internal/audio's direct scan: eight sums side by side over
// local arrays.
func directScan(dots, block *[Size]float64, ref *[wsolaRef]float64, lags int) {
	for offset := range lags {
		var l0, l1, l2, l3, l4, l5, l6, l7 float64
		for k := 0; k <= wsolaRef-8; k += 8 {
			l0 += ref[k] * block[offset+k]
			l1 += ref[k+1] * block[offset+k+1]
			l2 += ref[k+2] * block[offset+k+2]
			l3 += ref[k+3] * block[offset+k+3]
			l4 += ref[k+4] * block[offset+k+4]
			l5 += ref[k+5] * block[offset+k+5]
			l6 += ref[k+6] * block[offset+k+6]
			l7 += ref[k+7] * block[offset+k+7]
		}
		dots[offset] = (l0 + l1) + (l2 + l3) + (l4 + l5) + (l6 + l7)
	}
}

// BenchmarkScan times both scans at each lag count, to find where the
// transform's fixed cost undercuts the direct scan's cost per lag.
func BenchmarkScan(b *testing.B) {
	rng := seeded(7, 8)
	var block [Size]float64
	var ref [wsolaRef]float64
	copy(block[:], toUnit(randomPCM(rng, wsolaBlock)))
	copy(ref[:], toUnit(randomPCM(rng, wsolaRef)))
	for _, lags := range []int{8, 64, 128, 256, 320, 352, 384, 416, 448, 512, wsolaBlock - wsolaRef + 1} {
		b.Run("direct/lags="+strconv.Itoa(lags), func(b *testing.B) {
			var dots [Size]float64
			for b.Loop() {
				directScan(&dots, &block, &ref, lags)
			}
		})
		b.Run("fft/lags="+strconv.Itoa(lags), func(b *testing.B) {
			var dots [Size]float64
			for b.Loop() {
				if err := Correlate(dots[:lags], block[:lags-1+wsolaRef], ref[:]); err != nil {
					require.NoError(b, err)
				}
			}
		})
	}
}
