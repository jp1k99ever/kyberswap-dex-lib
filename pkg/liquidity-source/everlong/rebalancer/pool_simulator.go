package everlongrebalancer

import (
	"math/big"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/samber/lo"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/util/bignumber"
)

// PoolSimulator prices the Everlong CollateralRebalancer settlement venue
// (CollateralRebalancerSwapper): a two-token venue between the stable (e.g. NECT) and
// the volatile (e.g. WBTC) leg whose price comes from the deployed CollateralRebalancer CR
// bonding curve, not an AMM invariant.
//
//   - volatile -> stable is LEVERAGE (swapVolatileForStable): the caller's volatile leg
//     mints CollVault shares, the position draws debt, the caller receives net stable.
//   - stable -> volatile is DELEVERAGE (swapStableForVolatile): the caller fronts net
//     stable to repay debt, CollVault shares burn and the freed volatile leg pays out.
//
// Both directions fill whenever the curve accepts them — the non-favored direction
// simply prices ~the spread worse (it does NOT revert on-chain). All math is a
// wei-exact port of the deployed contracts (see math.go).
type PoolSimulator struct {
	pool.Pool
	StaticExtra StaticExtra
	Extra       Extra

	// Meta/base coupling with the everlong-cvamm pool the ALM adapter wraps: a CVAMM
	// fill in the same route moves the reserves this quote prices from. The baseline is
	// the base reserves at THIS snapshot's block — only movement since then folds, so
	// tracker refreshes never double count. nil basePool = uncoupled.
	basePool      pool.IPoolSimulator
	baseStable0   *big.Int
	baseVolatile0 *big.Int
	// Accounted (idle-excluded) baseline: totals minus accounted isolates the fee leg a
	// base fill parks in idle — the only channel a swap moves rvps through.
	baseAccStable0   *big.Int
	baseAccVolatile0 *big.Int
}

var _ = pool.RegisterFactoryMeta(DexType, NewPoolSimulatorWithBases)

// NewPoolSimulatorWithBases wires the underlying everlong-cvamm sim (resolved at listing
// into StaticExtra.UnderlyingCvamm) as this pool's base. FAIL-CLOSED: an absent base
// would silently restore the uncoupled (double-counting) simulator, so construction
// errors instead — the pool routes only alongside its base.
func NewPoolSimulatorWithBases(p entity.Pool, basePoolMap map[string]pool.IPoolSimulator) (*PoolSimulator, error) {
	sim, err := NewPoolSimulator(p)
	if err != nil {
		return nil, err
	}
	if sim.StaticExtra.UnderlyingCvamm == "" {
		return nil, ErrUnderlyingCvamm
	}
	base, ok := basePoolMap[strings.ToLower(sim.StaticExtra.UnderlyingCvamm)]
	if !ok {
		base, ok = basePoolMap[sim.StaticExtra.UnderlyingCvamm]
	}
	if !ok {
		return nil, ErrMissingBasePool
	}
	sim.wireBase(base)
	if sim.basePool == nil {
		return nil, ErrMissingBasePool
	}
	return sim, nil
}

// totalReserver exposes the base's TOTAL inventory (accounted + idled fees) — the
// domain this sim's getTotalAmounts-based words live in. Swap fees leave the priced
// reserve but stay inside the ALM, so accounted deltas overstate the outflow.
type totalReserver interface {
	GetTotalReserves() []*big.Int
}

func baseReserves(base pool.IPoolSimulator) []*big.Int {
	if tr, ok := base.(totalReserver); ok {
		return tr.GetTotalReserves()
	}
	return base.GetReserves()
}

// wireBase adopts the base sim and pins the baseline at its current (snapshot)
// total reserves — everlong-cvamm order is [stable, volatile].
func (s *PoolSimulator) wireBase(base pool.IPoolSimulator) {
	res := baseReserves(base)
	if len(res) != 2 || res[0] == nil || res[1] == nil {
		return
	}
	s.basePool = base
	s.baseStable0 = new(big.Int).Set(res[0])
	s.baseVolatile0 = new(big.Int).Set(res[1])
	if acc := base.GetReserves(); len(acc) == 2 && acc[0] != nil && acc[1] != nil {
		s.baseAccStable0 = new(big.Int).Set(acc[0])
		s.baseAccVolatile0 = new(big.Int).Set(acc[1])
	}
}

