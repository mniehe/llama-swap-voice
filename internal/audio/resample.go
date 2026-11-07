package audio

import "math"

// A Blackman-windowed sinc with 64 taps and 1024 fractional phases measured
// 40 dB SNR against ffmpeg's resampler on speech.
const (
	kernelTaps    = 64
	kernelPhases  = 1024
	kernelCentre  = kernelTaps/2 - 1
	kernelReach   = kernelTaps - kernelCentre - 1
	cutoffMargin  = 0.94
	blackmanA0    = 0.42
	blackmanA1    = 0.5
	blackmanA2    = 0.08
	blackmanTerms = kernelTaps - 1
)

type kernel [kernelPhases][kernelTaps]float32

// resampler converts a stream of mono samples from one rate to another as
// they arrive, holding back only the input its next output still needs.
type resampler struct {
	inRate  int
	outRate int
	weights *kernel
	pending []float32
	first   int
	next    int
}

func newResampler(inRate, outRate int) *resampler {
	r := &resampler{inRate: inRate, outRate: outRate}
	if inRate != outRate {
		r.weights = sincKernel(inRate, outRate)
	}

	return r
}

func sincKernel(inRate, outRate int) *kernel {
	cutoff := math.Min(1, float64(outRate)/float64(inRate)) * cutoffMargin
	weights := &kernel{}
	for phase := range weights {
		var sum float64
		var taps [kernelTaps]float64
		for tap := range taps {
			distance := float64(tap-kernelCentre) - float64(phase)/kernelPhases
			taps[tap] = sinc(cutoff, distance) * blackman(tap)
			sum += taps[tap]
		}

		for tap := range taps {
			weights[phase][tap] = float32(taps[tap] / sum)
		}
	}

	return weights
}

func sinc(cutoff, distance float64) float64 {
	if distance == 0 {
		return cutoff
	}

	return math.Sin(math.Pi*cutoff*distance) / (math.Pi * distance)
}

func blackman(tap int) float64 {
	position := float64(tap) / blackmanTerms
	return blackmanA0 - blackmanA1*math.Cos(2*math.Pi*position) + blackmanA2*math.Cos(4*math.Pi*position)
}

func (r *resampler) push(out []int16, samples []float32) []int16 {
	if r.weights == nil {
		for _, sample := range samples {
			out = append(out, toS16(sample))
		}
		return out
	}

	r.pending = append(r.pending, samples...)
	received := r.first + len(r.pending)
	for {
		base, phase := r.position(r.next)
		if base+kernelReach >= received {
			break
		}

		out = append(out, toS16(r.convolve(base, phase)))
		r.next++
	}

	r.discardConsumed()
	return out
}

// flush emits the outputs that need input past the end, treating it as silence.
func (r *resampler) flush(out []int16) []int16 {
	if r.weights == nil {
		return out
	}

	received := r.first + len(r.pending)
	total := received * r.outRate / r.inRate
	for ; r.next < total; r.next++ {
		base, phase := r.position(r.next)
		out = append(out, toS16(r.convolve(base, phase)))
	}

	return out
}

func (r *resampler) position(output int) (base, phase int) {
	scaled := output * r.inRate
	base = scaled / r.outRate
	phase = scaled % r.outRate * kernelPhases / r.outRate
	return base, phase
}

func (r *resampler) convolve(base, phase int) float32 {
	var sum float32
	for tap, weight := range r.weights[phase] {
		index := base + tap - kernelCentre - r.first
		if index < 0 || index >= len(r.pending) {
			continue
		}
		sum += r.pending[index] * weight
	}

	return sum
}

func (r *resampler) discardConsumed() {
	base, _ := r.position(r.next)
	consumed := min(base-kernelCentre-r.first, len(r.pending))
	if consumed <= 0 {
		return
	}

	kept := copy(r.pending, r.pending[consumed:])
	r.pending = r.pending[:kept]
	r.first += consumed
}
