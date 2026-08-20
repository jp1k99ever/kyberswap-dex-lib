package everlongcvamm

import "math/big"

// Post-fill fee re-derivation for the SATURATED regime of CvammFeeLib.
//
// The fee law is fee = clamp(out + (mid-out)*g*v, out, mid) * skewMultiplier, floored by
// an optional hook floor. Its `v` term carries realized variance, which has no getter on
// the ALM and so cannot be recomputed off-chain. But when g*v saturates the cap the base
// collapses to MidFee, and the whole fee becomes MidFee * multiplier — and the multiplier
// depends only on the inventory coordinate and the reserve split, both of which a fill
// leaves us holding exactly.
//
// So a fill can be repriced exactly whenever the PRE-fill sample is reproduced by that
// same expression in BOTH directions. When it is not — the book left saturation, the hook
// floor binds, or the hook was upgraded — reSampleFees reports false and the caller keeps
// the conservative fold.

var (
	bigWadFee  = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	bigHalfWad = new(big.Int).Div(new(big.Int).Set(bigWadFee), big.NewInt(2))
)

func mulDivFloor(x, y, d *big.Int) *big.Int {
	var p big.Int
	p.Mul(x, y)
	return p.Quo(&p, d)
}

// skewMultiplierWad ports the directional and inventory-displacement terms.
//
// `restoring` is the venue's own price test, (spot < reservationPrice) == stableIn,
// evaluated on the same floored spot the law uses. (x > WAD/2 is the analytic
// equivalent only while the anchor sits exactly on the reservation price; after a
// recenter the floored anchor can sit a few wei under it.)
func skewMultiplierWad(e *Extra, xWad, reserveStable, reserveVolatile *big.Int,
	stableIn bool) *big.Int {
	vv := mulDivFloor(reserveVolatile, e.ReservationPriceWad.ToBig(), bigWadFee)
	total := new(big.Int).Add(reserveStable, vv)
	if reserveStable.Sign() == 0 || vv.Sign() == 0 {
		return nil // degenerate book: the law returns outFee, not the capped base
	}
	volatileWeight := mulDivFloor(vv, bigWadFee, total)

	spot := spotRawWad(e.AnchorSqrtX96.ToBig(), xWad, e.Support.AWad.ToBig())
	return skewMultiplier(e, volatileWeight, (spot.Cmp(e.ReservationPriceWad.ToBig()) < 0) == stableIn, stableIn)
}

// skewMultiplier is the directional and inventory-displacement product, given the
// volatile value weight and whether the fill restores the price toward the anchor.
func skewMultiplier(e *Extra, volatileWeight *big.Int, restoring, stableIn bool) *big.Int {
	m := new(big.Int)
	if restoring {
		m.Sub(bigWadFee, e.DirSkewWad.ToBig())
	} else {
		m.Add(bigWadFee, e.DirSkewWad.ToBig())
	}

	inventoryIncreasing := volatileWeight.Cmp(bigHalfWad) < 0
	if !stableIn {
		inventoryIncreasing = volatileWeight.Cmp(bigHalfWad) > 0
	}
	if kappa := e.InvSkewKappaWad.ToBig(); inventoryIncreasing && kappa.Sign() != 0 {
		absDev := new(big.Int).Sub(volatileWeight, bigHalfWad)
		absDev.Abs(absDev)
		if band := e.InvSkewBandWad.ToBig(); absDev.Cmp(band) > 0 {
			m.Add(m, mulDivFloor(kappa, new(big.Int).Sub(absDev, band), bigWadFee))
		}
	}
	return m
}

// floorFor is the hook floor at a saturated push rate — an upper bound on the floor at
// any live rate.
func floorFor(e *Extra, stableIn bool) *big.Int {
	f := e.FloorVolatileInWad
	if stableIn {
		f = e.FloorStableInWad
	}
	if f == nil {
		return new(big.Int)
	}
	return f.ToBig()
}

// feeUpperBoundWad bounds the fee at the given state from ABOVE, exactly.
//
// The law is fee = clamp(out + (mid-out)*g*v, out, mid) * multiplier, then raised to the
// hook floor. `g*v` carries realized variance and is unknowable off-chain — but the
// clamp caps the base at MidFee whatever it does, so the fee can never exceed
// max(MidFee * multiplier, floor). When the base actually sits at that cap the bound is
// the fee itself, so the saturated regime prices exactly and the rest prices safely.
//
// Zero curvature selects the law's scalar branch, where the fee is LpFee and the
// multiplier does not apply.
func feeUpperBoundWad(e *Extra, xWad, reserveStable, reserveVolatile *big.Int,
	stableIn bool) *big.Int {
	floor := floorFor(e, stableIn)
	if e.CurvatureWad != nil && e.CurvatureWad.Sign() == 0 {
		f := e.LpFeeWad.ToBig()
		if f.Cmp(floor) < 0 {
			f = floor
		}
		if f.Cmp(bigWadFee) > 0 {
			f = new(big.Int).Set(bigWadFee)
		}
		return f
	}
	m := skewMultiplierWad(e, xWad, reserveStable, reserveVolatile, stableIn)
	if m == nil {
		return nil
	}
	f := mulDivFloor(e.MidFeeWad.ToBig(), m, bigWadFee)
	if f.Cmp(bigWadFee) > 0 {
		f.Set(bigWadFee)
	}
	if f.Cmp(floor) < 0 {
		f = floor
	}
	// The cap is applied AFTER the floor: on-chain a floor above WAD is discarded
	// outright (`floorWad > WAD` returns the base fee), so a floor that escaped the cap
	// here would price a fee the venue never charges — and a fee above WAD underflows
	// the caller's `gross - fee`.
	if f.Cmp(bigWadFee) > 0 {
		f = new(big.Int).Set(bigWadFee)
	}
	return f
}

// feeLawTracked reports whether the snapshot carries every term the bound needs.
func (e *Extra) feeLawTracked() bool {
	return e.MidFeeWad != nil && e.DirSkewWad != nil && e.InvSkewKappaWad != nil &&
		e.InvSkewBandWad != nil && e.LpFeeWad != nil && e.ReservationPriceWad != nil &&
		e.ReservationPriceWad.Sign() > 0
}

// reSampleFees reprices both legs at the POST-fill state.
//
// It returns false when the bound fails to hold at the PRE-fill state, which means the
// modelled law no longer describes the venue — a hook upgrade, a term we do not read —
// and the caller must fall back rather than trust it.
func reSampleFees(e *Extra, preX, preStable, preVolatile,
	postX, postStable, postVolatile *big.Int) (newStableIn, newVolatileIn *big.Int, ok bool) {
	if !e.feeLawTracked() {
		return nil, nil, false
	}
	for _, dir := range []struct {
		stableIn bool
		sampled  *big.Int
	}{
		{true, e.FeeStableInWad.ToBig()},
		{false, e.FeeVolatileInWad.ToBig()},
	} {
		bound := feeUpperBoundWad(e, preX, preStable, preVolatile, dir.stableIn)
		if bound == nil || bound.Cmp(dir.sampled) < 0 {
			return nil, nil, false
		}
	}
	newStableIn = feeUpperBoundWad(e, postX, postStable, postVolatile, true)
	newVolatileIn = feeUpperBoundWad(e, postX, postStable, postVolatile, false)
	if newStableIn == nil || newVolatileIn == nil {
		return nil, nil, false
	}
	return newStableIn, newVolatileIn, true
}
