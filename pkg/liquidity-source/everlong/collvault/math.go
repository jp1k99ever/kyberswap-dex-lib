package everlongcollvault

import (
	"math/big"
)

// Pure-integer port of the deployed CollateralRebalancer curve ("CR math") plus the
// CollateralRebalancerSwapper's token-leg preview:
//   - CollRebalancerMath.leverageQuote / deleverageQuote (strict branch AND the
//     conservative missed-wall recovery continuation for deleverage),
//   - isStateSafe / anchorBestEffort (the fill-acceptance predicate applied on top of
//     every quote),
//   - Swapper._previewTokenAmounts (CollVault ERC-4626 -> ALM proportional split),
//   - the composed quoteLeverageAt / deleverageLegsAt the swap legs settle by.
//
// Source of truth is the deployment's CollRebalancerMath library; every function
// replicates Solidity semantics exactly: floor division, Math.mulDiv with Rounding.Up =
// ceil, Math.sqrt = floor, and Mul512.productGt as full-precision a*b > c*d. big.Int is
// used because intermediates exceed 256 bits (the contract uses a 512-bit library for
// them). Validated wei-exact against the shipped library bytecode over a fixture grid
// (see math_test.go / testdata).
//
// The curve constants (wall anchors, Bézier controls, rescue spread) are STRATEGY
// PARAMS frozen into each deployment — they are per-chain configuration, never
// universal math.

var (
	bigWad      = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	bigWadSq    = new(big.Int).Exp(big.NewInt(10), big.NewInt(36), nil)
	bigPpm      = big.NewInt(1_000_000)
	bigMaxInput = new(big.Int).Exp(big.NewInt(10), big.NewInt(38), nil)
	bigBp       = big.NewInt(10_000)
	// Deployed CollateralRebalancer fill-acceptance constants (CollateralRebalancer.sol):
	// the post-fill internal value must stay within [reservation/2, reservation*2], and the
	// post-fill ICR may only sit below MCR*1.2 when the fill did not decrease it.
	bigPriceBandNum    = big.NewInt(2)
	bigIcrFloorFactor  = new(big.Int).Mul(big.NewInt(12), new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil))
	bigInterestRayPrec = new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil) // INTEREST_PRECISION
)

// CurveParams are the deployed CollateralRebalancer's frozen curve constants.
type CurveParams struct {
	LeverageRatioWad   *big.Int    `json:"rWad"`
	HZero              *big.Int    `json:"hZero"`
	HJoin              *big.Int    `json:"hJoin"`
	HWall              *big.Int    `json:"hWall"`
	Width              *big.Int    `json:"width"`
	DJoin              *big.Int    `json:"dJoin"`
	DWall              *big.Int    `json:"dWall"`
	RescueSpreadPpm    *big.Int    `json:"rescueSpread"`
	BezierPhi          [4]*big.Int `json:"bezierPhi"`      // P0..P3: phi = dD/dh cubic controls
	BezierIntegral     [5]*big.Int `json:"bezierIntegral"` // Q0..Q4: exact quartic integral controls
	PhysicalCrFloorWad *big.Int    `json:"crFloor"`        // PRE-fill floor capping the max leverage lot
}

// VaultState is the per-refresh rebalancer + vault snapshot (all read from chain).
type VaultState struct {
	// exchangeState(): position collateral (CollVault shares), debt (stable wei),
	// reservation price R (WAD) and the posted spread (PPM).
	Collateral *big.Int `json:"c"`
	Debt       *big.Int `json:"d"`
	PriceWad   *big.Int `json:"r"`
	SpreadPpm  *big.Int `json:"s"`

	// ALM & CollVault state for the token-leg split.
	AlmStableReserve   *big.Int `json:"asr"` // alm.getTotalAmounts()[0]
	AlmVolatileReserve *big.Int `json:"avr"` // alm.getTotalAmounts()[1]
	AlmSupply          *big.Int `json:"as"`  // alm.totalSupply()
	CvTotalAssets      *big.Int `json:"cta"` // collVault.totalAssets()
	CvTotalSupply      *big.Int `json:"cts"` // collVault.totalSupply()
	CvDecimalsOffset   uint8    `json:"cdo"` // 18 - collVault.assetDecimals()
	WithdrawFeeBp      *big.Int `json:"wfb"` // collVault.getWithdrawFee()

	// positionManager.interestRate(): non-zero disables debt origination outright, so
	// the rebalancer reverts every leverage fill while deleverage stays live.
	InterestRate *big.Int `json:"ir,omitempty"`

	// borrowerOperations.minNetDebt() and the position manager's immutable
	// DEBT_GAS_COMPENSATION. The CDP refuses any repayment leaving the position's NET
	// debt (total debt minus gas compensation) below the minimum, so together they cap
	// the deleverage lot at debt - gasComp - minNetDebt. Zero disables the bound (a
	// deployment that does not expose them).
	MinNetDebt          *big.Int `json:"mnd,omitempty"`
	DebtGasCompensation *big.Int `json:"gasComp,omitempty"`

	// alm.getReservesAtReference(): reference-marked physical reserves for the
	// PHYSICAL_CR_FLOOR check bounding the max leverage lot.
	RefStableReserve   *big.Int `json:"rsr"`
	RefAssetReserve    *big.Int `json:"rar"`
	RefRawReferenceWad *big.Int `json:"rrw"`

	// alm.reservationValuePerShareWad(): the endogenous per-ALM-share mark the rebalancer
	// derives its reservation value from (_reservationValue). Invariant under the fill's
	// own pro-rata ALM mint/burn, so the post-fill reservation value recompute in
	// _assertFillSafe only re-runs the CollVault share conversion on the post-fill vault.
	// nil on states persisted before this word was tracked: the post-fill acceptance then
	// falls back to the pre-fill reservation value.
	RvpsWad *big.Int `json:"rvps,omitempty"`

	// positionManager.MCR() and fetchPrice(): the CDP-health leg of _assertFillSafe.
	// The fill reverts RebalanceBelowFloor when the post-fill ICR is below MCR*1.2 AND
	// below the pre-fill ICR. nil skips the check (states persisted before tracking).
	McrWad      *big.Int `json:"mcr,omitempty"`
	IcrPriceWad *big.Int `json:"icrP,omitempty"`

	// Interest-accrual venue (PositionManager): debt drifts with time once governance
	// enables interest, with no event. rawDebt*activeIndex/posIndex (+ pending
	// redistribution debt) reproduces getPositionCollAndDebt at any timestamp.
	ActiveInterestIndex   *big.Int `json:"aii,omitempty"` // activeInterestIndex() (1e27)
	LastActiveIndexUpdate *big.Int `json:"laiu,omitempty"`
	PosRawDebt            *big.Int `json:"prd,omitempty"` // Positions(vault).debt (unprojected)
	PosInterestIndex      *big.Int `json:"pii,omitempty"` // Positions(vault).activeInterestIndex
	PendingDebtReward     *big.Int `json:"pdr,omitempty"` // getPendingCollAndDebtRewards(vault)[1]
}

