package everlongcollvault

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

// PoolSimulator prices the Everlong CollVault settlement venue
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

	// Meta/base coupling with the everlong-cvamm pool the vault's ALM adapter wraps:
	// a CVAMM fill in the same route moves the ALM reserves this vault's quote prices
	// from. baseStable0/baseVolatile0 are the base reserves at THIS snapshot's block —
	// the fold applies only the movement since then, so tracker refreshes never double
	// count. nil basePool = uncoupled (quotes from the snapshot alone, as before).
	basePool      pool.IPoolSimulator
	baseStable0   *big.Int
	baseVolatile0 *big.Int
}

var _ = pool.RegisterFactoryMeta(DexType, NewPoolSimulatorWithBases)

// NewPoolSimulatorWithBases wires the underlying everlong-cvamm sim (resolved at listing
// into StaticExtra.UnderlyingCvamm) as this pool's base. Missing from the map = uncoupled.
func NewPoolSimulatorWithBases(p entity.Pool, basePoolMap map[string]pool.IPoolSimulator) (*PoolSimulator, error) {
	sim, err := NewPoolSimulator(p)
	if err != nil || sim.StaticExtra.UnderlyingCvamm == "" {
		return sim, err
	}
	base, ok := basePoolMap[strings.ToLower(sim.StaticExtra.UnderlyingCvamm)]
	if !ok {
		base, ok = basePoolMap[sim.StaticExtra.UnderlyingCvamm]
	}
	if ok {
		sim.wireBase(base)
	}
	return sim, nil
}

// wireBase adopts the base sim and pins the delta baseline at its CURRENT (snapshot)
// reserves. everlong-cvamm reserves are [stable, volatile] by construction.
func (s *PoolSimulator) wireBase(base pool.IPoolSimulator) {
	res := base.GetReserves()
	if len(res) != 2 || res[0] == nil || res[1] == nil {
		return
	}
	s.basePool = base
	s.baseStable0 = new(big.Int).Set(res[0])
	s.baseVolatile0 = new(big.Int).Set(res[1])
}

func (s *PoolSimulator) GetBasePools() []pool.IPoolSimulator {
	if s.basePool == nil {
		return nil
	}
	return []pool.IPoolSimulator{s.basePool}
}

// SetBasePool swaps the base POINTER only (the router re-wires clones mid-route); the
// baseline stays at snapshot time so deltas accumulated within the route keep applying.
// First wiring (constructed without a map) also pins the baseline.
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

// baseDeltas is the base pool's reserve movement since this snapshot's baseline.
func (s *PoolSimulator) baseDeltas() (dStable, dVolatile *big.Int, moved bool) {
	if s.basePool == nil || s.baseStable0 == nil {
		return nil, nil, false
	}
	res := s.basePool.GetReserves()
	if len(res) != 2 || res[0] == nil || res[1] == nil {
		return nil, nil, false
	}
	dStable = new(big.Int).Sub(res[0], s.baseStable0)
	dVolatile = new(big.Int).Sub(res[1], s.baseVolatile0)
	return dStable, dVolatile, dStable.Sign() != 0 || dVolatile.Sign() != 0
}

