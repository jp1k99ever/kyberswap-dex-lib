package everlongcvamm

import (
	"math/big"

	"github.com/holiman/uint256"
)

// Exact port of CvammFeeLib for the UNSATURATED regime. The bound in fee_law.go is safe but
// conservative: off the cap it substitutes MidFee for a base the venue prices lower, which
// understates a revisit by the whole gap (58-59 bps measured).
//
// The law's one unobservable input is `rv` (realized variance) — no getter on the ALM. But it
// reaches the fee ONLY through sigma/sigmaRef, a scalar a swap does not move, so it never has
// to be read: it is SOLVED from the fee the tracker already samples each block, then reused at
// the post-fill state where everything else (reserves, spot, dislocation) is known.

var (
	bigOne   = big.NewInt(1)
	bigQ192  = new(big.Int).Lsh(big.NewInt(1), 192)
	bigWadSq = new(big.Int).Mul(bigWadFee, bigWadFee)
)

// cWadSigned = CvammCurve.cWad: 1/(2A) - 1, signed and negative for every live A.
func cWadSigned(aWad *big.Int) *big.Int {
	twoA := new(big.Int).Mul(big.NewInt(2), aWad)
	if twoA.Sign() == 0 {
		return nil
	}
	return new(big.Int).Sub(new(big.Int).Quo(bigWadSq, twoA), bigWadFee)
}

// priceAtXWad = CvammCurve.priceAtX: the curve's marginal price at coordinate x.
func priceAtXWad(xWad, aWad *big.Int) *big.Int {
	xu, of1 := uint256.FromBig(xWad)
	au, of2 := uint256.FromBig(aWad)
	if of1 || of2 {
		return nil
	}
	var yu uint256.Int
	if err := yAtX(&yu, xu, au); err != nil {
		return nil
	}
	y := yu.ToBig()
	c := cWadSigned(aWad)
	if c == nil {
		return nil
	}
	num := new(big.Int).Mul(y, new(big.Int).Add(new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(2), xWad), y), c))
	den := new(big.Int).Mul(xWad, new(big.Int).Add(new(big.Int).Add(
		xWad, new(big.Int).Mul(big.NewInt(2), y)), c))
	if num.Sign() <= 0 || den.Sign() <= 0 {
		return nil
	}
	return mulDivFloor(num, bigWadFee, den)
}

// spotRawWad = CvammFeeLib.spotRawWad(CvammCurve.sqrtX96At(...)): the live marginal price in
// the frame the anchor uses. Kept stepwise so each floor lands where Solidity puts it.
func spotRawWad(anchorSqrtX96, xWad, aWad *big.Int) *big.Int {
	p := priceAtXWad(xWad, aWad)
	if p == nil {
		return nil
	}
	root := new(big.Int).Sqrt(new(big.Int).Mul(p, bigWadFee))
	s := mulDivFloor(anchorSqrtX96, root, bigWadFee)
	// The curve reverts outside the uint160 sqrt-price envelope, taking the whole fee
	// sample with it. Declining here keeps the port refusing exactly where the venue does.
	if s.Sign() == 0 || s.BitLen() > 160 {
		return nil
	}
	return mulDivFloor(s, new(big.Int).Mul(s, bigWadFee), bigQ192)
}

// logRatioAbsWad = RepegMath.logRatioAbsWad: |ln(a/b)| in WAD, 0 when the ratio floors away.
func logRatioAbsWad(aWad, bWad *big.Int) *big.Int {
	if aWad.Sign() == 0 || bWad.Sign() == 0 {
		return new(big.Int)
	}
	ratio := mulDivFloor(aWad, bigWadFee, bWad)
	if ratio.Sign() == 0 {
		return new(big.Int)
	}
	r, err := lnWadBI(ratio)
	if err != nil {
		return nil
	}
	return new(big.Int).Abs(r)
}