// ---------- Solidity primitive equivalents ----------

// mulDiv = Math.mulDiv(x, y, d): floor(x*y/d). Operands are non-negative.
func mulDiv(x, y, d *big.Int) *big.Int {
	var p big.Int
	p.Mul(x, y)
	return p.Quo(&p, d)
}

// mulDivUp = Math.mulDiv(x, y, d, Rounding.Up): ceil(x*y/d). Operands are non-negative.
func mulDivUp(x, y, d *big.Int) *big.Int {
	var p, m big.Int
	p.Mul(x, y)
	p.QuoRem(&p, d, &m)
	if m.Sign() != 0 {
		p.Add(&p, big.NewInt(1))
	}
	return &p
}

// productGt = Mul512.productGt: a*b > c*d at full precision.
func productGt(a, b, c, d *big.Int) bool {
	var l, r big.Int
	return l.Mul(a, b).Cmp(r.Mul(c, d)) > 0
}

// ---------- Marked value & anchor ----------

// markedValue: cv = floor(collateral*price/WAD); ok=false on overflow/out-of-bounds.
func markedValue(collateral, price *big.Int) (*big.Int, bool) {
	if price.Sign() == 0 {
		return nil, false
	}
	// the contract rejects when the 512-bit product's high word reaches WAD<<256
	var limit big.Int
	limit.Lsh(bigWad, 256)
	var p big.Int
	if p.Mul(collateral, price); p.Cmp(&limit) >= 0 {
		return nil, false
	}
	cv := mulDiv(collateral, price, bigWad)
	return cv, cv.Cmp(bigMaxInput) <= 0
}

func rootIntervalContains(cv, debt, anchor *big.Int) bool {
	var s, s2, a8 big.Int
	s.Add(anchor, debt)
	s2.Mul(&s, &s)
	a8.Mul(big.NewInt(8), anchor)
	return !productGt(big.NewInt(3), &s2, &a8, cv)
}

func halfLawAnchor(cv, debt *big.Int) *big.Int {
	var twoCv, threeDebt big.Int
	twoCv.Mul(big.NewInt(2), cv)
	threeDebt.Mul(big.NewInt(3), debt)
	if threeDebt.Cmp(&twoCv) > 0 {
		return new(big.Int)
	}
	var rootArg, root big.Int
	rootArg.Sub(&twoCv, &threeDebt)
	rootArg.Mul(&twoCv, &rootArg)
	root.Sqrt(&rootArg) // Math.sqrt default rounding = floor
	var anchor big.Int
	anchor.Mul(big.NewInt(8), cv)
	anchor.Sub(&anchor, new(big.Int).Mul(big.NewInt(6), debt))
	anchor.Add(&anchor, new(big.Int).Mul(big.NewInt(4), &root))
	anchor.Quo(&anchor, big.NewInt(6))
	var plusOne big.Int
	plusOne.Add(&anchor, big.NewInt(1))
	if rootIntervalContains(cv, debt, &plusOne) {
		return &plusOne
	}
	return &anchor
}

func (cp *CurveParams) debtNormWad(h *big.Int) *big.Int {
	if h.Cmp(cp.HJoin) <= 0 {
		var arg, root big.Int
		arg.Mul(h, bigWad)
		root.Sqrt(&arg)
		root.Mul(big.NewInt(2), &root)
		return root.Sub(&root, big.NewInt(1_500_000_000_000_000_000))
	}
	var x big.Int
	x.Sub(h, cp.HJoin)
	xNorm := mulDiv(&x, bigWad, cp.Width)
	return new(big.Int).Add(cp.DJoin, mulDiv(cp.Width, cp.bezier4(xNorm), bigWad))
}

// strictAnchor -> (anchor, h, halfLaw, ok).
func (cp *CurveParams) strictAnchor(cv, debt, rWad *big.Int) (*big.Int, *big.Int, bool, bool) {
	if rWad.Cmp(cp.LeverageRatioWad) != 0 || cv.Cmp(bigMaxInput) > 0 || debt.Cmp(bigMaxInput) > 0 {
		return nil, nil, false, false
	}
	if cv.Sign() == 0 {
		return new(big.Int), new(big.Int), true, debt.Sign() == 0
	}
	if productGt(debt, cp.HWall, cv, cp.DWall) {
		return nil, nil, false, false
	}
	if !productGt(debt, cp.HJoin, cv, cp.DJoin) {
		anchor := halfLawAnchor(cv, debt)
		return anchor, new(big.Int), true, anchor.Sign() != 0
	}
	lo, hi := new(big.Int).Set(cp.HJoin), new(big.Int).Set(cp.HWall)
	var mid big.Int
	for lo.Cmp(hi) < 0 {
		mid.Add(lo, hi)
		mid.Rsh(&mid, 1)
		if productGt(debt, &mid, cv, cp.debtNormWad(&mid)) {
			lo.Add(&mid, big.NewInt(1))
		} else {
			hi.Set(&mid)
		}
	}
	var threeCv big.Int
	threeCv.Mul(big.NewInt(3), cv)
	var twoH big.Int
	twoH.Mul(big.NewInt(2), lo)
	anchor := mulDiv(&threeCv, bigWad, &twoH)
	return anchor, lo, false, anchor.Sign() != 0
}

// ---------- Normalized curve (Bézier) ----------

func lerpFloor(a, b, x *big.Int) *big.Int {
	if b.Cmp(a) >= 0 {
		var d big.Int
		d.Sub(b, a)
		return new(big.Int).Add(a, mulDiv(&d, x, bigWad))
	}
	var d big.Int
	d.Sub(a, b)
	return new(big.Int).Sub(a, mulDivUp(&d, x, bigWad))
}

func (cp *CurveParams) bezier4(x *big.Int) *big.Int {
	q := cp.BezierIntegral
	a0 := lerpFloor(q[0], q[1], x)
	a1 := lerpFloor(q[1], q[2], x)
	a2 := lerpFloor(q[2], q[3], x)
	a3 := lerpFloor(q[3], q[4], x)
	b0 := lerpFloor(a0, a1, x)
	b1 := lerpFloor(a1, a2, x)
	b2 := lerpFloor(a2, a3, x)
	c0 := lerpFloor(b0, b1, x)
	c1 := lerpFloor(b1, b2, x)
	return lerpFloor(c0, c1, x)
}

