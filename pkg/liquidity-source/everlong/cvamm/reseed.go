package everlongcvamm

import (
	"math/big"
)

// bigQ96 is 2**96 at big.Int width.
var bigQ96 = new(big.Int).Lsh(big.NewInt(1), 96)

// The reseed path: CvammCurve.reseed as CvammALM._solveReseed calls it. A deposit into a
// RETRACTED book (kappa == 0) does not scale kappa — there is nothing to scale — it
// re-solves the coordinate and the scale from the reserves the deposit just created, and
// parks whatever the curve cannot express as idle. Missing it leaves a coupled base pool
// permanently retracted while the chain has re-armed it, which is liquidity this
// integration would then never quote.
//
// Everything here is big.Int: it runs once per re-arm, never on the quote path.

// heldAt = CvammCurve.heldAt: the curve's normalized holdings at coordinate x.
func heldAt(sup *Support, xWad *big.Int) (volatileWad, stableWad *big.Int) {
	xLo, xHi := sup.XLo.ToBig(), sup.XHi.ToBig()
	x := new(big.Int).Set(xWad)
	if x.Cmp(xLo) < 0 {
		x = xLo
	} else if x.Cmp(xHi) > 0 {
		x = xHi
	}
	volatileWad = new(big.Int).Sub(x, xLo)
	y := yAtXBig(x, sup.AWad.ToBig())
	stableWad = new(big.Int)
	if yHi := sup.YHi.ToBig(); y.Cmp(yHi) > 0 {
		stableWad.Sub(y, yHi)
	}
	return volatileWad, stableWad
}

// reservesAt = CvammCurve.reservesAt: the token reserves a scale kappa expresses at x.
func reservesAt(sup *Support, anchorSqrtX96, kappa, xWad *big.Int) (stable, volatileAmount *big.Int) {
	volatileHeld, stableHeld := heldAt(sup, xWad)
	stable = mulDivFloorBig(mulDivFloorBig(kappa, stableHeld, bigWadFee), anchorSqrtX96, bigQ96)
	volatileAmount = mulDivFloorBig(mulDivFloorBig(kappa, volatileHeld, bigWadFee), bigQ96, anchorSqrtX96)
	return stable, volatileAmount
}

// ratioGte = CvammCurve._ratioGte: is the curve at x at least as stable-heavy as the
// book? Compared as a cross product, so neither side is divided and floored first.
func ratioGte(sup *Support, xWad, stableValue, volatileValue *big.Int) bool {
	v, s := heldAt(sup, xWad)
	left := new(big.Int).Mul(s, volatileValue)
	right := new(big.Int).Mul(stableValue, v)
	return left.Cmp(right) >= 0
}

// xAtValueRatio = CvammCurve.xAtValueRatio: the coordinate whose composition matches the
// book's, by the contract's own bisection (same edges, same 128-iteration cap, same
// convergence exit, and the same choice of `lo` so the stable leg binds in the scale
// solve and the volatile residual becomes surplus rather than deficit).
func xAtValueRatio(sup *Support, stableValue, volatileValue *big.Int) *big.Int {
	if stableValue.Sign() == 0 && volatileValue.Sign() == 0 {
		return new(big.Int).Rsh(bigWadFee, 1) // an empty book carries no composition
	}
	xLo, xHi := sup.XLo.ToBig(), sup.XHi.ToBig()
	if volatileValue.Sign() == 0 {
		return new(big.Int).Set(xLo)
	}
	if stableValue.Sign() == 0 {
		return new(big.Int).Set(xHi)
	}
	lo, hi := new(big.Int).Set(xLo), new(big.Int).Set(xHi)
	if !ratioGte(sup, lo, stableValue, volatileValue) {
		return lo
	}
	if ratioGte(sup, hi, stableValue, volatileValue) {
		return hi
	}
	var mid big.Int
	for i := 0; i < 128; i++ {
		mid.Add(lo, hi).Rsh(&mid, 1)
		if mid.Cmp(lo) == 0 {
			break
		}
		if ratioGte(sup, &mid, stableValue, volatileValue) {
			lo.Set(&mid)
		} else {
			hi.Set(&mid)
		}
	}
	return lo
}

// scaleAt = CvammCurve._scaleAt: the largest kappa both legs can fund. The order of
// association is load-bearing on-chain (scale by WAD/held BEFORE the anchor conversion,
// or a wei-scale leg floors to zero) and is kept here.
func scaleAt(sup *Support, anchorSqrtX96, xWad, stableTarget, volatileTarget *big.Int) *big.Int {
	volatileHeld, stableHeld := heldAt(sup, xWad)
	kStable, kVolatile := (*big.Int)(nil), (*big.Int)(nil)
	if stableHeld.Sign() != 0 {
		kStable = mulDivFloorBig(mulDivFloorBig(stableTarget, bigWadFee, stableHeld), bigQ96, anchorSqrtX96)
	}
	if volatileHeld.Sign() != 0 {
		kVolatile = mulDivFloorBig(mulDivFloorBig(volatileTarget, bigWadFee, volatileHeld), anchorSqrtX96, bigQ96)
	}
	switch {
	case kStable == nil && kVolatile == nil:
		return new(big.Int) // degenerate band: fail closed, exactly as the contract does
	case kStable == nil:
		return kVolatile
	case kVolatile == nil:
		return kStable
	case kStable.Cmp(kVolatile) < 0:
		return kStable
	default:
		return kVolatile
	}
}

// reseed = CvammCurve.reseed: the coordinate and scale a retracted book re-arms at, plus
// the per-leg surplus the curve cannot express — which the ALM moves into idle.
func reseed(sup *Support, anchorSqrtX96, stableTarget, volatileTarget *big.Int) (
	xWad, newKappa, stableSurplus, volatileSurplus *big.Int) {
	if anchorSqrtX96 == nil || anchorSqrtX96.Sign() == 0 {
		return nil, nil, nil, nil
	}
	// Value the volatile leg in stable AT THE ANCHOR: one factor of the anchor per
	// mulDiv, matching the contract's scale convention rather than forming the price.
	volatileValue := mulDivFloorBig(mulDivFloorBig(volatileTarget, anchorSqrtX96, bigQ96), anchorSqrtX96, bigQ96)
	xWad = xAtValueRatio(sup, stableTarget, volatileValue)
	newKappa = scaleAt(sup, anchorSqrtX96, xWad, stableTarget, volatileTarget)
	s, v := reservesAt(sup, anchorSqrtX96, newKappa, xWad)
	stableSurplus, volatileSurplus = new(big.Int), new(big.Int)
	if stableTarget.Cmp(s) > 0 {
		stableSurplus.Sub(stableTarget, s)
	}
	if volatileTarget.Cmp(v) > 0 {
		volatileSurplus.Sub(volatileTarget, v)
	}
	return xWad, newKappa, stableSurplus, volatileSurplus
}