// foldBaseDeltas shifts the ALM legs of e by the base movement. The reference-marked
// reserves are the same physical totals under a different marking price (see
// UpdateBalance), so the deltas apply to both exactly.
func foldBaseDeltas(e *Extra, dStable, dVolatile *big.Int) {
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

	// Coupled quote: when the base CVAMM moved within this route, price a folded COPY
	// (CalcAmountOut stays pure; the copy's nil basePool makes the recursion a no-op).
	if dS, dV, moved := s.baseDeltas(); moved {
		folded := *s
		folded.basePool = nil
		foldBaseDeltas(&folded.Extra, dS, dV)
		return folded.CalcAmountOut(params)
	}

	cp := &s.StaticExtra.CurveParams
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
	cp := &s.StaticExtra.CurveParams
	state := &s.Extra

	// Debt origination is halted while the CDP charges borrow interest: the rebalancer
	// reverts every leverage fill, though deleverage stays live.
	if state.InterestRate != nil && state.InterestRate.Sign() != 0 {
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
	stableLeg, volatileLeg, ok := state.previewTokenAmounts(shares, true)
	if !ok {
		return nil, ErrSwapRejected
	}
	netStableOut, ok := cp.quoteLeverageAt(state, shares)
	if !ok {
		return nil, ErrSwapRejected
	}
	if netStableOut.Sign() <= 0 {
		return nil, ErrZeroAmountOut
	}
	_, newColl, newDebt := cp.leverageQuoteChecked(state, shares)

	var remaining big.Int
	remaining.Sub(amountIn, volatileLeg)
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
			StableLeg:         stableLeg,
			VolatileLeg:       volatileLeg,
			AlmShares:         state.cvConvertToAssets(shares, true),
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
	cp := &s.StaticExtra.CurveParams
	state := &s.Extra

	maxGross := cp.maxDeleverageIn(state)
	if maxGross.Sign() == 0 {
		return nil, ErrSwapRejected
	}

	gross := cp.grossForNetStableIn(state, amountIn, maxGross)
	if gross.Sign() == 0 {
		return nil, ErrSwapRejected
	}
	sharesOut, newColl, newDebt := cp.deleverageQuoteChecked(state, gross)
	if sharesOut.Sign() == 0 {
		return nil, ErrSwapRejected
	}
	stableOut, volatileOut, ok := state.previewTokenAmounts(sharesOut, false)
	if !ok {
		return nil, ErrSwapRejected
	}
	if volatileOut.Sign() <= 0 {
		return nil, ErrZeroAmountOut
	}

	// The forward net is what the swapper actually pulls: gross - freed stable.
	var net, remaining big.Int
	net.Sub(gross, stableOut)
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
			AlmShares:         s.deleverageAlmShares(sharesOut),
			NewCollateral:     newColl,
			NewDebt:           newDebt,
			PostCvTotalAssets: postAssets,
			PostCvTotalSupply: postSupply,
			PostPriceWad:      state.postReservation(newColl, postAssets, postSupply),
			AlmBurned:         new(big.Int).Sub(state.CvTotalAssets, postAssets),
		},
	}, nil
}

// deleverageAlmShares mirrors previewTokenAmounts' redeem path share conversion.
func (s *PoolSimulator) deleverageAlmShares(collVaultShares *big.Int) *big.Int {
	shareFee := mulDivUp(collVaultShares, s.Extra.WithdrawFeeBp, bigBp)
	var net big.Int
	net.Sub(collVaultShares, shareFee)
	return s.Extra.cvConvertToAssets(&net, false)
}

// UpdateBalance replays the exact fill from SwapInfo — never recomputes swap results.
func (s *PoolSimulator) UpdateBalance(params pool.UpdateBalanceParams) {
	si, ok := params.SwapInfo.(SwapInfo)
	if !ok {
		return
	}
	// Absorb any base CVAMM movement into the snapshot first (the fill being replayed
	// was quoted on the folded state), then advance the baseline so it isn't recounted.
	if dS, dV, moved := s.baseDeltas(); moved {
		foldBaseDeltas(&s.Extra, dS, dV)
		s.baseStable0 = new(big.Int).Add(s.baseStable0, dS)
		s.baseVolatile0 = new(big.Int).Add(s.baseVolatile0, dV)
	}
	e := &s.Extra
	e.Collateral = si.NewCollateral
	e.Debt = si.NewDebt
	if si.IsLeverage {
		e.AlmStableReserve = new(big.Int).Add(e.AlmStableReserve, si.StableLeg)
		e.AlmVolatileReserve = new(big.Int).Add(e.AlmVolatileReserve, si.VolatileLeg)
		e.AlmSupply = new(big.Int).Add(e.AlmSupply, si.AlmShares)
		// the reference-marked reserves ARE the physical totals (the adapter reads both
		// from getTotalAmounts; only the marking price differs), so the legs shift them
		// exactly
		e.RefStableReserve = new(big.Int).Add(e.RefStableReserve, si.StableLeg)
		e.RefAssetReserve = new(big.Int).Add(e.RefAssetReserve, si.VolatileLeg)
	} else {
		e.AlmStableReserve = new(big.Int).Sub(e.AlmStableReserve, si.StableLeg)
		e.AlmVolatileReserve = new(big.Int).Sub(e.AlmVolatileReserve, si.VolatileLeg)
		// the ALM burns the raw-ratio share amount the CollVault released, not the
		// preview conversion
		almBurned := si.AlmBurned
		if almBurned == nil {
			almBurned = si.AlmShares
		}
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
	// UpdateBalance reassigns Extra's pointers wholesale (copy-on-write), so the value
	// copy of Extra above already insulates the clone.
	return &cloned
}

func (s *PoolSimulator) GetMetaInfo(_, _ string) any {
	return PoolMeta{
		Swapper:              s.StaticExtra.Swapper,
		Rebalancer:           s.StaticExtra.Rebalancer,
		UpfrontNetStablePull: true,
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