// reductionG = CvammFeeLib._reductionG: (g, volatile value weight). g == 0 marks a degenerate
// or one-sided book, where the law returns the imbalanced fee instead of interpolating.
func reductionG(e *Extra, reserveStable, reserveVolatile, curvature *big.Int) (*big.Int, *big.Int) {
	vv := mulDivFloor(reserveVolatile, e.ReservationPriceWad.ToBig(), bigWadFee)
	total := new(big.Int).Add(reserveStable, vv)
	if reserveStable.Sign() == 0 || vv.Sign() == 0 || total.Sign() == 0 {
		return new(big.Int), new(big.Int)
	}
	k := mulDivFloor(mulDivFloor(new(big.Int).Mul(big.NewInt(4), reserveStable), bigWadFee, total), vv, total)
	gk := mulDivFloor(curvature, k, bigWadFee)
	denom := new(big.Int).Sub(new(big.Int).Add(gk, bigWadFee), k)
	if denom.Sign() <= 0 {
		return new(big.Int), new(big.Int)
	}
	return mulDivFloor(gk, bigWadFee, denom), mulDivFloor(vv, bigWadFee, total)
}
func addBI(x, y *big.Int) *big.Int {
	return new(big.Int).Add(x, y)
}
func subBI(x, y *big.Int) *big.Int {
	return new(big.Int).Sub(x, y)
}
func mulBI(x, y *big.Int) *big.Int {
	return new(big.Int).Mul(x, y)
}
func sdivBI(x, y *big.Int) *big.Int {
	return new(big.Int).Quo(x, y)
}
func sarBI(x *big.Int, shift uint) *big.Int {
	return new(big.Int).Rsh(x, shift)
}

// Solady lnWad's rational-approximation constants, parsed once.
var (
	lnC0        = mustBI("43456485725739037958740375743393")
	lnC1        = mustBI("24828157081833163892658089445524")
	lnC2        = mustBI("3273285459638523848632254066296")
	lnC3        = mustBI("11111509109440967052023855526967")
	lnC4        = mustBI("45023709667254063763336534515857")
	lnC5        = mustBI("14706773417378608786704636184526")
	lnC6        = mustBI("795164235651350426258249787498")
	lnC7        = mustBI("5573035233440673466300451813936")
	lnC8        = mustBI("71694874799317883764090561454958")
	lnC9        = mustBI("283447036172924575727196451306956")
	lnC10       = mustBI("401686690394027663651624208769553")
	lnC11       = mustBI("204048457590392012362485061816622")
	lnC12       = mustBI("31853899698501571402653359427138")
	lnC13       = mustBI("909429971244387300277376558375")
	lnC14       = mustBI("1677202110996718588342820967067443963516166")
	lnC15       = mustBI("16597577552685614221487285958193947469193820559219878177908093499208371")
	lnC16       = mustBI("600920179829731861736702779321621459595472258049074101567377883020018308")
	lnC6Shifted = new(big.Int).Lsh(lnC6, 96)
)

func mustBI(s string) *big.Int {
	x, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("invalid big.Int constant")
	}
	return x
}

// lnWadBI is Solady's lnWad. Same rounding, so the dislocation term matches the venue's.
func lnWadBI(x *big.Int) (*big.Int, error) {
	if x.Sign() <= 0 {
		return nil, ErrCurveDomain
	}
	if x.BitLen() > 256 {
		return nil, ErrCurveDomain
	}

	r := int64(256 - x.BitLen())
	x96 := new(big.Int).Lsh(new(big.Int).Set(x), uint(r))
	x96.Rsh(x96, 159)

	p := addBI(lnC0, sarBI(mulBI(addBI(lnC1, sarBI(mulBI(addBI(lnC2, x96), x96), 96)), x96), 96))
	p = subBI(sarBI(mulBI(p, x96), 96), lnC3)
	p = subBI(sarBI(mulBI(p, x96), 96), lnC4)
	p = subBI(sarBI(mulBI(p, x96), 96), lnC5)
	p = subBI(mulBI(p, x96), lnC6Shifted)

	q := addBI(lnC7, x96)
	q = addBI(lnC8, sarBI(mulBI(x96, q), 96))
	q = addBI(lnC9, sarBI(mulBI(x96, q), 96))
	q = addBI(lnC10, sarBI(mulBI(x96, q), 96))
	q = addBI(lnC11, sarBI(mulBI(x96, q), 96))
	q = addBI(lnC12, sarBI(mulBI(x96, q), 96))
	q = addBI(lnC13, sarBI(mulBI(x96, q), 96))

	p = sdivBI(p, q)
	p = mulBI(lnC14, p)
	p = addBI(mulBI(lnC15, big.NewInt(159-r)), p)
	p = addBI(lnC16, p)
	return sarBI(p, 174), nil
}