func (s *PoolSimulator) GetBasePools() []pool.IPoolSimulator {
	if s.basePool == nil {
		return nil
	}
	return []pool.IPoolSimulator{s.basePool}
}

// SetBasePool swaps the pointer only — the baseline stays at snapshot time so
// in-route deltas keep applying. First wiring also pins the baseline.
func (s *PoolSimulator) SetBasePool(base pool.IPoolSimulator) {
	if base == nil || !strings.EqualFold(base.GetAddress(), s.StaticExtra.UnderlyingCvamm) {
		return
	}
	if s.basePool == nil {
		s.wireBase(base)
		return
	}
	s.basePool = base
}

// baseDeltas: the base's movement since this snapshot — total deltas for the reserve
// legs, plus the idle-only fee legs that value the rvps shift.
func (s *PoolSimulator) baseDeltas() (dStable, dVolatile, feeStable, feeVolatile *big.Int, moved bool) {
	if s.basePool == nil || s.baseStable0 == nil {
		return nil, nil, nil, nil, false
	}
	res := baseReserves(s.basePool)
	if len(res) != 2 || res[0] == nil || res[1] == nil {
		return nil, nil, nil, nil, false
	}
	dStable = new(big.Int).Sub(res[0], s.baseStable0)
	dVolatile = new(big.Int).Sub(res[1], s.baseVolatile0)
	feeStable, feeVolatile = new(big.Int), new(big.Int)
	if acc := s.basePool.GetReserves(); s.baseAccStable0 != nil &&
		len(acc) == 2 && acc[0] != nil && acc[1] != nil {
		feeStable.Sub(dStable, new(big.Int).Sub(acc[0], s.baseAccStable0))
		feeVolatile.Sub(dVolatile, new(big.Int).Sub(acc[1], s.baseAccVolatile0))
	}
	return dStable, dVolatile, feeStable, feeVolatile,
		dStable.Sign() != 0 || dVolatile.Sign() != 0
}

// liquidityDeltaApplier is the reverse-coupling surface of the everlong-cvamm sim
// (loose interface — no hard package dependency).
type liquidityDeltaApplier interface {
	ApplyLiquidityDelta(sharesDelta, supplyBefore *big.Int) (dStable, dVolatile *big.Int)
}

// pushFillToBase folds THIS fill's ALM mint/burn into the base CVAMM sim (pro-rata
// reserves + kappa), so a later direct CVAMM quote in the route isn't stale. The
// baseline advances by the applied deltas: the movement originated here and is already
// in this sim's own state.
func (s *PoolSimulator) pushFillToBase(sharesDelta, supplyBefore *big.Int) {
	applier, ok := s.basePool.(liquidityDeltaApplier)
	if !ok || s.baseStable0 == nil {
		return
	}
	dS, dV := applier.ApplyLiquidityDelta(sharesDelta, supplyBefore)
	if dS == nil || dV == nil {
		return
	}
	// Accounted-only movement (idle untouched): both baselines advance identically.
	s.baseStable0 = new(big.Int).Add(s.baseStable0, dS)
	s.baseVolatile0 = new(big.Int).Add(s.baseVolatile0, dV)
	if s.baseAccStable0 != nil {
		s.baseAccStable0 = new(big.Int).Add(s.baseAccStable0, dS)
		s.baseAccVolatile0 = new(big.Int).Add(s.baseAccVolatile0, dV)
	}
}