// bezier3 = CollRebalancerMath._bezier3: the phi = dD/dh cubic on the P0..P3 controls.
func (cp *CurveParams) bezier3(x *big.Int) *big.Int {
	p := cp.BezierPhi
	a0 := lerpFloor(p[0], p[1], x)
	a1 := lerpFloor(p[1], p[2], x)
	a2 := lerpFloor(p[2], p[3], x)
	b0 := lerpFloor(a0, a1, x)
	b1 := lerpFloor(a1, a2, x)
	return lerpFloor(b0, b1, x)
}

// phiWad = CollRebalancerMath._phiWad.
func (cp *CurveParams) phiWad(h *big.Int) *big.Int {
	if h.Cmp(cp.HJoin) <= 0 {
		var arg, root big.Int
		arg.Mul(h, bigWad)
		root.Sqrt(&arg)
		return new(big.Int).Quo(bigWadSq, &root)
	}
	var x big.Int
	x.Sub(h, cp.HJoin)
	return cp.bezier3(mulDiv(&x, bigWad, cp.Width))
}

// anchorAndBase = CollRebalancerMath.anchorAndBase -> (xAnchor, baseX): the value anchor
// and the stable-value marginal numerator M the post-fill acceptance is measured on.
func (cp *CurveParams) anchorAndBase(collateral, debt, price *big.Int) (*big.Int, *big.Int) {
	zero := func() (*big.Int, *big.Int) { return new(big.Int), new(big.Int) }
	cv, ok := markedValue(collateral, price)
	if !ok {
		return zero()
	}
	anchor, h, halfLaw, aok := cp.strictAnchor(cv, debt, cp.LeverageRatioWad)
	if aok {
		if anchor.Sign() == 0 {
			return zero()
		}
		if halfLaw {
			var baseX big.Int
			baseX.Add(anchor, debt)
			baseX.Quo(&baseX, big.NewInt(2))
			return anchor, &baseX
		}
		return anchor, mulDiv(cv, cp.phiWad(h), bigWad)
	}
	rec := cp.recoveryStateFor(cv, debt)
	if rec.ok {
		return rec.anchor, rec.baseX
	}
	return zero()
}

// ---------- Debt-cap / cv-required on an anchor ----------

func (cp *CurveParams) debtCapOnAnchor(anchor, cv *big.Int) (*big.Int, bool) {
	var threeCv, twoAnchor big.Int
	threeCv.Mul(big.NewInt(3), cv)
	twoAnchor.Mul(big.NewInt(2), anchor)
	h := mulDiv(&threeCv, bigWad, &twoAnchor)
	if h.Cmp(cp.HZero) < 0 || h.Cmp(cp.HWall) > 0 {
		return nil, false
	}
	if h.Cmp(cp.HJoin) <= 0 {
		var eightAnchor, root big.Int
		eightAnchor.Mul(big.NewInt(8), anchor)
		root.Sqrt(mulDiv(&eightAnchor, cv, big.NewInt(3)))
		if root.Cmp(anchor) <= 0 {
			return nil, false
		}
		return root.Sub(&root, anchor), true
	}
	return mulDiv(cv, cp.debtNormWad(h), h), true
}

func (cp *CurveParams) cvRequiredOnAnchor(anchor, debt *big.Int) (*big.Int, bool) {
	var threeDebt, twoAnchor big.Int
	threeDebt.Mul(big.NewInt(3), debt)
	twoAnchor.Mul(big.NewInt(2), anchor)
	if !productGt(&threeDebt, bigWad, &twoAnchor, cp.DJoin) {
		var s, s2 big.Int
		s.Add(anchor, debt)
		s2.Mul(&s, &s)
		var eightAnchor big.Int
		eightAnchor.Mul(big.NewInt(8), anchor)
		return mulDivUp(big.NewInt(3), &s2, &eightAnchor), true
	}
	if productGt(&threeDebt, bigWad, &twoAnchor, cp.DWall) {
		return nil, false
	}
	lo, hi := new(big.Int).Set(cp.HJoin), new(big.Int).Set(cp.HWall)
	var mid big.Int
	for lo.Cmp(hi) < 0 {
		mid.Add(lo, hi)
		mid.Rsh(&mid, 1)
		if productGt(&threeDebt, bigWad, &twoAnchor, cp.debtNormWad(&mid)) {
			lo.Add(&mid, big.NewInt(1))
		} else {
			hi.Set(&mid)
		}
	}
	var threeWad big.Int
	threeWad.Mul(big.NewInt(3), bigWad)
	return mulDivUp(&twoAnchor, lo, &threeWad), true
}

func (cp *CurveParams) deleverageSpread(cv, debt, posted *big.Int) *big.Int {
	var half big.Int
	half.Add(cv, big.NewInt(1))
	half.Quo(&half, big.NewInt(2))
	if debt.Cmp(&half) >= 0 && posted.Cmp(cp.RescueSpreadPpm) > 0 {
		return cp.RescueSpreadPpm
	}
	return posted
}

func (cp *CurveParams) postStrictAnchorAccepted(preAnchor, collateral, debt, price, rWad,
	spread *big.Int) bool {
	cv, ok := markedValue(collateral, price)
	if !ok {
		return false
	}
	postAnchor, _, _, aok := cp.strictAnchor(cv, debt, rWad)
	if !aok || (cv.Sign() != 0 && postAnchor.Sign() == 0) {
		return false
	}
	if spread.Sign() == 0 {
		return postAnchor.Cmp(preAnchor) >= 0
	}
	return postAnchor.Cmp(preAnchor) > 0
}

// ---------- Region tracking ----------

type region int

const (
	regionStrict region = iota
	// regionRecovery: past the honest CR wall. Only the conservative deleverage
	// continuation quotes here; leverage is strict-only by construction.
	regionRecovery
	regionOut
)

func (cp *CurveParams) stateRegion(collateral, debt, price *big.Int) region {
	cv, ok := markedValue(collateral, price)
	if !ok || cv.Sign() == 0 {
		return regionOut
	}
	if productGt(debt, cp.HWall, cv, cp.DWall) {
		return regionRecovery
	}
	anchor, _, _, aok := cp.strictAnchor(cv, debt, cp.LeverageRatioWad)
	if aok && anchor.Sign() != 0 {
		return regionStrict
	}
	return regionOut
}

// ---------- Public quotes (strict branch) ----------