// feeCtx is the part of the law that a fill fixes but the vol scalar does not touch. It
// costs one square root and one logarithm, so it is built once per book state and then
// re-evaluated cheaply across the solve.
type feeCtx struct {
	// scalar is set on the law's two short-circuit branches (zero curvature, degenerate
	// book), where the fee is a constant and carries no information about the scalar.
	scalar       *big.Int
	g            *big.Int
	boost        *big.Int
	multStableIn *big.Int
	multVolIn    *big.Int
}

func newFeeCtx(e *Extra, xWad, reserveStable, reserveVolatile *big.Int) *feeCtx {
	if e.CurvatureWad.Sign() == 0 {
		return &feeCtx{scalar: e.LpFeeWad.ToBig()}
	}
	g, volatileWeight := reductionG(e, reserveStable, reserveVolatile, e.CurvatureWad.ToBig())
	if g.Sign() == 0 {
		return &feeCtx{scalar: e.OutFeeWad.ToBig()}
	}
	anchor := e.ReservationPriceWad.ToBig()
	spot := spotRawWad(e.AnchorSqrtX96.ToBig(), xWad, e.Support.AWad.ToBig())
	if spot == nil {
		return nil
	}
	disloc := logRatioAbsWad(spot, anchor)
	if disloc == nil {
		return nil
	}
	below := spot.Cmp(anchor) < 0
	c := &feeCtx{
		g:     g,
		boost: new(big.Int).Add(bigWadFee, mulDivFloor(e.VolBetaWad.ToBig(), disloc, bigWadFee)),
	}
	c.multStableIn = skewMultiplier(e, volatileWeight, below, true)
	c.multVolIn = skewMultiplier(e, volatileWeight, !below, false)
	return c
}

// raw evaluates the law at this state and scalar, WITHOUT the hook floor.
func (c *feeCtx) raw(e *Extra, sWad *big.Int, stableIn bool) *big.Int {
	if c.scalar != nil {
		return c.scalar
	}
	// A zero reference disables the vol term outright — the hook returns WAD unclamped.
	v := new(big.Int).Set(bigWadFee)
	if e.VolSigmaRefWad.Sign() != 0 {
		v = mulDivFloor(sWad, c.boost, bigWadFee)
		if vMin := e.VolMinWad.ToBig(); v.Cmp(vMin) < 0 {
			v = vMin
		} else if vMax := e.VolMaxWad.ToBig(); v.Cmp(vMax) > 0 {
			v = vMax
		}
	}
	outFee, midFee := e.OutFeeWad.ToBig(), e.MidFeeWad.ToBig()
	f := new(big.Int).Add(outFee, mulDivFloor(new(big.Int).Sub(midFee, outFee),
		mulDivFloor(c.g, v, bigWadFee), bigWadFee))
	if f.Cmp(midFee) > 0 {
		f.Set(midFee)
	}
	m := c.multStableIn
	if !stableIn {
		m = c.multVolIn
	}
	f = mulDivFloor(f, m, bigWadFee)
	if f.Cmp(bigWadFee) > 0 {
		f.Set(bigWadFee)
	}
	return f
}

// maxFactor returns the largest a with floor(a*mul/div) <= limit — the inverse of one
// floored multiply. nil means the step imposes no bound at all.
func maxFactor(limit, mul, div *big.Int) *big.Int {
	if mul.Sign() == 0 {
		return nil
	}
	n := new(big.Int).Mul(new(big.Int).Add(limit, bigOne), div)
	return n.Quo(n.Sub(n, bigOne), mul)
}

