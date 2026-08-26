package everlongcvamm

import (
	"math/big"

	"github.com/holiman/uint256"
)

// Exact port of CvammFeeLib. The deployed implementation has no realizedVariance()
// getter, so the tracker reads CvammStore.rv from its implementation-pinned ERC-7201
// slot at the snapshot block. No fee input is inferred from sampled output: the sampled
// directional fees are used only as a parity attestation before a chained quote is
// enabled.

var (
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

// realizedVarianceScalar is Solidity's
// Math.mulDiv(Math.sqrt(rv) * 1e9, WAD, sigmaRef). The intermediate fits uint256
// because sqrt(uint256.max) * 1e9 is below 2^158.
func realizedVarianceScalar(e *Extra) *big.Int {
	if e.RealizedVarianceWad == nil || e.VolSigmaRefWad == nil {
		return nil
	}
	if e.VolSigmaRefWad.IsZero() {
		return new(big.Int).Set(bigWadFee) // raw() disables the vol term on this branch
	}
	sigma := new(big.Int).Sqrt(e.RealizedVarianceWad.ToBig())
	sigma.Mul(sigma, big.NewInt(1_000_000_000))
	return mulDivFloor(sigma, bigWadFee, e.VolSigmaRefWad.ToBig())
}

// applyExactFloor mirrors CvammFeeLib._applyHotFloor. A malformed floor above WAD is
// ignored by the contract rather than capped.
func applyExactFloor(raw *big.Int, floor *uint256.Int) *big.Int {
	out := new(big.Int).Set(raw)
	if floor == nil {
		return out
	}
	f := floor.ToBig()
	if f.Cmp(bigWadFee) <= 0 && f.Cmp(out) > 0 {
		out.Set(f)
	}
	return out
}

// feeLawExactTracked reports whether the snapshot carries every input used by the
// deployed dynamic fee law. The zero-curvature branch needs only lpFee and the exact
// live floors; every other branch carries the full set so a liquidity move cannot
// switch from a degenerate shortcut into an untracked branch.
func (e *Extra) feeLawExactTracked() bool {
	if !e.FeeHookActive || !e.HotFloorsExact || e.CurvatureWad == nil || e.LpFeeWad == nil ||
		e.FloorStableInWad == nil || e.FloorVolatileInWad == nil ||
		e.FeeStableInWad == nil || e.FeeVolatileInWad == nil {
		return false
	}
	if e.CurvatureWad.IsZero() {
		return true
	}
	return e.MidFeeWad != nil && e.DirSkewWad != nil && e.InvSkewKappaWad != nil &&
		e.InvSkewBandWad != nil && e.ReservationPriceWad != nil &&
		e.ReservationPriceWad.Sign() > 0 && e.OutFeeWad != nil &&
		e.VolSigmaRefWad != nil && e.VolBetaWad != nil && e.VolMinWad != nil &&
		e.VolMaxWad != nil && e.VolMaxWad.Sign() > 0 && e.RealizedVarianceWad != nil &&
		e.Support.AWad != nil && e.AnchorSqrtX96 != nil && e.AnchorSqrtX96.Sign() > 0 &&
		!e.MidFeeWad.Lt(e.OutFeeWad)
}

func exactFeesAt(e *Extra, x, reserveStable, reserveVolatile *big.Int) (
	stableIn, volatileIn *big.Int, ok bool) {
	if !e.feeLawExactTracked() {
		return nil, nil, false
	}
	c := newFeeCtx(e, x, reserveStable, reserveVolatile)
	if c == nil {
		return nil, nil, false
	}
	scalar := realizedVarianceScalar(e)
	if c.scalar == nil && scalar == nil {
		return nil, nil, false
	}
	// The scalar is ignored by raw() on the zero-curvature and degenerate branches.
	if scalar == nil {
		scalar = new(big.Int)
	}
	return applyExactFloor(c.raw(e, scalar, true), e.FloorStableInWad),
		applyExactFloor(c.raw(e, scalar, false), e.FloorVolatileInWad), true
}

// reSampleFeesExact first proves that the layout-read rv and all ported hook terms
// reproduce BOTH fees sampled from the contract at the pre-move book. Only then does it
// publish fees for the post-move book. A mismatch is not bounded or approximated: the
// caller disables any chained quote from that state.
func reSampleFeesExact(e *Extra, preX, preStable, preVolatile,
	postX, postStable, postVolatile *big.Int) (newStableIn, newVolatileIn *big.Int, ok bool) {
	preS, preV, ok := exactFeesAt(e, preX, preStable, preVolatile)
	if !ok || preS.Cmp(e.FeeStableInWad.ToBig()) != 0 ||
		preV.Cmp(e.FeeVolatileInWad.ToBig()) != 0 {
		return nil, nil, false
	}
	return exactFeesAt(e, postX, postStable, postVolatile)
}
