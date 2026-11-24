// Package fft computes exact cross-correlations of short integer signals
// with a fixed-size radix-2 floating-point FFT.
//
// Inputs must be integer multiples of 2^-15 in [-1, 1], as 16-bit PCM
// scaled by 1/32768 is. Each true dot product is then an integer multiple
// of 2^-30, and PROOF.md bounds the transform's error below 0.0177 of
// that step for every input the contract allows, so rounding to the
// nearest step recovers each dot product exactly. The bound holds only for the operation graph,
// twiddle table and rounding written here; PROOF.md lists what may not
// change without redoing it.
package fft

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
)

//go:generate go run gentwiddle/main.go

// Size is the transform length, and the most samples a block may hold.
const Size = 1024

const (
	log2Size = 10
	// spectrumScale undoes the transform's gain of Size and the inputs'
	// 2^-15 scale on both factors in one exact power-of-two step:
	// 2^30 / Size = 2^20.
	spectrumScale = 0x1p20
	dotStep       = 0x1p-30
)

// Correlate runs three transforms: the block's, the reference's, then the
// inverse of their product.
const (
	blockPass = iota
	refPass
	inversePass
	passCount
)

// ErrLength reports lengths outside Correlate's contract.
var ErrLength = errors.New("fft: lengths outside the correlation contract")

var (
	bitReversed = reversal()
	conjugates  = conjugate(&roots)
)

func reversal() *[Size]int {
	var table [Size]int
	for i := range table {
		table[i] = int(bits.Reverse32(uint32(i)) >> (32 - log2Size)) //nolint:gosec // G115: i < Size = 2^10 fits in 32 bits.
	}
	return &table
}

// conjugate negates the imaginary parts, which is exact, so the inverse
// transform's roots are the exact conjugates of the forward roots.
func conjugate(table *[Size / 2][2]float64) *[Size / 2][2]float64 {
	var conj [Size / 2][2]float64
	for k, w := range table {
		conj[k] = [2]float64{w[0], -w[1]}
	}
	return &conj
}

// Correlate sets dots[k] = Σ_i block[k+i]·ref[i], summed over i in
// [0, len(ref)), for each lag k in [0, len(block)-len(ref)], exactly.
//
// Every element of block and ref must be an integer multiple of 2^-15 with
// magnitude at most 1; this is not checked, and other inputs get no
// exactness guarantee. len(block) may be at most Size, len(ref) at most
// len(block), and dots must hold at least len(block)-len(ref)+1 elements.
// Other lengths return ErrLength and leave dots unchanged. Correlate keeps
// no state and is safe for concurrent use.
func Correlate(dots, block, ref []float64) error {
	if len(block) > Size || len(ref) > len(block) || len(dots) < len(block)-len(ref)+1 {
		return fmt.Errorf("%w: block %d, reference %d, dots %d, size %d", ErrLength, len(block), len(ref), len(dots), Size)
	}

	var scaled [Size]float64
	correlate(&scaled, block, ref)
	for k := range len(block) - len(ref) + 1 {
		dots[k] = float64(int64(math.Round(scaled[k]*spectrumScale))) * dotStep
	}

	return nil
}

// correlate sets scaled to the real part of Size times the circular
// cross-correlation of block and ref.
//
// The radix-2 decimation-in-time transform runs three times, written once
// over local arrays rather than in a function of its own: the race
// detector instruments every read through a pointer but not reads of a
// local array, and the transform's reads through pointers doubled the
// audio package's race tests. The block's spectrum is set aside while the
// reference's is computed, and the roots are copied in, for the same
// reason.
//
// Every product is converted to float64 before it is added, because Go
// may otherwise fuse a multiply and an add into one FMA, which the error
// bound in PROOF.md does not cover.
func correlate(scaled *[Size]float64, block, ref []float64) {
	var re, im, blockRe, blockIm [Size]float64
	var w [Size / 2][2]float64
	load(&re, block)

	for pass := range passCount {
		switch pass {
		case blockPass:
			w = roots
		case refPass:
			blockRe, blockIm = re, im
			im = [Size]float64{}
			load(&re, ref)
		case inversePass:
			multiplyConjugate(&re, &im, &blockRe, &blockIm)
			w = *conjugates
		}

		for span := 1; span < Size; span <<= 1 {
			stride := Size / (2 * span)
			for start := 0; start < Size; start += 2 * span {
				for j := range span {
					root := w[j*stride]
					top, bottom := start+j, start+j+span
					productRe := float64(float64(root[0]*re[bottom]) - float64(root[1]*im[bottom]))
					productIm := float64(float64(root[0]*im[bottom]) + float64(root[1]*re[bottom]))
					topRe, topIm := re[top], im[top]
					re[top], im[top] = topRe+productRe, topIm+productIm
					re[bottom], im[bottom] = topRe-productRe, topIm-productIm
				}
			}
		}
	}

	*scaled = re
}

// load sets re to samples, zero-padded to Size, in bit-reversed order.
func load(re *[Size]float64, samples []float64) {
	*re = [Size]float64{}
	for i, v := range samples {
		re[bitReversed[i]] = v
	}
}

// multiplyConjugate sets y to x·conj(y), element by element, then permutes
// it into bit-reversed order for the inverse transform.
func multiplyConjugate(yRe, yIm, xRe, xIm *[Size]float64) {
	for k := range Size {
		a, b, c, d := xRe[k], xIm[k], yRe[k], yIm[k]
		yRe[k] = float64(float64(a*c) + float64(b*d))
		yIm[k] = float64(float64(b*c) - float64(a*d))
	}

	for i, j := range bitReversed {
		if i < j {
			yRe[i], yRe[j] = yRe[j], yRe[i]
			yIm[i], yIm[j] = yIm[j], yIm[i]
		}
	}
}