// leverageQuote = CollRebalancerMath.leverageQuote -> (stableOut, newCollateral,
// newDebt). stableOut == 0 means the fill is rejected.
func (cp *CurveParams) leverageQuote(collateral, debt, price, rWad, spread,
	collateralIn *big.Int) (*big.Int, *big.Int, *big.Int) {
	reject := func() (*big.Int, *big.Int, *big.Int) { return new(big.Int), collateral, debt }
	if collateral.Sign() == 0 || price.Sign() == 0 || rWad.Cmp(cp.LeverageRatioWad) != 0 ||
		spread.Cmp(bigPpm) >= 0 || collateralIn.Sign() == 0 ||
		collateralIn.Cmp(bigMaxInput) > 0 || debt.Cmp(bigMaxInput) > 0 {
		return reject()
	}
	cv, ok := markedValue(collateral, price)
	if !ok || cv.Sign() == 0 {
		return reject()
	}
	anchor, _, _, aok := cp.strictAnchor(cv, debt, rWad)
	if !aok || anchor.Sign() == 0 {
		return reject()
	}
	newCollateral := new(big.Int).Add(collateral, collateralIn)
	newCv, ok := markedValue(newCollateral, price)
	if !ok || newCv.Cmp(cv) <= 0 {
		return reject()
	}
	debtCap, feasible := cp.debtCapOnAnchor(anchor, newCv)
	if !feasible || debtCap.Cmp(debt) <= 0 {
		return reject()
	}
	var grossOut, keep big.Int
	grossOut.Sub(debtCap, debt)
	keep.Sub(bigPpm, spread)
	stableOut := mulDiv(&grossOut, &keep, bigPpm)
	if stableOut.Sign() == 0 {
		return reject()
	}
	newDebt := new(big.Int).Add(debt, stableOut)
	if !cp.postStrictAnchorAccepted(anchor, newCollateral, newDebt, price, rWad, spread) {
		return reject()
	}
	return stableOut, newCollateral, newDebt
}

// deleverageQuote = CollRebalancerMath.deleverageQuote -> (collateralOut, newCollateral,
// newDebt). collateralOut == 0 means the fill is rejected. A solvent state beyond the
// honest wall quotes along its conservative recovery continuation (deleverage only;
// there is deliberately no recovery leverage path).
func (cp *CurveParams) deleverageQuote(collateral, debt, price, rWad, spread,
	stableIn *big.Int) (*big.Int, *big.Int, *big.Int) {
	reject := func() (*big.Int, *big.Int, *big.Int) { return new(big.Int), collateral, debt }
	if collateral.Sign() == 0 || price.Sign() == 0 || rWad.Cmp(cp.LeverageRatioWad) != 0 ||
		spread.Cmp(bigPpm) >= 0 || stableIn.Sign() == 0 || stableIn.Cmp(debt) > 0 ||
		stableIn.Cmp(bigMaxInput) > 0 || debt.Cmp(bigMaxInput) > 0 {
		return reject()
	}
	cv, ok := markedValue(collateral, price)
	if !ok || cv.Sign() == 0 {
		return reject()
	}
	anchor, _, _, aok := cp.strictAnchor(cv, debt, rWad)
	if !aok || anchor.Sign() == 0 {
		return cp.recoveryDeleverage(collateral, debt, cv, price, spread, stableIn)
	}
	newDebt := new(big.Int).Sub(debt, stableIn)
	cvRequired, feasible := cp.cvRequiredOnAnchor(anchor, newDebt)
	if !feasible {
		return reject()
	}
	collateralRequired := mulDivUp(cvRequired, bigWad, price)
	if collateralRequired.Cmp(collateral) >= 0 {
		return reject()
	}
	var outGross, keep big.Int
	outGross.Sub(collateral, collateralRequired)
	effSpread := cp.deleverageSpread(cv, debt, spread)
	keep.Sub(bigPpm, effSpread)
	collateralOut := mulDiv(&outGross, &keep, bigPpm)
	if collateralOut.Sign() == 0 {
		return reject()
	}
	newCollateral := new(big.Int).Sub(collateral, collateralOut)
	if !cp.postStrictAnchorAccepted(anchor, newCollateral, newDebt, price, rWad, effSpread) {
		return reject()
	}
	return collateralOut, newCollateral, newDebt
}

// ---------- Conservative recovery continuation (missed-wall deleverage) ----------

// recoveryState = CollRebalancerMath._recoveryState: the partial state on the wall
// anchor for a solvent state beyond the honest wall (y^2 = (rho - rhoW)/(1 - rhoW)).
type recoveryState struct {
	ok           bool
	anchor       *big.Int
	baseX        *big.Int
	y            *big.Int
	wallCv       *big.Int
	wallDebt     *big.Int
	stableToWall *big.Int
}

func (cp *CurveParams) recoveryStateFor(cv, debt *big.Int) recoveryState {
	var rec recoveryState
	if cv.Sign() == 0 || cv.Cmp(bigMaxInput) > 0 || debt.Cmp(bigMaxInput) > 0 ||
		debt.Cmp(cv) >= 0 || !productGt(debt, cp.HWall, cv, cp.DWall) {
		return rec
	}
	var numerator, denominator, wadSq, hwMinusDw big.Int
	numerator.Mul(debt, cp.HWall)
	numerator.Sub(&numerator, new(big.Int).Mul(cv, cp.DWall))
	hwMinusDw.Sub(cp.HWall, cp.DWall)
	denominator.Mul(cv, &hwMinusDw)
	wadSq.Mul(bigWad, bigWad)
	y := new(big.Int).Sqrt(mulDiv(&numerator, &wadSq, &denominator))
	if y.Sign() == 0 || y.Cmp(bigWad) >= 0 {
		return rec
	}
	var wadMinusY, wadPlusY big.Int
	wadMinusY.Sub(bigWad, y)
	wadPlusY.Add(bigWad, y)
	wallCv := mulDiv(cv, &wadMinusY, &wadPlusY)
	if wallCv.Sign() == 0 {
		return rec
	}
	wallDebt := mulDiv(wallCv, cp.DWall, cp.HWall)
	if wallDebt.Cmp(debt) >= 0 {
		return rec
	}
	wallAnchor, _, _, wallOk := cp.strictAnchor(wallCv, wallDebt, cp.LeverageRatioWad)
	if !wallOk || wallAnchor.Sign() == 0 {
		return rec
	}
	var cvMinusDebt big.Int
	cvMinusDebt.Sub(cv, debt)
	rec.anchor = wallAnchor
	rec.baseX = new(big.Int).Add(debt, mulDiv(&cvMinusDebt, y, bigWad))
	rec.y = y
	rec.wallCv = wallCv
	rec.wallDebt = wallDebt
	rec.stableToWall = new(big.Int).Sub(debt, wallDebt)
	rec.ok = rec.baseX.Sign() > 0 && rec.stableToWall.Sign() > 0
	return rec
}