// solveVolScalar returns the largest vol scalar that both sampled fees admit.
//
// The scalar is sigma/sigmaRef, which a swap cannot move, so the value solved here at the
// pre-fill book is still the venue's at the post-fill one. It is an UPPER bound rather
// than the exact value because the hook floor only ever raises a fee: the true scalar
// always satisfies raw <= sampled, and this returns the largest scalar that still does.
// The fee is monotone in it, so pricing the fill with it cannot under-quote.
//
// The law is a chain of floored multiplies, so it inverts step by step rather than by
// search. The result is then confirmed to be exactly maximal — anything else means the
// inversion and the forward law disagree, and the caller must fall back.
func solveVolScalar(e *Extra, c *feeCtx) (*big.Int, bool) {
	fits := func(s *big.Int) bool {
		return c.raw(e, s, true).Cmp(e.FeeStableInWad.ToBig()) <= 0 &&
			c.raw(e, s, false).Cmp(e.FeeVolatileInWad.ToBig()) <= 0
	}
	unbounded := e.VolMaxWad.ToBig()
	if c.scalar != nil || e.VolSigmaRefWad.Sign() == 0 {
		// The fee does not depend on the scalar here, so the samples say nothing about it.
		return unbounded, fits(unbounded)
	}

	// Back through the directional multiplier, taking the tighter of the two legs.
	var baseHi *big.Int
	for _, d := range []struct {
		mult    *big.Int
		sampled *uint256.Int
	}{{c.multStableIn, e.FeeStableInWad}, {c.multVolIn, e.FeeVolatileInWad}} {
		sampled := d.sampled.ToBig()
		if sampled.Cmp(bigWadFee) >= 0 {
			continue // the law caps at WAD, so this leg admits any base
		}
		if b := maxFactor(sampled, d.mult, bigWadFee); b != nil && (baseHi == nil || b.Cmp(baseHi) < 0) {
			baseHi = b
		}
	}

	hi := unbounded
	outFee, midFee := e.OutFeeWad.ToBig(), e.MidFeeWad.ToBig()
	if baseHi != nil && baseHi.Cmp(midFee) < 0 {
		if baseHi.Cmp(outFee) < 0 {
			return nil, false // below the law's own floor at every scalar
		}
		// Back through the interpolation, the reduction coefficient, and the boost.
		v := maxFactor(new(big.Int).Sub(baseHi, outFee), new(big.Int).Sub(midFee, outFee), bigWadFee)
		if v != nil {
			v = maxFactor(v, c.g, bigWadFee)
		}
		if v != nil && v.Cmp(e.VolMaxWad.ToBig()) < 0 {
			if v.Cmp(e.VolMinWad.ToBig()) < 0 {
				return nil, false // the clamp holds the multiplier above this
			}
			if s := maxFactor(v, c.boost, bigWadFee); s != nil && s.Cmp(hi) < 0 {
				hi = s
			}
		}
	}

	// Maximal by construction; confirmed so an inversion that disagrees with the forward
	// law declines instead of pricing off a scalar on either side of the truth.
	if !fits(hi) || (hi.Cmp(unbounded) < 0 && fits(new(big.Int).Add(hi, bigOne))) {
		return nil, false
	}
	return hi, true
}

// feeSolve is what the re-derivation must not recompute per hop: the vol scalar, which a
// swap cannot move, and the block's OWN sampled fees, which bound the hook floor the law
// never gets to read. Solving again from an already re-derived fee would ratchet the
// haircut up hop over hop instead of repricing from the venue.
type feeSolve struct {
	tried  bool
	scalar *big.Int
	// Per-direction upper bounds on the hook floor, fixed at solve time — see floorUpperBound.
	floorS, floorV *big.Int
}

// solve brackets the scalar once, from the state the snapshot was read at.
func (f *feeSolve) solve(e *Extra, x, reserveStable, reserveVolatile *big.Int) bool {
	if f.tried {
		return f.scalar != nil
	}
	f.tried = true
	if !e.feeLawExactTracked() {
		return false
	}
	c := newFeeCtx(e, x, reserveStable, reserveVolatile)
	if c == nil {
		return false
	}
	s, ok := solveVolScalar(e, c)
	if !ok {
		return false
	}
	f.scalar = s
	f.floorS = floorUpperBound(e, c.raw(e, s, true), e.FeeStableInWad.ToBig(), true)
	f.floorV = floorUpperBound(e, c.raw(e, s, false), e.FeeVolatileInWad.ToBig(), false)
	return true
}

