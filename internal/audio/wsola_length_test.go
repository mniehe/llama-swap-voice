package audio

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Input shorter than one window is not stretched: the reply is the input cut
// to input/speed samples, so a slow speed returns all of it and a fast one
// the start. From one window on, the reply is stretched to input/speed.
func TestNewStretcher_OneWindowBoundary_PinsTheLength(t *testing.T) {
	input := s16le(tone(outputRate, stretchToneSeconds, toneAmplitude))

	tests := []struct {
		samples int
		speed   float64
		want    int
	}{
		{samples: stretchWindow - 1, speed: 0.5, want: stretchWindow - 1},
		{samples: stretchWindow, speed: 0.5, want: 2 * stretchWindow},
		{samples: stretchWindow - 1, speed: 2, want: stretchWindow/2 - 1},
		{samples: stretchWindow, speed: 2, want: stretchWindow / 2},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.samples)+"@"+speedName(tt.speed), func(t *testing.T) {
			pcm := input[:tt.samples*outputSampleBytes]

			out := stretch(t, pcm, tt.speed)

			assert.Len(t, out, tt.want*outputSampleBytes)
			if tt.samples < stretchWindow {
				assert.Equal(t, pcm[:len(out)], out, "input shorter than one window is cut, not stretched")
			}
		})
	}
}