// recoveryDebtAtY = CollRebalancerMath._recoveryDebtAtY.
func (cp *CurveParams) recoveryDebtAtY(wallCv, y *big.Int) *big.Int {
	z := wallCv
	if y.Sign() != 0 {
		var wadPlusY, wadMinusY big.Int
		wadPlusY.Add(bigWad, y)
		wadMinusY.Sub(bigWad, y)
		z = mulDivUp(wallCv, &wadPlusY, &wadMinusY)
	}
	// rhoNumerator = D_WALL*WAD^2 + (H_WALL - D_WALL)*y^2
	var wadSq, rhoNum, hwMinusDw, ySq, den big.Int
	wadSq.Mul(bigWad, bigWad)
	rhoNum.Mul(cp.DWall, &wadSq)
	hwMinusDw.Sub(cp.HWall, cp.DWall)
	ySq.Mul(y, y)
	rhoNum.Add(&rhoNum, new(big.Int).Mul(&hwMinusDw, &ySq))
	den.Mul(cp.HWall, &wadSq)
	return mulDiv(z, &rhoNum, &den)
}

// recoveryDeleverage = CollRebalancerMath._recoveryDeleverage: D(y) is strictly
// increasing, so a bounded integer bisection reconstructs the partial state on the same
// wall anchor.
func (cp *CurveParams) recoveryDeleverage(collateral, debt, cv, price, spread,
	stableIn *big.Int) (*big.Int, *big.Int, *big.Int) {
	reject := func() (*big.Int, *big.Int, *big.Int) { return new(big.Int), collateral, debt }
	rec := cp.recoveryStateFor(cv, debt)
	if !rec.ok || stableIn.Cmp(rec.stableToWall) > 0 {
		return reject()
	}
	newDebt := new(big.Int).Sub(debt, stableIn)
	yNew := new(big.Int)
	if stableIn.Cmp(rec.stableToWall) != 0 {
		lo := new(big.Int)
		hi := new(big.Int).Set(rec.y)
		var wadMinusOne big.Int
		wadMinusOne.Sub(bigWad, big.NewInt(1))
		if rec.y.Cmp(&wadMinusOne) < 0 {
			hi.Add(rec.y, big.NewInt(1))
		}
		if cp.recoveryDebtAtY(rec.wallCv, hi).Cmp(newDebt) < 0 {
			return reject()
		}
		var mid big.Int
		for lo.Cmp(hi) < 0 {
			mid.Add(lo, hi)
			mid.Rsh(&mid, 1)
			if cp.recoveryDebtAtY(rec.wallCv, &mid).Cmp(newDebt) < 0 {
				lo.Add(&mid, big.NewInt(1))
			} else {
				hi.Set(&mid)
			}
		}
		yNew = lo
	}
	invariantCv := rec.wallCv
	if yNew.Sign() != 0 {
		var wadPlusY, wadMinusY big.Int
		wadPlusY.Add(bigWad, yNew)
		wadMinusY.Sub(bigWad, yNew)
		invariantCv = mulDivUp(rec.wallCv, &wadPlusY, &wadMinusY)
	}
	collateralRequired := mulDivUp(invariantCv, bigWad, price)
	if collateralRequired.Cmp(collateral) >= 0 {
		return reject()
	}
	var outGross, keep big.Int
	outGross.Sub(collateral, collateralRequired)
	effSpread := cp.deleverageSpread(cv, debt, spread)
	keep.Sub(bigPpm, effSpread)
	collateralOut := mulDiv(&outGross, &keep, bigPpm)
	if collateralOut.Sign() == 0 {
		return reject()
	}
	newCollateral := new(big.Int).Sub(collateral, collateralOut)
	if !cp.postAnyAnchorAccepted(rec.anchor, newCollateral, newDebt, price, effSpread) {
		return reject()
	}
	return collateralOut, newCollateral, newDebt
}

// anchorBestEffort = CollRebalancerMath.anchorBestEffort: the strict C1 anchor, or the
// conservative recovery anchor for solvent missed-wall states.
func (cp *CurveParams) anchorBestEffort(cv, debt *big.Int) *big.Int {
	anchor, _, _, ok := cp.strictAnchor(cv, debt, cp.LeverageRatioWad)
	if ok {
		return anchor
	}
	rec := cp.recoveryStateFor(cv, debt)
	if rec.ok {
		return rec.anchor
	}
	return new(big.Int)
}

// postAnyAnchorAccepted = CollRebalancerMath._postAnyAnchorAccepted.
func (cp *CurveParams) postAnyAnchorAccepted(preAnchor, collateral, debt, price,
	spread *big.Int) bool {
	cv, ok := markedValue(collateral, price)
	if !ok {
		return false
	}
	postAnchor := cp.anchorBestEffort(cv, debt)
	if postAnchor.Sign() == 0 {
		return false
	}
	if spread.Sign() == 0 {
		return postAnchor.Cmp(preAnchor) >= 0
	}
	return postAnchor.Cmp(preAnchor) > 0
}

// isStateSafe = CollRebalancerMath.isStateSafe: the fill-acceptance predicate the
// rebalancer applies on top of every quote (anchor non-decrease vs the pre-fill anchor;
// recovery states admitted for decrease paths).
func (cp *CurveParams) isStateSafe(collateral, debt, price, requiredXAnchor *big.Int) bool {
	cv, ok := markedValue(collateral, price)
	if !ok {
		return false
	}
	if cv.Sign() == 0 {
		return collateral.Sign() == 0 && debt.Sign() == 0 && requiredXAnchor.Sign() == 0
	}
	anchor, _, _, sok := cp.strictAnchor(cv, debt, cp.LeverageRatioWad)
	if sok {
		return anchor.Sign() > 0 && anchor.Cmp(requiredXAnchor) >= 0
	}
	rec := cp.recoveryStateFor(cv, debt)
	return rec.ok && rec.anchor.Cmp(requiredXAnchor) >= 0
}

// xAnchorForState mirrors the anchor half of CollRebalancerMath.anchorAndBase — the
// pre-fill anchor the rebalancer holds fills against (FillContext.xAnchor).
func (cp *CurveParams) xAnchorForState(collateral, debt, price *big.Int) *big.Int {
	cv, ok := markedValue(collateral, price)
	if !ok {
		return new(big.Int)
	}
	return cp.anchorBestEffort(cv, debt)
}

// ---------- Post-fill acceptance (_assertFillSafe) ----------

// postVaultLeverage / postVaultDeleverage return the CollVault's post-fill
// (totalAssets, totalSupply). Leverage mints previewMint assets against the minted
// shares. Deleverage burns only the NET shares — BaseCollateralVault._applyShareFee
// re-mints the fee shares to the fee receiver — and moves assets by the raw
// netShares*totalAssets/totalSupply ratio (no ERC-4626 virtual offsets).
func (s *VaultState) postVaultLeverage(sharesIn *big.Int) (*big.Int, *big.Int) {
	return new(big.Int).Add(s.CvTotalAssets, s.cvConvertToAssets(sharesIn, true)),
		new(big.Int).Add(s.CvTotalSupply, sharesIn)
}