// floorUpperBound bounds the hook floor for one direction, from above, at the state the
// snapshot was read at.
//
// The floor depends on the direction and the decayed push rate, neither of which a fill
// touches, so the bound still holds after one. Two things constrain it. The sampled fee is
// max(law, floor): when it sits ABOVE what the law produces, the floor is what raised it
// and the sample IS the floor exactly; otherwise the floor merely sits at or below the
// law's own output. The floor word is the hook's floor at the LIVE push rate when the
// tracker could compute it (then this bound is exact), else the floor at a SATURATED
// push rate — its ceiling outright, since the hook ramps it monotonically
// (`level * smoothstep(t)`) and clamps at that level.
//
// Taking the tighter of the two is what keeps a direction REVERSAL exact. The pre-fill
// sample for the reversing leg is high for a directional reason, not a floor one, so
// carrying it forward would re-impose the fee the fill just moved away from.
func floorUpperBound(e *Extra, rawPre, sampled *big.Int, stableIn bool) *big.Int {
	if sampled.Cmp(rawPre) > 0 {
		return new(big.Int).Set(sampled)
	}
	ub := new(big.Int).Set(rawPre)
	f := e.FloorVolatileInWad
	if stableIn {
		f = e.FloorStableInWad
	}
	// A floor word that did not decode leaves only the sample-derived bound.
	if f != nil && f.ToBig().Cmp(ub) < 0 {
		ub = f.ToBig()
	}
	return ub
}

// reSampleFeesExact reprices both legs at the post-fill book through the full law.
//
// The hook floor needs no second round of calls: floorUpperBound pins it from the sample
// and the floor word the tracker already carries (the live-rate floor when computable,
// else the saturated-rate ceiling).
//
// swap() itself folds nothing: `rv` and the FFAD push rate move only through the
// permissionless poke() and the keeper's recenter(), so the sampled fee is exact for the
// snapshot block and goes stale only if one of those lands first. Both hops of a route
// inherit the same staleness, which is what the snapshot already assumes everywhere else.
func reSampleFeesExact(e *Extra, f *feeSolve, preX, preStable, preVolatile,
	postX, postStable, postVolatile *big.Int) (newStableIn, newVolatileIn *big.Int, ok bool) {
	if !f.solve(e, preX, preStable, preVolatile) {
		return nil, nil, false
	}
	postCtx := newFeeCtx(e, postX, postStable, postVolatile)
	if postCtx == nil {
		return nil, nil, false
	}
	atLeastFloor := func(raw, floor *big.Int) *big.Int {
		if raw.Cmp(floor) < 0 {
			return new(big.Int).Set(floor)
		}
		return new(big.Int).Set(raw)
	}
	return atLeastFloor(postCtx.raw(e, f.scalar, true), f.floorS),
		atLeastFloor(postCtx.raw(e, f.scalar, false), f.floorV), true
}

// feeLawExactTracked reports whether the snapshot carries every term the exact law reads.
func (e *Extra) feeLawExactTracked() bool {
	return e.feeLawTracked() && e.CurvatureWad != nil && e.OutFeeWad != nil &&
		e.VolSigmaRefWad != nil && e.VolBetaWad != nil &&
		e.VolMinWad != nil && e.VolMaxWad != nil && e.VolMaxWad.Sign() > 0 &&
		e.Support.AWad != nil && e.AnchorSqrtX96 != nil && e.AnchorSqrtX96.Sign() > 0 &&
		e.FeeStableInWad != nil && e.FeeVolatileInWad != nil &&
		!e.MidFeeWad.Lt(e.OutFeeWad)
}

// reSampleFeesBest takes the tighter of the two re-derivations.
//
// Both bound the post-fill fee from above, so the smaller is the better quote and is
// still safe. The exact law wins wherever the base is off its cap — the case the bound
// alone prices as MidFee, which measured 58-59 bps too wide on a revisit. The bound wins
// where the exact law declines: a snapshot predating the extra hook terms, a hook
// upgrade, or a book the modelled law does not reproduce.
func reSampleFeesBest(e *Extra, f *feeSolve, preX, preStable, preVolatile,
	postX, postStable, postVolatile *big.Int) (newStableIn, newVolatileIn *big.Int, ok bool) {
	xs, xv, xok := reSampleFeesExact(e, f, preX, preStable, preVolatile, postX, postStable, postVolatile)
	bs, bv, bok := reSampleFees(e, preX, preStable, preVolatile, postX, postStable, postVolatile)
	switch {
	case xok && bok:
		return bigMin(xs, bs), bigMin(xv, bv), true
	case xok:
		return xs, xv, true
	case bok:
		return bs, bv, true
	}
	return nil, nil, false
}

func bigMin(a, b *big.Int) *big.Int {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}