// foldBaseDeltas shifts the ALM and reference legs by the base movement (both mark the
// same physical totals). The fee legs land in idle, which is the only channel a swap
// moves rvps through — the curve mark recomputes from swap-invariant words.
func foldBaseDeltas(e *Extra, dStable, dVolatile, feeStable, feeVolatile *big.Int) {
	if e.AlmStableReserve != nil {
		e.AlmStableReserve = new(big.Int).Add(e.AlmStableReserve, dStable)
	}
	if e.AlmVolatileReserve != nil {
		e.AlmVolatileReserve = new(big.Int).Add(e.AlmVolatileReserve, dVolatile)
	}
	if e.RefStableReserve != nil {
		e.RefStableReserve = new(big.Int).Add(e.RefStableReserve, dStable)
	}
	if e.RefAssetReserve != nil {
		e.RefAssetReserve = new(big.Int).Add(e.RefAssetReserve, dVolatile)
	}
	// A base fill's fee legs are exactly what lands in idle.
	if feeStable != nil && e.AlmIdleStable != nil {
		e.AlmIdleStable = new(big.Int).Add(e.AlmIdleStable, feeStable)
	}
	if feeVolatile != nil && e.AlmIdleVolatile != nil {
		e.AlmIdleVolatile = new(big.Int).Add(e.AlmIdleVolatile, feeVolatile)
	}
	if feeStable == nil || feeVolatile == nil ||
		e.RvpsWad == nil || e.RvpsWad.Sign() <= 0 ||
		e.AlmResvPriceWad == nil || e.AlmResvPriceWad.Sign() <= 0 ||
		e.AlmSupply == nil || e.AlmSupply.Sign() <= 0 {
		return
	}
	feeValue := new(big.Int).Add(feeStable, mulDiv(feeVolatile, e.AlmResvPriceWad, bigWad))
	if feeValue.Sign() <= 0 {
		return
	}
	rvps := new(big.Int).Add(e.RvpsWad, mulDiv(feeValue, bigWad, e.AlmSupply))
	if e.PriceWad != nil && e.Collateral != nil && e.CvTotalAssets != nil && e.CvTotalSupply != nil {
		pre := reservationValueAt(e.Collateral, e.CvTotalAssets, e.CvTotalSupply, e.CvDecimalsOffset, e.RvpsWad)
		post := reservationValueAt(e.Collateral, e.CvTotalAssets, e.CvTotalSupply, e.CvDecimalsOffset, rvps)
		e.PriceWad = new(big.Int).Add(e.PriceWad, new(big.Int).Sub(post, pre))
	}
	e.RvpsWad = rvps
}

func NewPoolSimulator(p entity.Pool) (*PoolSimulator, error) {
	var extra Extra
	if err := json.Unmarshal([]byte(p.Extra), &extra); err != nil {
		return nil, err
	}
	var staticExtra StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &staticExtra); err != nil {
		return nil, err
	}
	if extra.CvDecimalsOffset == 0 {
		extra.CvDecimalsOffset = staticExtra.CvDecimalsOffset
	}
	projectInterestDebt(&extra)

	return &PoolSimulator{
		Pool: pool.Pool{Info: pool.PoolInfo{
			Address:     p.Address,
			Exchange:    p.Exchange,
			Type:        p.Type,
			Tokens:      lo.Map(p.Tokens, func(e *entity.PoolToken, _ int) string { return e.Address }),
			Reserves:    lo.Map(p.Reserves, func(e string, _ int) *big.Int { return bignumber.NewBig(e) }),
			BlockNumber: p.BlockNumber,
		}},
		StaticExtra: staticExtra,
		Extra:       extra,
	}, nil
}

// projectInterestDebt reproduces PositionManager.getPositionCollAndDebt's interest leg at
// construction time: once governance enables borrow interest the position's debt compounds
// per second with NO event, so the tracker snapshot goes stale between refreshes. The
// projection is debt = floor(rawDebt * currentIndex / positionIndex) + pendingDebtReward,
// with currentIndex = activeIndex * (1 + deltaT*rate/1e27) (_calculateInterestIndex).
// Computed once here so repeated quoting stays deterministic. A zero rate — the current
// deployment, where leverage is refused outright otherwise — leaves the snapshot as-is.
func projectInterestDebt(e *Extra) {
	if e.InterestRate == nil || e.InterestRate.Sign() <= 0 ||
		e.PosRawDebt == nil || e.PosInterestIndex == nil || e.PosInterestIndex.Sign() <= 0 ||
		e.ActiveInterestIndex == nil || e.LastActiveIndexUpdate == nil {
		return
	}
	idx := new(big.Int).Set(e.ActiveInterestIndex)
	if now := time.Now().Unix(); now > e.LastActiveIndexUpdate.Int64() {
		var factor big.Int
		factor.Sub(big.NewInt(now), e.LastActiveIndexUpdate)
		factor.Mul(&factor, e.InterestRate)
		idx.Add(idx, mulDiv(idx, &factor, bigInterestRayPrec))
	}
	debt := mulDiv(e.PosRawDebt, idx, e.PosInterestIndex)
	if e.PendingDebtReward != nil {
		debt.Add(debt, e.PendingDebtReward)
	}
	if e.Debt == nil || debt.Cmp(e.Debt) > 0 {
		e.Debt = debt
	}
}

