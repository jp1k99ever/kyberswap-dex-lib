package everlongrebalancer

import (
	"fmt"
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
	// baseInventoryX0 pins the CVAMM coordinate at the same-block tracker snapshot.
	// A direct base swap moves x and can close ClammAlmAdapter's reference-oracle
	// neighborhood check. That check cannot be re-attested from route-local state, so
	// leverage is disabled once x differs; liquidity moves themselves leave x unchanged.
	baseInventoryX0 *big.Int
	// couplingExact is latched false when an UpdateBalance cannot replay the venue's
	// ALM deposit/withdraw shape exactly. UpdateBalance cannot return an error, so every
	// later quote checks this bit and refuses to continue from an approximate book.
	couplingExact bool
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
	if !strings.EqualFold(sim.StaticExtra.ALMAdapterCodeHash, supportedAlmAdapterCodeHash) {
		return nil, ErrUnsupportedAdapter
	}
	base, ok := basePoolMap[strings.ToLower(sim.StaticExtra.UnderlyingCvamm)]
	if !ok {
		base, ok = basePoolMap[sim.StaticExtra.UnderlyingCvamm]
	}
	if !ok {
		return nil, ErrMissingBasePool
	}
	exactBase, ok := base.(exactLiquidityBase)
	if !ok || !strings.EqualFold(base.GetAddress(), sim.StaticExtra.UnderlyingCvamm) ||
		!exactBase.IsLiquidityStateExact() ||
		exactBase.SnapshotBlockNumber() != sim.Info.BlockNumber {
		return nil, ErrInexactBasePool
	}
	if !sim.hasExactCouplingSnapshot() {
		return nil, ErrInexactCoupledState
	}
	totals, accounted, ok := exactBaseBook(base)
	if !ok || totals[0].Cmp(sim.Extra.AlmStableReserve) != 0 ||
		totals[1].Cmp(sim.Extra.AlmVolatileReserve) != 0 ||
		new(big.Int).Sub(totals[0], accounted[0]).Cmp(sim.Extra.AlmIdleStable) != 0 ||
		new(big.Int).Sub(totals[1], accounted[1]).Cmp(sim.Extra.AlmIdleVolatile) != 0 {
		return nil, ErrInexactCoupledState
	}
	rvps, ok := exactBase.ReservationValuePerShareWad(sim.Extra.AlmResvPriceWad, sim.Extra.AlmSupply)
	if !ok || rvps == nil || rvps.Cmp(sim.Extra.RvpsWad) != 0 {
		return nil, ErrInexactCoupledState
	}
	sim.wireBase(base)
	if sim.basePool == nil {
		return nil, ErrInexactBasePool
	}
	// Bare construction starts disabled. Only this factory has attested the same-block
	// totals, idle split, RVPS and required base capabilities, so only it may make the
	// simulator routable.
	sim.couplingExact = true
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

func exactBaseBook(base pool.IPoolSimulator) (totals, accounted []*big.Int, ok bool) {
	exact, ok := base.(exactLiquidityBase)
	if !ok || !exact.IsLiquidityStateExact() {
		return nil, nil, false
	}
	totals, accounted = exact.GetTotalReserves(), base.GetReserves()
	if len(totals) != 2 || len(accounted) != 2 ||
		totals[0] == nil || totals[1] == nil || accounted[0] == nil || accounted[1] == nil {
		return nil, nil, false
	}
	return totals, accounted, true
}

// wireBase adopts the base sim and pins the baseline at its current (snapshot)
// total reserves — everlong-cvamm order is [stable, volatile].
func (s *PoolSimulator) wireBase(base pool.IPoolSimulator) {
	res, acc, ok := exactBaseBook(base)
	exact, exactOK := base.(exactLiquidityBase)
	if !ok || !exactOK {
		return
	}
	x := exact.CurrentInventoryXWad()
	if x == nil || x.Sign() <= 0 {
		return
	}
	s.basePool = base
	s.baseStable0 = new(big.Int).Set(res[0])
	s.baseVolatile0 = new(big.Int).Set(res[1])
	s.baseAccStable0 = new(big.Int).Set(acc[0])
	s.baseAccVolatile0 = new(big.Int).Set(acc[1])
	s.baseInventoryX0 = new(big.Int).Set(x)
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
	exactBase, ok := base.(exactLiquidityBase)
	if !ok || !exactBase.IsLiquidityStateExact() {
		s.couplingExact = false
		return
	}
	if exactBase.SnapshotBlockNumber() != s.Info.BlockNumber {
		s.couplingExact = false
		return
	}
	if _, _, ok := exactBaseBook(base); !ok {
		s.couplingExact = false
		return
	}
	if s.basePool == nil {
		s.wireBase(base)
		if s.basePool == nil {
			s.couplingExact = false
		}
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
	// ApplyLiquidityDelta replays one ALM deposit (positive shares, with the amounts the
	// venue pulled) or withdrawal (negative shares) and reports the movement in both
	// domains: totals first, then the accounted reserves alone.
	ApplyLiquidityDelta(sharesDelta, supplyBefore, used0, used1 *big.Int) (
		dTotalStable, dTotalVolatile, dAccStable, dAccVolatile *big.Int)
}

// exactLiquidityBase is deliberately a loose interface to avoid a package cycle. The
// underlying simulator must expose both its exactness latch and an invalidation hook:
// a malformed legacy SwapInfo must disable the direct base quote too, otherwise a route
// could continue through a stale CVAMM book after this meta pool rejected the replay.
type exactLiquidityBase interface {
	liquidityDeltaApplier
	totalReserver
	IsLiquidityStateExact() bool
	InvalidateLiquidityState()
	ReservationValuePerShareWad(reservationPriceWad, totalSupply *big.Int) (*big.Int, bool)
	CurrentInventoryXWad() *big.Int
	SnapshotBlockNumber() uint64
}

func (s *PoolSimulator) hasExactCouplingSnapshot() bool {
	e := &s.Extra
	return e.AlmIdleStable != nil && e.AlmIdleVolatile != nil &&
		e.AlmIdleStable.Sign() >= 0 && e.AlmIdleVolatile.Sign() >= 0 &&
		e.AlmStableReserve != nil && e.AlmVolatileReserve != nil &&
		e.AlmIdleStable.Cmp(e.AlmStableReserve) <= 0 &&
		e.AlmIdleVolatile.Cmp(e.AlmVolatileReserve) <= 0 &&
		e.AlmSupply != nil && e.AlmSupply.Sign() > 0 &&
		e.RvpsWad != nil && e.RvpsWad.Sign() > 0 &&
		e.AlmResvPriceWad != nil && e.AlmResvPriceWad.Sign() > 0
}

func (s *PoolSimulator) invalidateCoupling() {
	s.couplingExact = false
	if base, ok := s.basePool.(exactLiquidityBase); ok {
		base.InvalidateLiquidityState()
	}
}

func (s *PoolSimulator) latchCoupling() {
	s.couplingExact = false
}

func (s *PoolSimulator) coupledStateExact() bool {
	if !s.couplingExact {
		return false
	}
	if s.basePool == nil {
		return true
	}
	base, ok := s.basePool.(exactLiquidityBase)
	return ok && base.IsLiquidityStateExact()
}

func (s *PoolSimulator) baseRvps(totalSupply *big.Int) (*big.Int, bool) {
	base, ok := s.basePool.(exactLiquidityBase)
	if !ok || s.Extra.AlmResvPriceWad == nil {
		return nil, false
	}
	return base.ReservationValuePerShareWad(s.Extra.AlmResvPriceWad, totalSupply)
}

// baseReferenceGateAttested is intentionally stricter than reserve folding. The
// adapter's getReservesAtReference() checks the live CVAMM spot against an external
// oracle. We can fold a route-local swap's reserves exactly, but we cannot reconstruct
// that external check at execution time. Equality with the snapshot x is the only safe
// local attestation; a missing marker also fails leverage closed.
func (s *PoolSimulator) baseReferenceGateAttested() bool {
	if s.basePool == nil {
		return true
	}
	base, ok := s.basePool.(exactLiquidityBase)
	if !ok || s.baseInventoryX0 == nil {
		return false
	}
	x := base.CurrentInventoryXWad()
	return x != nil && x.Cmp(s.baseInventoryX0) == 0
}

// previewLiquidityState applies a candidate transition to a CLONE of the base. This is
// used inside the quote's boundary searches: the deployed wrapper floors center
// reserves, idle buckets and total supply separately, so the post-fill rvps cannot be
// obtained by scaling the pre-fill word.
func (s *PoolSimulator) previewLiquidityState(si SwapInfo, supplyBefore *big.Int) (*postLiquidityState, bool) {
	if s.basePool == nil || supplyBefore == nil || supplyBefore.Sign() <= 0 {
		return nil, false
	}
	cloned := s.basePool.CloneState()
	base, ok := cloned.(exactLiquidityBase)
	if !ok || !base.IsLiquidityStateExact() {
		return nil, false
	}
	apply := func(shares, supply, used0, used1 *big.Int) bool {
		beforeTot, beforeAcc, exact := exactBaseBook(cloned)
		beforeX := base.CurrentInventoryXWad()
		if !exact || beforeX == nil {
			return false
		}
		beforeTot = []*big.Int{new(big.Int).Set(beforeTot[0]), new(big.Int).Set(beforeTot[1])}
		beforeAcc = []*big.Int{new(big.Int).Set(beforeAcc[0]), new(big.Int).Set(beforeAcc[1])}
		d0, d1, a0, a1 := base.ApplyLiquidityDelta(shares, supply, used0, used1)
		afterTot, afterAcc, exact := exactBaseBook(cloned)
		afterX := base.CurrentInventoryXWad()
		return d0 != nil && d1 != nil && a0 != nil && a1 != nil && exact &&
			afterX != nil && afterX.Cmp(beforeX) == 0 &&
			new(big.Int).Sub(afterTot[0], beforeTot[0]).Cmp(d0) == 0 &&
			new(big.Int).Sub(afterTot[1], beforeTot[1]).Cmp(d1) == 0 &&
			new(big.Int).Sub(afterAcc[0], beforeAcc[0]).Cmp(a0) == 0 &&
			new(big.Int).Sub(afterAcc[1], beforeAcc[1]).Cmp(a1) == 0
	}
	postSupply := new(big.Int).Set(supplyBefore)
	if si.IsLeverage {
		if si.AlmMintedShares == nil || si.AlmUsedStable == nil || si.AlmUsedVolatile == nil ||
			si.AlmSoldBackShare == nil ||
			!apply(si.AlmMintedShares, supplyBefore, si.AlmUsedStable, si.AlmUsedVolatile) {
			return nil, false
		}
		postSupply.Add(postSupply, si.AlmMintedShares)
		if si.AlmSoldBackShare.Sign() > 0 {
			if !apply(new(big.Int).Neg(si.AlmSoldBackShare), postSupply, nil, nil) {
				return nil, false
			}
			postSupply.Sub(postSupply, si.AlmSoldBackShare)
		}
	} else {
		if si.AlmBurned == nil || si.AlmBurned.Sign() <= 0 ||
			!apply(new(big.Int).Neg(si.AlmBurned), supplyBefore, nil, nil) {
			return nil, false
		}
		postSupply.Sub(postSupply, si.AlmBurned)
	}
	if postSupply.Sign() <= 0 {
		return nil, false
	}
	rvps, ok := base.ReservationValuePerShareWad(s.Extra.AlmResvPriceWad, postSupply)
	if !ok {
		return nil, false
	}
	totals := base.GetTotalReserves()
	if len(totals) != 2 || totals[0] == nil || totals[1] == nil {
		return nil, false
	}
	return &postLiquidityState{
		RvpsWad:       rvps,
		StableTotal:   new(big.Int).Set(totals[0]),
		VolatileTotal: new(big.Int).Set(totals[1]),
		TotalSupply:   postSupply,
	}, true
}

func (s *PoolSimulator) quoteState(amountIn *big.Int) *VaultState {
	if s.basePool == nil {
		return &s.Extra
	}
	state := s.Extra
	type cacheEntry struct {
		state *postLiquidityState
		ok    bool
	}
	cache := make(map[string]cacheEntry)
	state.postLiquidity = func(isLeverage bool, shares *big.Int) (*postLiquidityState, bool) {
		key := shares.String()
		if isLeverage {
			key = "l:" + key
		} else {
			key = "d:" + key
		}
		if cached, exists := cache[key]; exists {
			return cached.state, cached.ok
		}
		var result *postLiquidityState
		var exact bool
		if isLeverage {
			stableCap, _, ok := state.previewTokenAmounts(shares, true)
			if !ok {
				cache[key] = cacheEntry{}
				return nil, false
			}
			almRequired := state.cvConvertToAssets(shares, true)
			_, _, _, _, mint, ok := state.leverageLegsActual(almRequired, stableCap, amountIn)
			if !ok {
				cache[key] = cacheEntry{}
				return nil, false
			}
			result, exact = s.previewLiquidityState(SwapInfo{
				IsLeverage:       true,
				AlmMintedShares:  mint.Minted,
				AlmUsedStable:    mint.Used0,
				AlmUsedVolatile:  mint.Used1,
				AlmSoldBackShare: mint.SoldBack,
			}, state.AlmSupply)
		} else {
			almShares, ok := state.redeemAlmShares(state.netRedeemShares(shares))
			if !ok {
				cache[key] = cacheEntry{}
				return nil, false
			}
			result, exact = s.previewLiquidityState(
				SwapInfo{IsLeverage: false, AlmBurned: almShares}, state.AlmSupply)
		}
		cache[key] = cacheEntry{state: result, ok: exact}
		return result, exact
	}
	return &state
}

// pushFillToBase folds THIS fill's ALM mint/burn into the base CVAMM sim (pro-rata
// reserves + kappa), so a later direct CVAMM quote in the route isn't stale. The
// baseline advances by the applied deltas: the movement originated here and is already
// in this sim's own state.
func (s *PoolSimulator) pushFillToBase(sharesDelta, supplyBefore, used0, used1 *big.Int) bool {
	base, ok := s.basePool.(exactLiquidityBase)
	if !ok || s.baseStable0 == nil || sharesDelta == nil || sharesDelta.Sign() == 0 ||
		supplyBefore == nil || supplyBefore.Sign() <= 0 ||
		(sharesDelta.Sign() > 0 && (used0 == nil || used1 == nil || used0.Sign() < 0 || used1.Sign() < 0)) {
		s.invalidateCoupling()
		return false
	}
	beforeTot, beforeAcc, ok := exactBaseBook(s.basePool)
	if !ok {
		s.invalidateCoupling()
		return false
	}
	beforeTot = []*big.Int{new(big.Int).Set(beforeTot[0]), new(big.Int).Set(beforeTot[1])}
	beforeAcc = []*big.Int{new(big.Int).Set(beforeAcc[0]), new(big.Int).Set(beforeAcc[1])}
	beforeX := base.CurrentInventoryXWad()
	if beforeX == nil {
		s.invalidateCoupling()
		return false
	}
	dTotS, dTotV, dAccS, dAccV := base.ApplyLiquidityDelta(sharesDelta, supplyBefore, used0, used1)
	if dTotS == nil || dTotV == nil || dAccS == nil || dAccV == nil || !base.IsLiquidityStateExact() {
		s.invalidateCoupling()
		return false
	}
	afterTot, afterAcc, ok := exactBaseBook(s.basePool)
	afterX := base.CurrentInventoryXWad()
	if !ok || new(big.Int).Sub(afterTot[0], beforeTot[0]).Cmp(dTotS) != 0 ||
		new(big.Int).Sub(afterTot[1], beforeTot[1]).Cmp(dTotV) != 0 ||
		new(big.Int).Sub(afterAcc[0], beforeAcc[0]).Cmp(dAccS) != 0 ||
		new(big.Int).Sub(afterAcc[1], beforeAcc[1]).Cmp(dAccV) != 0 ||
		afterX == nil || afterX.Cmp(beforeX) != 0 {
		s.invalidateCoupling()
		return false
	}
	// The baselines advance in their own domains: totals include the idle slice the
	// deposit parked (or the withdrawal released), accounted does not.
	s.baseStable0 = new(big.Int).Add(s.baseStable0, dTotS)
	s.baseVolatile0 = new(big.Int).Add(s.baseVolatile0, dTotV)
	if s.baseAccStable0 != nil {
		s.baseAccStable0 = new(big.Int).Add(s.baseAccStable0, dAccS)
		s.baseAccVolatile0 = new(big.Int).Add(s.baseAccVolatile0, dAccV)
	}
	return true
}

// pushLeverageToBase replays the venue's two ALM steps in order: the swapper deposits
// both legs, then sells back the shares the CollVault did not need. Replaying only the
// net share change would price the sell-back against the pre-deposit book.
func (s *PoolSimulator) pushLeverageToBase(si SwapInfo, supplyBefore *big.Int) bool {
	if si.AlmMintedShares == nil || si.AlmUsedStable == nil || si.AlmUsedVolatile == nil ||
		si.AlmSoldBackShare == nil {
		s.invalidateCoupling()
		return false
	}
	if !s.pushFillToBase(si.AlmMintedShares, supplyBefore, si.AlmUsedStable, si.AlmUsedVolatile) {
		return false
	}
	if si.AlmSoldBackShare.Sign() > 0 {
		afterMint := new(big.Int).Add(supplyBefore, si.AlmMintedShares)
		if !s.pushFillToBase(new(big.Int).Neg(si.AlmSoldBackShare), afterMint, nil, nil) {
			return false
		}
	}
	return true
}

// foldBaseDeltas shifts the ALM and physical-reference legs by the base movement and
// assigns the wrapper's fully recomputed rvps. Never add a separately-floored fee delta
// to the previously-floored rvps: the deployed adapter floors only after recomputing the
// complete center NAV.
func foldBaseDeltas(e *Extra, dStable, dVolatile, feeStable, feeVolatile, rvps *big.Int) bool {
	if dStable == nil || dVolatile == nil || feeStable == nil || feeVolatile == nil ||
		rvps == nil || rvps.Sign() <= 0 {
		return false
	}
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
	if e.AlmIdleStable != nil {
		e.AlmIdleStable = new(big.Int).Add(e.AlmIdleStable, feeStable)
	}
	if e.AlmIdleVolatile != nil {
		e.AlmIdleVolatile = new(big.Int).Add(e.AlmIdleVolatile, feeVolatile)
	}
	if e.Collateral == nil || e.CvTotalAssets == nil || e.CvTotalSupply == nil {
		return false
	}
	e.RvpsWad = new(big.Int).Set(rvps)
	e.PriceWad = reservationValueAt(e.Collateral, e.CvTotalAssets, e.CvTotalSupply,
		e.CvDecimalsOffset, rvps)
	return true
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
	if !s.coupledStateExact() {
		return nil, ErrInexactCoupledState
	}
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
	if blocked := s.Extra.blockedFor(indexIn == 1); blocked != "" {
		return nil, fmt.Errorf("%w: %s", ErrVenueGateClosed, blocked)
	}
	if indexIn == 1 && !s.baseReferenceGateAttested() {
		return nil, fmt.Errorf("%w: %w", ErrVenueGateClosed, ErrUnattestedReference)
	}

	// Base moved within the route: price a folded COPY and advance only that copy's
	// baselines. Keep its base attached so candidate reverse-liquidity moves can derive
	// the exact post-fill wrapper mark.
	if dS, dV, fS, fV, moved := s.baseDeltas(); moved {
		folded := *s
		rvps, ok := s.baseRvps(s.Extra.AlmSupply)
		if !ok || !foldBaseDeltas(&folded.Extra, dS, dV, fS, fV, rvps) {
			return nil, ErrInexactCoupledState
		}
		totals, accounted, ok := exactBaseBook(s.basePool)
		if !ok {
			return nil, ErrInexactCoupledState
		}
		folded.baseStable0 = new(big.Int).Set(totals[0])
		folded.baseVolatile0 = new(big.Int).Set(totals[1])
		folded.baseAccStable0 = new(big.Int).Set(accounted[0])
		folded.baseAccVolatile0 = new(big.Int).Set(accounted[1])
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
	state := s.quoteState(amountIn)

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
	// The executor passes the swapper's previewed stable leg (rounded up) as the flash
	// cap and the whole amountIn as the volatile cap; the ALM sizes the mint off those
	// two and the swapper sells the surplus back, which is what the physical legs and
	// therefore the net stable out settle by.
	stableLeg, _, ok := state.previewTokenAmounts(shares, true)
	if !ok {
		return nil, ErrSwapRejected
	}
	almRequired := state.cvConvertToAssets(shares, true)
	physStable, physVolatile, dIdleS, dIdleV, mint, ok := state.leverageLegsActual(almRequired, stableLeg, amountIn)
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
	postPrice, postRvps, ok := state.postReservation(true, shares, newColl, postAssets, postSupply)
	if !ok {
		return nil, ErrInexactCoupledState
	}
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
			AlmMintedShares:   mint.Minted,
			AlmUsedStable:     mint.Used0,
			AlmUsedVolatile:   mint.Used1,
			AlmSoldBackShare:  mint.SoldBack,
			FlashStableCap:    stableLeg,
			IdleStableDelta:   dIdleS,
			IdleVolatileDelta: dIdleV,
			NewCollateral:     newColl,
			NewDebt:           newDebt,
			PostCvTotalAssets: postAssets,
			PostCvTotalSupply: postSupply,
			PostRvpsWad:       postRvps,
			PostPriceWad:      postPrice,
		},
	}, nil
}

func (s *PoolSimulator) calcDeleverage(params pool.CalcAmountOutParams,
	amountIn *big.Int) (*pool.CalcAmountOutResult, error) {
	cp := s.curveParams()
	state := s.quoteState(amountIn)

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
	postPrice, postRvps, ok := state.postReservation(false, sharesOut, newColl, postAssets, postSupply)
	if !ok {
		return nil, ErrInexactCoupledState
	}
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
			PostRvpsWad:       postRvps,
			PostPriceWad:      postPrice,
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

func nonNegative(values ...*big.Int) bool {
	for _, v := range values {
		if v == nil || v.Sign() < 0 {
			return false
		}
	}
	return true
}

func completeSwapInfo(si *SwapInfo) bool {
	if !nonNegative(si.CollVaultShares, si.StableLeg, si.VolatileLeg, si.AlmShares,
		si.NewCollateral, si.NewDebt, si.PostCvTotalAssets, si.PostCvTotalSupply,
		si.PostPriceWad) || si.CollVaultShares.Sign() == 0 || si.AlmShares.Sign() == 0 {
		return false
	}
	if si.IsLeverage {
		return nonNegative(si.AlmMintedShares, si.AlmUsedStable, si.AlmUsedVolatile,
			si.AlmSoldBackShare) && si.AlmMintedShares.Sign() > 0 &&
			si.AlmSoldBackShare.Cmp(si.AlmMintedShares) <= 0
	}
	return si.AlmBurned != nil && si.AlmBurned.Sign() > 0
}

// exactSwapInfo rejects every compatibility shape that used to make UpdateBalance
// approximate a deposit/withdraw. CalcAmountOut always emits this complete shape; only
// stale persisted/foreign SwapInfo values are rejected here.
func (s *PoolSimulator) exactSwapInfo(si *SwapInfo) bool {
	e := &s.Extra
	if !completeSwapInfo(si) || !s.hasExactCouplingSnapshot() ||
		si.IdleStableDelta == nil || si.IdleVolatileDelta == nil ||
		si.PostRvpsWad == nil || si.PostRvpsWad.Sign() <= 0 ||
		reservationValueAt(si.NewCollateral, si.PostCvTotalAssets, si.PostCvTotalSupply,
			e.CvDecimalsOffset, si.PostRvpsWad).Cmp(si.PostPriceWad) != 0 {
		return false
	}
	if si.IsLeverage {
		if si.IdleStableDelta.Sign() < 0 || si.IdleVolatileDelta.Sign() < 0 {
			return false
		}
		netMint := new(big.Int).Sub(si.AlmMintedShares, si.AlmSoldBackShare)
		return netMint.Cmp(si.AlmShares) == 0 &&
			new(big.Int).Sub(si.PostCvTotalAssets, e.CvTotalAssets).Cmp(si.AlmShares) == 0
	}
	if si.AlmBurned == nil || si.AlmBurned.Sign() <= 0 ||
		si.IdleStableDelta.Sign() > 0 || si.IdleVolatileDelta.Sign() > 0 {
		return false
	}
	return new(big.Int).Sub(e.CvTotalAssets, si.PostCvTotalAssets).Cmp(si.AlmBurned) == 0
}

func (s *PoolSimulator) UpdateBalance(params pool.UpdateBalanceParams) {
	si, ok := params.SwapInfo.(SwapInfo)
	if !ok {
		return
	}
	if !completeSwapInfo(&si) || !s.coupledStateExact() ||
		(s.basePool != nil && !s.exactSwapInfo(&si)) {
		s.invalidateCoupling()
		return
	}
	// Absorb base movement first (the fill was quoted on the folded state), then
	// advance the baseline so it isn't recounted.
	if dS, dV, fS, fV, moved := s.baseDeltas(); moved {
		rvps, ok := s.baseRvps(s.Extra.AlmSupply)
		if !ok || !foldBaseDeltas(&s.Extra, dS, dV, fS, fV, rvps) {
			s.latchCoupling()
			return
		}
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
	// The collateral-wide debt ceiling is consumed by a leverage fill and released by a
	// deleverage, so a second quote on the same pool sees the headroom the first one
	// left — not the snapshot's.
	if e.SysTotalDebt != nil {
		e.SysTotalDebt = new(big.Int).Add(e.SysTotalDebt, new(big.Int).Sub(si.NewDebt, e.Debt))
		if e.SysTotalDebt.Sign() < 0 {
			e.SysTotalDebt = new(big.Int)
		}
	}
	e.Collateral = si.NewCollateral
	e.Debt = si.NewDebt
	// The raw (unprojected) debt the interest projection starts from moves with the fill
	// too, else a later projection would re-inflate the debt from the stale raw word.
	if e.PosRawDebt != nil && e.PosInterestIndex != nil && e.ActiveInterestIndex != nil && e.ActiveInterestIndex.Sign() > 0 {
		e.PosRawDebt = mulDiv(si.NewDebt, e.PosInterestIndex, e.ActiveInterestIndex)
	}
	// CalcAmountOut carries the venue's separate-floor idle deltas. Never derive them
	// from a net share ratio: leverage is deposit-then-withdraw, and the second floor is
	// taken against the post-deposit book.
	shiftIdle := func() {
		if e.AlmIdleStable != nil && si.IdleStableDelta != nil {
			e.AlmIdleStable = new(big.Int).Add(e.AlmIdleStable, si.IdleStableDelta)
		}
		if e.AlmIdleVolatile != nil && si.IdleVolatileDelta != nil {
			e.AlmIdleVolatile = new(big.Int).Add(e.AlmIdleVolatile, si.IdleVolatileDelta)
		}
	}
	if si.IsLeverage {
		// Reverse coupling replays the exact deposit and optional sell-back. There is no
		// net-share fallback: it prices the withdrawal against a different book.
		if s.basePool != nil && !s.pushLeverageToBase(si, e.AlmSupply) {
			return
		}
		if s.basePool != nil {
			postSupply := new(big.Int).Add(e.AlmSupply, si.AlmShares)
			rvps, ok := s.baseRvps(postSupply)
			if !ok || rvps.Cmp(si.PostRvpsWad) != 0 {
				s.latchCoupling()
				return
			}
		}
		shiftIdle()
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
		if s.basePool != nil && !s.pushFillToBase(new(big.Int).Neg(almBurned), e.AlmSupply, nil, nil) {
			return
		}
		if s.basePool != nil {
			postSupply := new(big.Int).Sub(e.AlmSupply, almBurned)
			rvps, ok := s.baseRvps(postSupply)
			if !ok || rvps.Cmp(si.PostRvpsWad) != 0 {
				s.latchCoupling()
				return
			}
		}
		shiftIdle()
		e.AlmStableReserve = new(big.Int).Sub(e.AlmStableReserve, si.StableLeg)
		e.AlmVolatileReserve = new(big.Int).Sub(e.AlmVolatileReserve, si.VolatileLeg)
		e.AlmSupply = new(big.Int).Sub(e.AlmSupply, almBurned)
		e.RefStableReserve = new(big.Int).Sub(e.RefStableReserve, si.StableLeg)
		e.RefAssetReserve = new(big.Int).Sub(e.RefAssetReserve, si.VolatileLeg)
	}
	// Exact post-fill vault words computed at quote time.
	if si.PostCvTotalAssets != nil && si.PostCvTotalSupply != nil {
		e.CvTotalAssets = si.PostCvTotalAssets
		e.CvTotalSupply = si.PostCvTotalSupply
	}
	// The fill's own CollVault mint/burn moves the reservation value the next quote
	// prices from (rvps stays put under the pro-rata ALM leg).
	if si.PostPriceWad != nil {
		e.PriceWad = si.PostPriceWad
	}
	if si.PostRvpsWad != nil {
		e.RvpsWad = si.PostRvpsWad
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
		} else {
			cloned.basePool = nil
			cloned.couplingExact = false
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