func (s *VaultState) postVaultDeleverage(sharesOut *big.Int) (*big.Int, *big.Int) {
	shareFee := mulDivUp(sharesOut, s.WithdrawFeeBp, bigBp)
	var netShares big.Int
	netShares.Sub(sharesOut, shareFee)
	assetDelta := new(big.Int)
	if s.CvTotalSupply.Sign() != 0 {
		assetDelta = mulDiv(&netShares, s.CvTotalAssets, s.CvTotalSupply)
	}
	return new(big.Int).Sub(s.CvTotalAssets, assetDelta),
		new(big.Int).Sub(s.CvTotalSupply, &netShares)
}

// reservationValueAt = CollateralRebalancer._reservationValue on the given vault words:
// collVault.convertToAssets(collVaultShares) valued at rvps, per collateral share.
func reservationValueAt(collVaultShares, cvTotalAssets, cvTotalSupply *big.Int,
	decimalsOffset uint8, rvpsWad *big.Int) *big.Int {
	if collVaultShares.Sign() == 0 {
		return new(big.Int)
	}
	var num, den big.Int
	num.Add(cvTotalAssets, big.NewInt(1))
	den.Exp(big.NewInt(10), big.NewInt(int64(decimalsOffset)), nil)
	den.Add(cvTotalSupply, &den)
	almShares := mulDiv(collVaultShares, &num, &den) // convertToAssets, floor
	value := mulDiv(almShares, rvpsWad, bigWad)
	return mulDiv(value, bigWad, collVaultShares)
}

// postReservation is the reservation value the venue would report after the fill, or the
// pre-fill value when rvps is not tracked (legacy persisted states).
func (s *VaultState) postReservation(newColl, postAssets, postSupply *big.Int) *big.Int {
	if s.RvpsWad == nil || s.RvpsWad.Sign() <= 0 {
		return s.PriceWad
	}
	return reservationValueAt(newColl, postAssets, postSupply, s.CvDecimalsOffset, s.RvpsWad)
}

// computeCR = PropMath._computeCR: coll*price/debt floored; nil means infinite (debt 0).
func computeCR(coll, debt, price *big.Int) *big.Int {
	if debt.Sign() == 0 {
		return nil
	}
	return mulDiv(coll, price, debt)
}

// fillOutcomeAccepted ports CollateralRebalancer._assertFillSafe — the post-EXECUTION
// predicate applied on top of every quote (the quote-level isStateSafe re-check against
// the PRE-fill reservation already lives in leverageQuoteChecked/deleverageQuoteChecked):
//
//  1. isStateSafe on the RECOMPUTED post-fill reservation value (the fill's own CollVault
//     mint/burn shifts convertToAssets; rvps itself is invariant under the pro-rata ALM leg),
//  2. post anchor >= pre anchor and baseX != 0,
//  3. the post-fill physical-CR floor (leverage only; modelled in leverageOk),
//  4. the +-PRICE_BAND_NUM price band on the post internal value,
//  5. the ICR floor: post ICR below MCR*1.2 only accepted when not decreasing.
//
// Words persisted before rvps/MCR tracking degrade gracefully: the reservation recompute
// falls back to the pre-fill value and the ICR leg is skipped when unreadable.
func (cp *CurveParams) fillOutcomeAccepted(s *VaultState, isLeverage bool,
	shares, newColl, newDebt *big.Int) bool {
	var postAssets, postSupply *big.Int
	if isLeverage {
		postAssets, postSupply = s.postVaultLeverage(shares)
	} else {
		postAssets, postSupply = s.postVaultDeleverage(shares)
	}
	resPost := s.postReservation(newColl, postAssets, postSupply)

	preAnchor := cp.xAnchorForState(s.Collateral, s.Debt, s.PriceWad)
	if !cp.isStateSafe(newColl, newDebt, resPost, preAnchor) {
		return false
	}
	xAfter, baseXAfter := cp.anchorAndBase(newColl, newDebt, resPost)
	if xAfter.Cmp(preAnchor) < 0 || baseXAfter.Sign() == 0 {
		return false
	}
	if newColl.Sign() == 0 {
		return false
	}
	internalValue := mulDiv(baseXAfter, bigWad, newColl)
	var upper, lower big.Int
	upper.Mul(resPost, bigPriceBandNum)
	lower.Quo(resPost, bigPriceBandNum)
	if internalValue.Cmp(&upper) > 0 || internalValue.Cmp(&lower) < 0 {
		return false
	}

	if s.IcrPriceWad != nil && s.IcrPriceWad.Sign() > 0 && s.McrWad != nil && s.McrWad.Sign() > 0 {
		icrAfter := computeCR(newColl, newDebt, s.IcrPriceWad)
		if icrAfter != nil { // debt 0 = infinite ICR, always safe
			floorCr := mulDiv(s.McrWad, bigIcrFloorFactor, bigWad)
			if icrAfter.Cmp(floorCr) < 0 {
				icrBefore := computeCR(s.Collateral, s.Debt, s.IcrPriceWad)
				// icrBefore == nil (infinite) always exceeds icrAfter
				if icrBefore == nil || icrAfter.Cmp(icrBefore) < 0 {
					return false
				}
			}
		}
	}
	return true
}

// ---------- previewTokenAmounts (CollVault 4626 -> ALM proportional split) ----------

// cvConvertToAssets = ERC4626Upgradeable._convertToAssets:
// shares*(totalAssets+1)/(totalSupply+10^offset) with the given rounding.
func (s *VaultState) cvConvertToAssets(shares *big.Int, up bool) *big.Int {
	var num, den big.Int
	num.Add(s.CvTotalAssets, big.NewInt(1))
	den.Exp(big.NewInt(10), big.NewInt(int64(s.CvDecimalsOffset)), nil)
	den.Add(s.CvTotalSupply, &den)
	if up {
		return mulDivUp(shares, &num, &den)
	}
	return mulDiv(shares, &num, &den)
}

// previewTokenAmounts = Swapper._previewTokenAmounts -> (stable, volatile, ok);
// ok=false is the NothingToFill revert.
func (s *VaultState) previewTokenAmounts(collVaultShares *big.Int, mint bool) (*big.Int, *big.Int, bool) {
	var almShares *big.Int
	if mint {
		almShares = s.cvConvertToAssets(collVaultShares, true) // previewMint
	} else {
		shareFee := mulDivUp(collVaultShares, s.WithdrawFeeBp, bigBp) // feeOnRaw, Up
		var net big.Int
		net.Sub(collVaultShares, shareFee)
		almShares = s.cvConvertToAssets(&net, false) // previewRedeem
	}
	if s.AlmSupply.Sign() == 0 || almShares.Sign() == 0 {
		return nil, nil, false
	}
	if mint {
		return mulDivUp(s.AlmStableReserve, almShares, s.AlmSupply),
			mulDivUp(s.AlmVolatileReserve, almShares, s.AlmSupply), true
	}
	return mulDiv(s.AlmStableReserve, almShares, s.AlmSupply),
		mulDiv(s.AlmVolatileReserve, almShares, s.AlmSupply), true
}