func (s *PoolSimulator) CalcAmountOut(params pool.CalcAmountOutParams) (*pool.CalcAmountOutResult, error) {
	indexIn, indexOut := s.GetTokenIndex(params.TokenAmountIn.Token), s.GetTokenIndex(params.TokenOut)
	if indexIn < 0 || indexOut < 0 || indexIn == indexOut {
		return nil, ErrInvalidToken
	}
	amountIn := params.TokenAmountIn.Amount
	if amountIn == nil || amountIn.Sign() <= 0 {
		return nil, ErrInvalidAmountIn
	}
	if s.Extra.Collateral == nil {
		return nil, ErrNotPriceable
	}
	if s.Extra.SwapperRotated {
		return nil, ErrSwapperNotRegistered
	}

	// Base moved within the route: price a folded COPY (stays pure; nil basePool on
	// the copy ends the recursion).
	if dS, dV, fS, fV, moved := s.baseDeltas(); moved {
		folded := *s
		folded.basePool = nil
		foldBaseDeltas(&folded.Extra, dS, dV, fS, fV)
		return folded.CalcAmountOut(params)
	}

	cp := s.curveParams()
	state := &s.Extra
	if cp.stateRegion(state.Collateral, state.Debt, state.PriceWad) == regionOut {
		// degenerate/unmarked states — refuse to quote rather than risk a wrong price.
		// (Recovery states past the CR wall quote deleverage-only; leverage quotes
		// reject there by construction.)
		return nil, ErrNotPriceable
	}

	if indexIn == 1 { // volatile -> stable: LEVERAGE
		return s.calcLeverage(params, amountIn)
	}
	return s.calcDeleverage(params, amountIn) // stable -> volatile: DELEVERAGE
}

func (s *PoolSimulator) calcLeverage(params pool.CalcAmountOutParams,
	amountIn *big.Int) (*pool.CalcAmountOutResult, error) {
	cp := s.curveParams()
	state := &s.Extra

	// Debt origination is halted while the CDP charges borrow interest: the rebalancer
	// reverts every leverage fill, though deleverage stays live.
	if state.InterestRate != nil && state.InterestRate.Sign() != 0 || state.MintDenied {
		return nil, ErrLeverageDisabled
	}

	// Cap by the physical-CR floor. The bound is modelled on the POST-fill book exactly
	// as the venue re-checks it, and reproduces the venue's own boundary to the share —
	// so no safety shave is applied. What it cannot cover is oracle movement between
	// this snapshot and execution, which is ordinary quote staleness.
	maxShares := cp.maxLeverageShares(state)
	if maxShares.Sign() == 0 {
		return nil, ErrSwapRejected
	}

	shares := state.sharesForVolatileIn(amountIn, maxShares)
	if shares.Sign() == 0 {
		return nil, ErrSwapRejected
	}
	// The executor passes the swapper's previewed stable leg (rounded up) as the flash
	// cap and the whole amountIn as the volatile cap; the ALM sizes the mint off those
	// two and the swapper sells the surplus back, which is what the physical legs and
	// therefore the net stable out settle by.
	stableLeg, _, ok := state.previewTokenAmounts(shares, true)
	if !ok {
		return nil, ErrSwapRejected
	}
	almRequired := state.cvConvertToAssets(shares, true)
	physStable, physVolatile, dIdleS, dIdleV, ok := state.leverageLegsActual(almRequired, stableLeg, amountIn)
	if !ok {
		return nil, ErrSwapRejected
	}
	grossStableOut, newColl, newDebt := cp.leverageQuoteChecked(state, shares)
	if grossStableOut.Sign() == 0 || grossStableOut.Cmp(physStable) <= 0 {
		return nil, ErrZeroAmountOut
	}
	netStableOut := new(big.Int).Sub(grossStableOut, physStable)

	var remaining big.Int
	remaining.Sub(amountIn, physVolatile)
	if remaining.Sign() < 0 {
		return nil, ErrSwapRejected
	}

	postAssets, postSupply := state.postVaultLeverage(shares)
	return &pool.CalcAmountOutResult{
		TokenAmountOut:         &pool.TokenAmount{Token: params.TokenOut, Amount: netStableOut},
		Fee:                    &pool.TokenAmount{Token: params.TokenOut, Amount: bignumber.ZeroBI},
		RemainingTokenAmountIn: &pool.TokenAmount{Token: params.TokenAmountIn.Token, Amount: &remaining},
		Gas:                    s.gasFor(true),
		SwapInfo: SwapInfo{
			IsLeverage:        true,
			CollVaultShares:   shares,
			StableLeg:         physStable,
			VolatileLeg:       physVolatile,
			AlmShares:         almRequired,
			FlashStableCap:    stableLeg,
			IdleStableDelta:   dIdleS,
			IdleVolatileDelta: dIdleV,
			NewCollateral:     newColl,
			NewDebt:           newDebt,
			PostCvTotalAssets: postAssets,
			PostCvTotalSupply: postSupply,
			PostPriceWad:      state.postReservation(newColl, postAssets, postSupply),
		},
	}, nil
}

