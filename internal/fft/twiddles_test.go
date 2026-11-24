package fft

import (
	"math"
	"math/big"
	"strconv"
	"testing"
)

// The certificate recomputes every root by a route independent of
// gentwiddle: a different arctangent formula for π, a finer precision, and
// the Taylor series over the whole angle with no reduction by symmetry.
const (
	certPrecision = 512
	certTailBits  = 480
	halfUlpBits   = 53
	quarterRoot   = Size / 4
)

type bounds struct {
	lo, hi *big.Float
}

func certFloat(mode big.RoundingMode) *big.Float {
	return new(big.Float).SetPrec(certPrecision).SetMode(mode)
}

func certExact(v float64) bounds {
	return bounds{certFloat(big.ToNegativeInf).SetFloat64(v), certFloat(big.ToPositiveInf).SetFloat64(v)}
}

func (a bounds) plus(b bounds) bounds {
	return bounds{certFloat(big.ToNegativeInf).Add(a.lo, b.lo), certFloat(big.ToPositiveInf).Add(a.hi, b.hi)}
}

func (a bounds) minus(b bounds) bounds {
	return bounds{certFloat(big.ToNegativeInf).Sub(a.lo, b.hi), certFloat(big.ToPositiveInf).Sub(a.hi, b.lo)}
}

// timesNonNegative multiplies two bounds that lie in [0, ∞).
func (a bounds) timesNonNegative(b bounds) bounds {
	return bounds{certFloat(big.ToNegativeInf).Mul(a.lo, b.lo), certFloat(big.ToPositiveInf).Mul(a.hi, b.hi)}
}

func (a bounds) over(d int64) bounds {
	den := new(big.Float).SetInt64(d)
	return bounds{certFloat(big.ToNegativeInf).Quo(a.lo, den), certFloat(big.ToPositiveInf).Quo(a.hi, den)}
}

func (a bounds) within(e *big.Float) bounds {
	return bounds{certFloat(big.ToNegativeInf).Sub(a.lo, e), certFloat(big.ToPositiveInf).Add(a.hi, e)}
}

func tiny(x *big.Float) bool {
	return x.Sign() == 0 || x.MantExp(nil) < -certTailBits
}

// certArctan encloses arctan(1/m); its alternating, shrinking terms bound
// the error after any term by the next.
func certArctan(m int64) bounds {
	power := certExact(1).over(m)
	sum := certExact(0)
	for j := int64(0); ; j++ {
		term := power.over(2*j + 1)
		if tiny(term.hi) {
			return sum.within(term.hi)
		}
		if j%2 == 0 {
			sum = sum.plus(term)
		} else {
			sum = sum.minus(term)
		}
		power = power.over(m * m)
	}
}

// certPi uses Gauss's formula, π = 48 arctan(1/18) + 32 arctan(1/57) -
// 20 arctan(1/239).
func certPi() bounds {
	return certArctan(18).timesNonNegative(certExact(48)).
		plus(certArctan(57).timesNonNegative(certExact(32))).
		minus(certArctan(239).timesNonNegative(certExact(20)))
}

// certSinCos encloses sin and cos of x in [0, π] by the Taylor series and
// the Lagrange remainder, which is at most the next term's magnitude.
func certSinCos(x bounds) (sin, cos bounds) {
	sin, cos = certExact(0), certExact(0)
	term := certExact(1)
	for n := int64(0); ; n++ {
		if tiny(term.hi) {
			return sin.within(term.hi), cos.within(term.hi)
		}
		sum := &cos
		if n%2 == 1 {
			sum = &sin
		}
		if (n/2)%2 == 0 {
			*sum = sum.plus(term)
		} else {
			*sum = sum.minus(term)
		}
		term = term.timesNonNegative(x).over(n + 1)
	}
}

// halfUlp is half the spacing of binary64 values in v's binade, 2^-54·2^e
// for 2^e <= |v| < 2^(e+1).
func halfUlp(v float64) *big.Float {
	_, exp := math.Frexp(v)
	return new(big.Float).SetMantExp(big.NewFloat(1), exp-1-halfUlpBits)
}

// certify checks stored against the exact value inside want and returns an
// upper bound on |stored - exact|.
func certify(t *testing.T, name string, stored float64, want bounds) *big.Float {
	t.Helper()

	lo, _ := want.lo.Float64()
	hi, _ := want.hi.Float64()
	if lo != stored || hi != stored {
		t.Errorf("%s: stored %x, exact value lies in [%s, %s], which rounds to [%x, %x]", name, stored, want.lo.Text('g', 30), want.hi.Text('g', 30), lo, hi)
	}

	s := new(big.Float).SetFloat64(stored)
	below := certFloat(big.ToPositiveInf).Sub(s, want.lo)
	above := certFloat(big.ToPositiveInf).Sub(want.hi, s)
	worst := below
	if above.Cmp(below) > 0 {
		worst = above
	}
	if stored != 0 && worst.Cmp(halfUlp(stored)) > 0 {
		t.Errorf("%s: |stored - exact| may reach %s, over half an ulp %s", name, worst.Text('g', 10), halfUlp(stored).Text('g', 10))
	}

	return worst
}

func TestRoots_AreCorrectlyRounded(t *testing.T) {
	pi := certPi()
	u := new(big.Float).SetMantExp(big.NewFloat(1), -53)
	betaSquared := certFloat(big.ToPositiveInf).Quo(certFloat(big.ToPositiveInf).Mul(u, u), big.NewFloat(2))
	worstRatio := 0.0

	for k, root := range roots {
		angle := pi.timesNonNegative(certExact(float64(k))).over(Size / 2)
		sin, cos := certSinCos(angle)
		if k == quarterRoot {
			// cos(π/2) is exactly zero, which no enclosure can certify.
			cos = certExact(0)
		}

		reErr := certify(t, "Re ω^"+strconv.Itoa(k), root[0], cos)
		imErr := certify(t, "Im ω^"+strconv.Itoa(k), root[1], bounds{new(big.Float).Neg(sin.hi), new(big.Float).Neg(sin.lo)})

		squared := certFloat(big.ToPositiveInf).Add(
			certFloat(big.ToPositiveInf).Mul(reErr, reErr),
			certFloat(big.ToPositiveInf).Mul(imErr, imErr))
		if squared.Cmp(betaSquared) > 0 {
			t.Errorf("ω^%d: |stored - exact| may reach %s, over u/√2", k, squared.Sqrt(squared).Text('g', 10))
		}
		ratio, _ := certFloat(big.ToPositiveInf).Quo(squared, certFloat(big.ToNearestEven).Mul(u, u)).Float64()
		worstRatio = max(worstRatio, math.Sqrt(ratio))
	}

	t.Logf("largest root error bound: %.6f u (u/√2 = %.6f u)", worstRatio, 1/math.Sqrt2)
}