// ---------- Composite quotes (the values swap legs settle by) ----------

// quoteLeverageAt: net stable (e.g. NECT) the caller receives for minting sharesIn CollVault
// shares — gross debt draw minus the stable leg pulled to mint the shares.
func (cp *CurveParams) quoteLeverageAt(s *VaultState, sharesIn *big.Int) (*big.Int, bool) {
	grossStableOut, _, _ := cp.leverageQuoteChecked(s, sharesIn)
	stableIn, _, ok := s.previewTokenAmounts(sharesIn, true)
	if !ok {
		return nil, false
	}
	if grossStableOut.Cmp(stableIn) > 0 {
		return new(big.Int).Sub(grossStableOut, stableIn), true
	}
	return new(big.Int), true
}

// leverageQuoteChecked / deleverageQuoteChecked wrap the raw quotes with the
// rebalancer's fill-acceptance predicate: after every quote the contract requires
// isStateSafe(newState, preFillAnchor) and reverts NothingToFill otherwise. A zero
// first return means the fill is rejected.
func (cp *CurveParams) leverageQuoteChecked(s *VaultState, collateralIn *big.Int) (*big.Int, *big.Int, *big.Int) {
	out, newColl, newDebt := cp.leverageQuote(s.Collateral, s.Debt, s.PriceWad,
		cp.LeverageRatioWad, s.SpreadPpm, collateralIn)
	if out.Sign() == 0 {
		return out, newColl, newDebt
	}
	preAnchor := cp.xAnchorForState(s.Collateral, s.Debt, s.PriceWad)
	if !cp.isStateSafe(newColl, newDebt, s.PriceWad, preAnchor) {
		return new(big.Int), s.Collateral, s.Debt
	}
	if !cp.fillOutcomeAccepted(s, true, collateralIn, newColl, newDebt) {
		return new(big.Int), s.Collateral, s.Debt
	}
	return out, newColl, newDebt
}

func (cp *CurveParams) deleverageQuoteChecked(s *VaultState, stableIn *big.Int) (*big.Int, *big.Int, *big.Int) {
	out, newColl, newDebt := cp.deleverageQuote(s.Collateral, s.Debt, s.PriceWad,
		cp.LeverageRatioWad, s.SpreadPpm, stableIn)
	if out.Sign() == 0 {
		return out, newColl, newDebt
	}
	preAnchor := cp.xAnchorForState(s.Collateral, s.Debt, s.PriceWad)
	if !cp.isStateSafe(newColl, newDebt, s.PriceWad, preAnchor) {
		return new(big.Int), s.Collateral, s.Debt
	}
	if !cp.fillOutcomeAccepted(s, false, out, newColl, newDebt) {
		return new(big.Int), s.Collateral, s.Debt
	}
	return out, newColl, newDebt
}

// deleverageLegsAt: both freed legs at a gross stableDebtIn -> (stableOut, volatileOut).
// The net the caller fronts is stableDebtIn - stableOut. ok=false when the fill is
// rejected or the preview degenerates.
func (cp *CurveParams) deleverageLegsAt(s *VaultState, stableDebtIn *big.Int) (*big.Int, *big.Int, bool) {
	sharesOut, _, _ := cp.deleverageQuoteChecked(s, stableDebtIn)
	if sharesOut.Sign() == 0 {
		return nil, nil, false
	}
	return s.previewTokenAmounts(sharesOut, false)
}

// ---------- Max-lot sizing ----------

// leverageOk: a leverage fill of collateralIn quotes, passes the fill-acceptance
// predicate AND clears the physical-CR floor.
//
// The floor is evaluated on the POST-fill book, exactly as CollateralRebalancer
// re-checks it after the adjustment. Two things move and both must be carried or the
// bound comes out wrong:
//
//   - the ALM grows by the minted legs, and share minting is pro-rata, so its
//     reference-marked reserves scale by the same factor its supply does. That factor
//     cancels between the position's share of the book and the book's own total, which
//     is why `totalPhysicalValue` and `AlmSupply` may both stay PRE-fill here.
//   - the CollVault does NOT cancel: its assets grow by the minted ALM shares and its
//     supply by the minted CollVault shares, and ERC-4626's virtual offsets make the
//     conversion sensitive to both. Converting the post-fill collateral on the PRE-fill
//     vault is what left the old bound ~3.5% short of what the venue accepts.
func (cp *CurveParams) leverageOk(s *VaultState, collateralIn, totalPhysicalValue *big.Int) bool {
	out, newColl, newDebt := cp.leverageQuoteChecked(s, collateralIn)
	if out.Sign() == 0 {
		return false
	}
	if newDebt.Sign() == 0 {
		return true
	}
	almSharesMinted := s.cvConvertToAssets(collateralIn, true) // previewMint
	post := *s
	post.CvTotalAssets = new(big.Int).Add(s.CvTotalAssets, almSharesMinted)
	post.CvTotalSupply = new(big.Int).Add(s.CvTotalSupply, collateralIn)
	almShares := post.cvConvertToAssets(newColl, false)
	positionPhysicalValue := mulDiv(totalPhysicalValue, almShares, s.AlmSupply)
	requiredValue := mulDivUp(newDebt, cp.PhysicalCrFloorWad, bigWad)
	return positionPhysicalValue.Cmp(requiredValue) >= 0
}

// maxLeverageShares: largest collateralIn that fills and clears the physical-CR floor.
// Boundary bisection; zero when even size 1 fails.
func (cp *CurveParams) maxLeverageShares(s *VaultState) *big.Int {
	if s.AlmSupply.Sign() == 0 || s.RefRawReferenceWad.Sign() == 0 {
		return new(big.Int)
	}
	totalPhysicalValue := new(big.Int).Add(s.RefStableReserve,
		mulDiv(s.RefAssetReserve, s.RefRawReferenceWad, bigWad))
	one := big.NewInt(1)
	if !cp.leverageOk(s, one, totalPhysicalValue) {
		return new(big.Int)
	}
	lo := new(big.Int).Set(one)
	hi := mulDiv(bigMaxInput, bigWad, s.PriceWad)
	var mid, span big.Int
	for lo.Cmp(hi) < 0 {
		span.Sub(hi, lo)
		span.Add(&span, one)
		span.Quo(&span, big.NewInt(2))
		mid.Add(lo, &span)
		if cp.leverageOk(s, &mid, totalPhysicalValue) {
			lo.Set(&mid)
		} else {
			hi.Sub(&mid, one)
		}
	}
	return lo
}