func (s *PoolSimulator) calcDeleverage(params pool.CalcAmountOutParams,
	amountIn *big.Int) (*pool.CalcAmountOutResult, error) {
	cp := s.curveParams()
	state := &s.Extra

	maxGross := cp.maxDeleverageIn(state)
	if maxGross.Sign() == 0 {
		return nil, ErrSwapRejected
	}

	// Sized to the swapper's PREVIEW net, one wei under the budget: the ALM floors the
	// accounted and idle parts of a withdraw separately, so the stable it physically
	// releases can land a wei under the preview the flash repayment was sized off, and an
	// exactly-funded call reverts Slippage. The wei of headroom absorbs it — the
	// adapter keeps the same wei (see EverlongRebalancerAdapter).
	var budget big.Int
	budget.Sub(amountIn, big.NewInt(1))
	gross := cp.grossForNetStableIn(state, &budget, maxGross)
	if gross.Sign() == 0 {
		return nil, ErrSwapRejected
	}
	sharesOut, newColl, newDebt := cp.deleverageQuoteChecked(state, gross)
	if sharesOut.Sign() == 0 {
		return nil, ErrSwapRejected
	}
	stablePreview, _, ok := state.previewTokenAmounts(sharesOut, false)
	if !ok {
		return nil, ErrSwapRejected
	}
	// net = gross - released stable must stay positive and monotone in gross; past the
	// point where the released stable outruns the debt retired the swapper would refund
	// more than it pulled and the sizing's monotonicity no longer holds.
	if stablePreview.Cmp(gross) >= 0 {
		return nil, ErrSwapRejected
	}
	almShares, ok := state.redeemAlmShares(state.netRedeemShares(sharesOut))
	if !ok {
		return nil, ErrSwapRejected // the vault's preview and transfer disagree: TokenAmountMismatch
	}
	stableOut, volatileOut := state.redeemLegs(almShares)
	if volatileOut.Sign() <= 0 {
		return nil, ErrZeroAmountOut
	}
	dIdleS, dIdleV := state.redeemIdleDeltas(almShares)

	// What the route must deliver: the preview net plus the wei of headroom. The swapper
	// pulls it and refunds whatever the physical legs leave.
	var net, remaining big.Int
	net.Sub(gross, stablePreview)
	net.Add(&net, big.NewInt(1))
	remaining.Sub(amountIn, &net)
	if remaining.Sign() < 0 {
		return nil, ErrSwapRejected
	}

	postAssets, postSupply := state.postVaultDeleverage(sharesOut)
	return &pool.CalcAmountOutResult{
		TokenAmountOut:         &pool.TokenAmount{Token: params.TokenOut, Amount: volatileOut},
		Fee:                    &pool.TokenAmount{Token: params.TokenOut, Amount: bignumber.ZeroBI},
		RemainingTokenAmountIn: &pool.TokenAmount{Token: params.TokenAmountIn.Token, Amount: &remaining},
		Gas:                    s.gasFor(false),
		SwapInfo: SwapInfo{
			IsLeverage:        false,
			CollVaultShares:   sharesOut,
			GrossStableIn:     gross,
			StableLeg:         stableOut,
			VolatileLeg:       volatileOut,
			AlmShares:         almShares,
			IdleStableDelta:   dIdleS,
			IdleVolatileDelta: dIdleV,
			NewCollateral:     newColl,
			NewDebt:           newDebt,
			PostCvTotalAssets: postAssets,
			PostCvTotalSupply: postSupply,
			PostPriceWad:      state.postReservation(newColl, postAssets, postSupply),
			AlmBurned:         new(big.Int).Sub(state.CvTotalAssets, postAssets),
		},
	}, nil
}

// netRedeemShares is the CollVault share count a redeem burns net of the withdraw fee.
func (s *VaultState) netRedeemShares(collVaultShares *big.Int) *big.Int {
	shareFee := mulDivUp(collVaultShares, s.WithdrawFeeBp, bigBp)
	return new(big.Int).Sub(collVaultShares, shareFee)
}

