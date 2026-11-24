//go:build !race

// The race detector skips these: they run the exact oracle and 60 s of
// speech a second time, with go-fft, which costs more than two minutes on two
// cores under it, and each adapter runs in one goroutine, so it has no race
// to find.

package audio

import (
	"bytes"
	"crypto/sha256"
	"io"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashSpeeds are the speeds whose output on the 60 s speech input must be
// the same with either correlator.
var hashSpeeds = []float64{0.25, 1.10, 1.15, 1.20}

// goFFTCalls counts the go-fft adapter's calls and failures across every
// stretcher a test builds. A failed call falls back to the exact direct
// scan, so the output alone cannot show that go-fft was used.
type goFFTCalls struct {
	calls, failures atomic.Int64
}

func (c *goFFTCalls) stretcher(r io.Reader, speed float64) (io.Reader, error) {
	correlate := goFFTCorrelate()
	return newStretcherWith(r, speed, func(dots, block, ref []float64) error {
		c.calls.Add(1)
		err := correlate(dots, block, ref)
		if err != nil {
			c.failures.Add(1)
		}
		return err
	})
}

func (c *goFFTCalls) assertAllSucceeded(t *testing.T) {
	t.Helper()

	calls, failures := c.calls.Load(), c.failures.Load()
	assert.Positive(t, calls, "no wide search called go-fft")
	assert.Zero(t, failures, "go-fft failed %d of %d calls", failures, calls)
}

func TestGoFFTCorrelator_MatchesExactOracle(t *testing.T) {
	t.Parallel()

	var counted goFFTCalls
	t.Run("cases", func(t *testing.T) {
		runExactOracle(t, counted.stretcher)
	})

	counted.assertAllSucceeded(t)
}

func TestGoFFTCorrelator_Speech60s_MatchesInternal(t *testing.T) {
	input := speech60s(t)

	for _, speed := range hashSpeeds {
		t.Run(speedName(speed), func(t *testing.T) {
			t.Parallel()
			var counted goFFTCalls
			want := stretchAll(t, input, speed, NewStretcher)
			got := stretchAll(t, input, speed, counted.stretcher)
			t.Logf("speed %g: internal %x, go-fft %x", speed, sha256.Sum256(want), sha256.Sum256(got))

			assert.Equal(t, sha256.Sum256(want), sha256.Sum256(got))
			counted.assertAllSucceeded(t)
		})
	}
}

func stretchAll(t *testing.T, input []byte, speed float64, factory stretcherFactory) []byte {
	t.Helper()

	stretcher, err := factory(bytes.NewReader(input), speed)
	require.NoError(t, err)
	out, err := io.ReadAll(stretcher)
	require.NoError(t, err)

	return out
}
