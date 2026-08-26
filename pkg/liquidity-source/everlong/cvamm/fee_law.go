package everlongcvamm

import "math/big"

// Shared arithmetic for the exact CvammFeeLib port in fee_law_exact.go.

var (
	bigWadFee              = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	bigHalfWad             = new(big.Int).Div(new(big.Int).Set(bigWadFee), big.NewInt(2))
	ffadReferenceRateWad   = new(big.Int).Mul(big.NewInt(40), bigWadFee)
	ffadSaturationRateWad  = new(big.Int).Mul(big.NewInt(160), bigWadFee)
	ffadStableInFloorWad   = big.NewInt(15_000_000_000_000_000)
	ffadVolatileInFloorWad = big.NewInt(25_000_000_000_000_000)
)

func mulDivFloor(x, y, d *big.Int) *big.Int {
	var p big.Int
	p.Mul(x, y)
	return p.Quo(&p, d)
}

// supportedHookHotFloor is the exact hotFeeFloorWad law embedded in the reviewed
// ClammFeeHook runtime selected by supportedFeeHookCodeHash. Callers must attest that
// hash before using this function: another hook may expose the same ABI with arbitrary
// semantics.
func supportedHookHotFloor(stableIn bool, pushRateWad *big.Int) *big.Int {
	if pushRateWad == nil || pushRateWad.Cmp(ffadReferenceRateWad) <= 0 {
		return new(big.Int)
	}
	level := ffadVolatileInFloorWad
	if stableIn {
		level = ffadStableInFloorWad
	}
	if pushRateWad.Cmp(ffadSaturationRateWad) >= 0 {
		return new(big.Int).Set(level)
	}
	t := mulDivFloor(new(big.Int).Sub(pushRateWad, ffadReferenceRateWad), bigWadFee,
		new(big.Int).Sub(ffadSaturationRateWad, ffadReferenceRateWad))
	tSquared := mulDivFloor(t, t, bigWadFee)
	smoothstep := mulDivFloor(tSquared,
		new(big.Int).Sub(new(big.Int).Mul(big.NewInt(3), bigWadFee), new(big.Int).Mul(big.NewInt(2), t)),
		bigWadFee)
	return mulDivFloor(level, smoothstep, bigWadFee)
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