// debtRepayCeiling: the largest repayment the CDP itself will accept, independent of
// the curve. BorrowerOperations rejects any repayment leaving the position's NET debt
// (total debt minus the per-position gas compensation) below minNetDebt, so the bound
// is debt - gasComp - minNetDebt. Validated at the wei on-chain: one wei under fills,
// one wei over reverts. Returns the full debt when the deployment does not report the
// two values, so an unknown CDP never silently shrinks the lot.
func (s *VaultState) debtRepayCeiling() *big.Int {
	if s.MinNetDebt == nil || s.MinNetDebt.Sign() == 0 {
		return new(big.Int).Set(s.Debt)
	}
	gasComp := s.DebtGasCompensation
	if gasComp == nil {
		gasComp = new(big.Int)
	}
	var ceiling big.Int
	ceiling.Sub(s.Debt, gasComp)
	ceiling.Sub(&ceiling, s.MinNetDebt)
	if ceiling.Sign() < 0 {
		return new(big.Int)
	}
	if ceiling.Cmp(s.Debt) > 0 {
		return new(big.Int).Set(s.Debt)
	}
	return &ceiling
}

// maxDeleverageIn: largest gross stableIn that still quotes (strict branch or the
// conservative recovery continuation), passes the fill-acceptance predicate AND stays
// within the CDP's minimum-net-debt bound.
func (cp *CurveParams) maxDeleverageIn(s *VaultState) *big.Int {
	if s.Debt.Sign() == 0 {
		return new(big.Int)
	}
	ceiling := s.debtRepayCeiling()
	if ceiling.Sign() == 0 {
		return new(big.Int)
	}
	one := big.NewInt(1)
	lo, hi := new(big.Int).Set(one), new(big.Int).Set(ceiling)
	var mid, span big.Int
	for lo.Cmp(hi) < 0 {
		span.Sub(hi, lo)
		span.Add(&span, one)
		span.Quo(&span, big.NewInt(2))
		mid.Add(lo, &span)
		out, _, _ := cp.deleverageQuoteChecked(s, &mid)
		if out.Sign() != 0 {
			lo.Set(&mid)
		} else {
			hi.Sub(&mid, one)
		}
	}
	// even size 1 may not quote
	out, _, _ := cp.deleverageQuoteChecked(s, lo)
	if out.Sign() == 0 {
		return new(big.Int)
	}
	return lo
}

// ---------- Inversions used by CalcAmountOut ----------

// sharesForVolatileIn: largest sharesIn whose mint-leg volatile requirement fits within
// amountIn (previewTokenAmounts' volatile leg is monotone nondecreasing in shares).
// The result is additionally capped by maxShares (the physical-CR-floor cap).
func (s *VaultState) sharesForVolatileIn(amountIn, maxShares *big.Int) *big.Int {
	if amountIn.Sign() <= 0 || maxShares.Sign() <= 0 {
		return new(big.Int)
	}
	volAtOne, ok := volatileForShares(s, big.NewInt(1))
	if !ok || volAtOne.Cmp(amountIn) > 0 {
		return new(big.Int)
	}
	one := big.NewInt(1)
	lo, hi := new(big.Int).Set(one), new(big.Int).Set(maxShares)
	var mid, span big.Int
	for lo.Cmp(hi) < 0 {
		span.Sub(hi, lo)
		span.Add(&span, one)
		span.Quo(&span, big.NewInt(2))
		mid.Add(lo, &span)
		vol, ok := volatileForShares(s, &mid)
		if ok && vol.Cmp(amountIn) <= 0 {
			lo.Set(&mid)
		} else {
			hi.Sub(&mid, one)
		}
	}
	return lo
}

func volatileForShares(s *VaultState, shares *big.Int) (*big.Int, bool) {
	_, volatile, ok := s.previewTokenAmounts(shares, true)
	if !ok {
		return nil, false
	}
	return volatile, true
}

// grossForNetStableIn: largest gross stableDebtIn (within [1, maxGross], where maxGross
// must already be a VALID quoting size — pass maxDeleverageIn) whose NET front
// (gross - freed stable) fits within netBudget. The net is nondecreasing in the gross,
// so the predicate is monotone; sub-wei grosses that round to a rejected quote are
// treated by their gross (net <= gross always), and the winner is re-validated.
func (cp *CurveParams) grossForNetStableIn(s *VaultState, netBudget, maxGross *big.Int) *big.Int {
	if netBudget.Sign() <= 0 || maxGross.Sign() <= 0 {
		return new(big.Int)
	}
	fitsNet := func(gross *big.Int) bool {
		stableOut, _, ok := cp.deleverageLegsAt(s, gross)
		if !ok {
			// only tiny grosses are invalid below maxGross; net <= gross bounds them
			return gross.Cmp(netBudget) <= 0
		}
		var net big.Int
		net.Sub(gross, stableOut)
		return net.Cmp(netBudget) <= 0
	}
	one := big.NewInt(1)
	lo, hi := new(big.Int).Set(one), new(big.Int).Set(maxGross)
	var mid, span big.Int
	for lo.Cmp(hi) < 0 {
		span.Sub(hi, lo)
		span.Add(&span, one)
		span.Quo(&span, big.NewInt(2))
		mid.Add(lo, &span)
		if fitsNet(&mid) {
			lo.Set(&mid)
		} else {
			hi.Sub(&mid, one)
		}
	}
	if _, _, ok := cp.deleverageLegsAt(s, lo); !ok {
		return new(big.Int)
	}
	return lo
}

// ---------- Direction / full-lot views ----------

// rebalanceState: the pending realignment direction (0 none / 1 leverage / 2
// deleverage) and the MAX lot (in / out).
func (cp *CurveParams) rebalanceState(s *VaultState) (int, *big.Int, *big.Int) {
	zero := func() (int, *big.Int, *big.Int) { return 0, new(big.Int), new(big.Int) }
	if s.Collateral.Sign() == 0 || s.PriceWad.Sign() == 0 {
		return zero()
	}
	one := big.NewInt(1)
	levProbe, _, _ := cp.leverageQuoteChecked(s, one)
	if levProbe.Sign() != 0 {
		x := cp.maxLeverageShares(s)
		if x.Sign() == 0 {
			return zero()
		}
		stableOut, _, _ := cp.leverageQuoteChecked(s, x)
		return 1, x, stableOut
	}
	if s.Debt.Sign() != 0 {
		probe, _, _ := cp.deleverageQuoteChecked(s, one)
		if probe.Sign() != 0 {
			sIn := cp.maxDeleverageIn(s)
			if sIn.Sign() == 0 {
				return zero()
			}
			out, _, _ := cp.deleverageQuoteChecked(s, sIn)
			return 2, sIn, out
		}
	}
	return zero()
}