// UpdateBalance replays the exact fill from SwapInfo — never recomputes swap results.
// curveParams prefers the per-refresh on-chain curve over the frozen constants.
func (s *PoolSimulator) curveParams() *CurveParams {
	if s.Extra.LiveCurve != nil {
		return s.Extra.LiveCurve
	}
	return &s.StaticExtra.CurveParams
}

func (s *PoolSimulator) UpdateBalance(params pool.UpdateBalanceParams) {
	si, ok := params.SwapInfo.(SwapInfo)
	if !ok {
		return
	}
	// Absorb base movement first (the fill was quoted on the folded state), then
	// advance the baseline so it isn't recounted.
	if dS, dV, fS, fV, moved := s.baseDeltas(); moved {
		foldBaseDeltas(&s.Extra, dS, dV, fS, fV)
		s.baseStable0 = new(big.Int).Add(s.baseStable0, dS)
		s.baseVolatile0 = new(big.Int).Add(s.baseVolatile0, dV)
		if s.baseAccStable0 != nil {
			s.baseAccStable0 = new(big.Int).Add(s.baseAccStable0, new(big.Int).Sub(dS, fS))
			s.baseAccVolatile0 = new(big.Int).Add(s.baseAccVolatile0, new(big.Int).Sub(dV, fV))
		}
	}
	e := &s.Extra
	// The CDP's system totals move by the position's own change.
	if e.SysCollateral != nil && e.SysDebt != nil {
		e.SysCollateral = new(big.Int).Add(e.SysCollateral, new(big.Int).Sub(si.NewCollateral, e.Collateral))
		e.SysDebt = new(big.Int).Add(e.SysDebt, new(big.Int).Sub(si.NewDebt, e.Debt))
	}
	e.Collateral = si.NewCollateral
	e.Debt = si.NewDebt
	// The raw (unprojected) debt the interest projection starts from moves with the fill
	// too, else a later projection would re-inflate the debt from the stale raw word.
	if e.PosRawDebt != nil && e.PosInterestIndex != nil && e.ActiveInterestIndex != nil && e.ActiveInterestIndex.Sign() > 0 {
		e.PosRawDebt = mulDiv(si.NewDebt, e.PosInterestIndex, e.ActiveInterestIndex)
	}
	// The ALM takes and releases idle pro-rata alongside the accounted reserves; the
	// quote carries the exact deltas, the pro-rata floor is the legacy fallback.
	shiftIdle := func(almShares *big.Int, sign int) {
		for i, idle := range []**big.Int{&e.AlmIdleStable, &e.AlmIdleVolatile} {
			if *idle == nil || e.AlmSupply.Sign() == 0 {
				continue
			}
			exact := si.IdleStableDelta
			if i == 1 {
				exact = si.IdleVolatileDelta
			}
			if exact != nil {
				*idle = new(big.Int).Add(*idle, exact)
				continue
			}
			d := mulDiv(*idle, almShares, e.AlmSupply)
			if sign < 0 {
				d.Neg(d)
			}
			*idle = new(big.Int).Add(*idle, d)
		}
	}
	if si.IsLeverage {
		// Reverse coupling: this fill's ALM mint also moves the base CVAMM pool. The
		// exact mint is the vault's ALM-share growth (post words); preview fallback.
		minted := si.AlmShares
		if si.PostCvTotalAssets != nil && e.CvTotalAssets != nil {
			minted = new(big.Int).Sub(si.PostCvTotalAssets, e.CvTotalAssets)
		}
		s.pushFillToBase(minted, e.AlmSupply)
		shiftIdle(minted, 1)
		e.AlmStableReserve = new(big.Int).Add(e.AlmStableReserve, si.StableLeg)
		e.AlmVolatileReserve = new(big.Int).Add(e.AlmVolatileReserve, si.VolatileLeg)
		e.AlmSupply = new(big.Int).Add(e.AlmSupply, si.AlmShares)
		// the reference-marked reserves ARE the physical totals (the adapter reads both
		// from getTotalAmounts; only the marking price differs), so the legs shift them
		// exactly
		e.RefStableReserve = new(big.Int).Add(e.RefStableReserve, si.StableLeg)
		e.RefAssetReserve = new(big.Int).Add(e.RefAssetReserve, si.VolatileLeg)
	} else {
		// the ALM burns the raw-ratio share amount the CollVault released, not the
		// preview conversion
		almBurned := si.AlmBurned
		if almBurned == nil {
			almBurned = si.AlmShares
		}
		// Reverse coupling: this fill's ALM burn also moves the base CVAMM pool. The
		// exact burn is the vault's ALM-share drop (post words); raw-ratio fallback.
		burned := almBurned
		if si.PostCvTotalAssets != nil && e.CvTotalAssets != nil {
			burned = new(big.Int).Sub(e.CvTotalAssets, si.PostCvTotalAssets)
		}
		s.pushFillToBase(new(big.Int).Neg(burned), e.AlmSupply)
		shiftIdle(burned, -1)
		e.AlmStableReserve = new(big.Int).Sub(e.AlmStableReserve, si.StableLeg)
		e.AlmVolatileReserve = new(big.Int).Sub(e.AlmVolatileReserve, si.VolatileLeg)
		e.AlmSupply = new(big.Int).Sub(e.AlmSupply, almBurned)
		e.RefStableReserve = new(big.Int).Sub(e.RefStableReserve, si.StableLeg)
		e.RefAssetReserve = new(big.Int).Sub(e.RefAssetReserve, si.VolatileLeg)
	}
	// Exact post-fill vault words computed at quote time; legacy SwapInfo (no post words)
	// falls back to the preview-delta model.
	if si.PostCvTotalAssets != nil && si.PostCvTotalSupply != nil {
		e.CvTotalAssets = si.PostCvTotalAssets
		e.CvTotalSupply = si.PostCvTotalSupply
	} else if si.IsLeverage {
		e.CvTotalAssets = new(big.Int).Add(e.CvTotalAssets, si.AlmShares)
		e.CvTotalSupply = new(big.Int).Add(e.CvTotalSupply, si.CollVaultShares)
	} else {
		e.CvTotalAssets = new(big.Int).Sub(e.CvTotalAssets, si.AlmShares)
		e.CvTotalSupply = new(big.Int).Sub(e.CvTotalSupply, si.CollVaultShares)
	}
	// The fill's own CollVault mint/burn moves the reservation value the next quote
	// prices from (rvps stays put under the pro-rata ALM leg).
	if si.PostPriceWad != nil {
		e.PriceWad = si.PostPriceWad
	}
	s.Info.Reserves = []*big.Int{
		new(big.Int).Set(e.AlmStableReserve),
		new(big.Int).Set(e.AlmVolatileReserve),
	}
}

func (s *PoolSimulator) CloneState() pool.IPoolSimulator {
	cloned := *s
	cloned.Info.Reserves = lo.Map(s.Info.Reserves, func(r *big.Int, _ int) *big.Int {
		return new(big.Int).Set(r)
	})
	// Extra is copy-on-write, so the value copy above insulates it. The base is not:
	// UpdateBalance pushes this fill into it, so a shared pointer would let a clone's
	// fill mutate the caller's backup. nil means the base cannot clone itself.
	if s.basePool != nil {
		if clonedBase := s.basePool.CloneState(); clonedBase != nil {
			cloned.basePool = clonedBase
		}
	}
	return &cloned
}

func (s *PoolSimulator) GetMetaInfo(_, _ string) any {
	return PoolMeta{
		Swapper:              s.StaticExtra.Swapper,
		Rebalancer:           s.StaticExtra.Rebalancer,
		Math:                 s.StaticExtra.Math,
		LeverageRatioWad:     s.curveParams().LeverageRatioWad,
		UpfrontNetStablePull: true,
		ApprovalAddress:      s.GetApprovalAddress("", ""),
		BlockNumber:          s.Info.BlockNumber,
	}
}

// GetApprovalAddress: the swapper transferFroms both legs from the payer.
func (s *PoolSimulator) GetApprovalAddress(_, _ string) string {
	return s.StaticExtra.Swapper
}

// gasFor returns the configured per-deployment gas estimate for a direction, or the
// measured default.
func (s *PoolSimulator) gasFor(leverage bool) int64 {
	if leverage {
		if s.StaticExtra.GasLeverage != 0 {
			return s.StaticExtra.GasLeverage
		}
		return defaultGasLeverage
	}
	if s.StaticExtra.GasDeleverage != 0 {
		return s.StaticExtra.GasDeleverage
	}
	return defaultGasDeleverage
}
