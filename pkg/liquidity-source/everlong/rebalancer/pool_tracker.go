package everlongrebalancer

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/goccy/go-json"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	pooltrack "github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool/tracker"
)

type PoolTracker struct {
	config       *Config
	ethrpcClient *ethrpc.Client
}

var (
	_                           = pooltrack.RegisterFactoryCE0(DexType, NewPoolTracker)
	_ pool.IBatchRPCPoolTracker = (*PoolTracker)(nil)
)

func NewPoolTracker(cfg *Config, ethrpcClient *ethrpc.Client) *PoolTracker {
	return &PoolTracker{
		config:       cfg,
		ethrpcClient: ethrpcClient,
	}
}

func (t *PoolTracker) GetNewPoolState(ctx context.Context, p entity.Pool,
	_ pool.GetNewPoolStateParams) (entity.Pool, error) {
	return t.getNewPoolState(ctx, p, nil)
}

func (t *PoolTracker) GetNewPoolStateWithOverrides(ctx context.Context, p entity.Pool,
	params pool.GetNewPoolStateWithOverridesParams) (entity.Pool, error) {
	// Flash-fee identity and the EIP-1967 implementation are probed outside the
	// Multicall3 snapshot. Raw Call/StorageAt probes do not inherit state overrides, so a
	// non-empty set would combine overridden contract state with real-chain gates. Exact
	// simulation is preferable to an internally impossible synthetic snapshot.
	if len(params.Overrides) != 0 {
		return p, ErrStateOverridesUnsupported
	}
	return t.getNewPoolState(ctx, p, params.Overrides)
}

// rpcState holds one refresh's raw decode targets.
type rpcState struct {
	exchangeState exchangeStateRaw
	totalAmounts  totalAmountsRaw
	refReserves   reservesAtReferenceRaw
	almSupply     *big.Int
	cvTotalAssets *big.Int
	cvTotalSupply *big.Int
	withdrawFeeBp *big.Int
	minNetDebt    *big.Int
	interestRate  *big.Int
	rvpsWad       *big.Int
	mcrWad        *big.Int
	icrPriceWad   *big.Int
	activeIndex   *big.Int
	lastIndexUpd  *big.Int
	position      positionsRaw
	pending       pendingRewardsRaw
	crFloorWad    *big.Int
	levCurve      levCurveRaw
	almResvPrice  *big.Int
	almIdleStable *big.Int
	almIdleVol    *big.Int
	ccrWad        *big.Int
	sysBalances   systemBalancesRaw
	settlementSw  common.Address
	// Execution-gate words. Bool getters decode into a big.Int, so nil always means the
	// read did not land — which every gate treats as closed.
	mintAllowed   *big.Int
	corePaused    *big.Int
	pmPaused      *big.Int
	pmSunsetting  *big.Int
	maxSysDebt    *big.Int
	defaultedDebt *big.Int
	activeDebt    *big.Int
	borrowingRate *big.Int
	almPaused     *big.Int
	flashFeeBp    *big.Int // from-scoped probe, outside the multicall
	managedVault  common.Address
	implSlot      *big.Int // EIP-1967 implementation word, outside the multicall
	mathProbe     *mathProbeArgs
	mathProbeOut  struct {
		CollateralOut *big.Int
		NewColl       *big.Int
		NewDebt       *big.Int
	}
	delegated   *big.Int
	isPeriphery *big.Int
}

// levCurveRaw decodes leverageCurve()'s 13-word C1 tuple (hZero stays a library
// constant on-chain; q0 is implicitly zero).
type levCurveRaw struct {
	HJoin *big.Int
	HWall *big.Int
	Width *big.Int
	DJoin *big.Int
	DWall *big.Int
	P0    *big.Int
	P1    *big.Int
	P2    *big.Int
	P3    *big.Int
	Q1    *big.Int
	Q2    *big.Int
	Q3    *big.Int
	Q4    *big.Int
}

// newRPCState deliberately pre-allocates NOTHING. The decoder allocates on success, so
// a word left nil means "this read did not land" — and that distinction is the only
// signal a partial snapshot has. Pre-allocating to zero destroys it: a call that
// succeeds on-chain but fails to decode would keep the zero, and every downstream guard
// tests Sign()==0 or !=nil, so "absent" would read as a legitimate value. Zero means
// leverage is enabled, the ICR leg is skipped, the deleverage ceiling is the full debt.
func newRPCState() *rpcState {
	return &rpcState{}
}

// addRPCCalls plans the whole refresh. Shared by the direct and batched paths so the
// two cannot drift. The returned index marks the getReservesAtReference call — the ONE
// read allowed to revert without killing the snapshot: the adapter fails closed when
// spot strays from the reference oracle, and the rebalancer consults it only inside
// increaseLeverage, so its failure gates leverage (zero ref words) while deleverage
// stays live — exactly the on-chain behavior.
func addRPCCalls(rawAdd func(*ethrpc.Call, []any), se *StaticExtra, rd *rpcState, mathProbe *mathProbeArgs) (tolerated map[int]bool) {
	tolerated = map[int]bool{}
	numCalls := 0
	add := func(c *ethrpc.Call, o []any) {
		rawAdd(c, o)
		numCalls++
	}
	add(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: se.Rebalancer,
		Method: rebalancerMethodExchangeState,
	}, []any{&rd.exchangeState})
	add(&ethrpc.Call{
		ABI:    almABI,
		Target: se.ALM,
		Method: almMethodGetTotalAmounts,
	}, []any{&rd.totalAmounts})
	add(&ethrpc.Call{
		ABI:    almABI,
		Target: se.ALM,
		Method: erc20MethodTotalSupply,
	}, []any{&rd.almSupply})
	add(&ethrpc.Call{
		ABI:    collVaultABI,
		Target: se.CollVault,
		Method: cvMethodTotalAssets,
	}, []any{&rd.cvTotalAssets})
	add(&ethrpc.Call{
		ABI:    collVaultABI,
		Target: se.CollVault,
		Method: erc20MethodTotalSupply,
	}, []any{&rd.cvTotalSupply})
	add(&ethrpc.Call{
		ABI:    collVaultABI,
		Target: se.CollVault,
		Method: cvMethodGetWithdrawFee,
	}, []any{&rd.withdrawFeeBp})
	tolerated[numCalls] = true
	add(&ethrpc.Call{
		ABI:    almABI,
		Target: se.ALM,
		Method: almMethodGetReservesAtReference,
	}, []any{&rd.refReserves})
	// Live curve reads: PHYSICAL_CR_FLOOR_WAD answers today; leverageCurve() ships
	// with the settable-curve upgrade. Both tolerated so no impl vintage kills the
	// snapshot.
	tolerated[numCalls] = true
	add(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: se.Rebalancer,
		Method: rebalancerMethodPhysicalCrFloor,
	}, []any{&rd.crFloorWad})
	tolerated[numCalls] = true
	add(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: se.Rebalancer,
		Method: rebalancerMethodLeverageCurve,
	}, []any{&rd.levCurve})
	// The endogenous per-ALM-share mark the reservation value derives from; needed to
	// recompute the post-fill reservation value in the fill-acceptance predicate.
	add(&ethrpc.Call{
		ABI:    almABI,
		Target: se.ALM,
		Method: almMethodRvpsWad,
	}, []any{&rd.rvpsWad})
	// The CVAMM's reservation price (stable-base per volatile-base, WAD). A base CVAMM
	// fill moves rvps only through fees landing in idle; this word values that fee leg
	// so the coupled simulator can shift rvps/PriceWad exactly. All three reads are
	// required: without the idle split a withdraw's separate floors cannot be replayed.
	if se.UnderlyingCvamm != "" {
		add(&ethrpc.Call{
			ABI:    cvammALMABI,
			Target: se.UnderlyingCvamm,
			Method: cvammMethodReservationPriceWad,
		}, []any{&rd.almResvPrice})
		add(&ethrpc.Call{
			ABI:    cvammALMABI,
			Target: se.UnderlyingCvamm,
			Method: cvammMethodIdleStable,
		}, []any{&rd.almIdleStable})
		add(&ethrpc.Call{
			ABI:    cvammALMABI,
			Target: se.UnderlyingCvamm,
			Method: cvammMethodIdleVolatile,
		}, []any{&rd.almIdleVol})
	}
	// Execution gates. Every one is tolerated at the RPC layer so a single revert cannot
	// kill the snapshot — but a word that does not land DISABLES the direction it guards
	// (see gateReasons); it never reads as "gate open".
	tolerated[numCalls] = true
	add(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: se.Rebalancer,
		Method: rebalancerMethodSettlementSwapper,
	}, []any{&rd.settlementSw})
	if se.MintAllowlist != "" {
		tolerated[numCalls] = true
		add(&ethrpc.Call{
			ABI:    depositAllowlistABI,
			Target: se.MintAllowlist,
			Method: allowlistMethodIsDepositAllowed,
			Params: []any{common.HexToAddress(se.Swapper)},
		}, []any{&rd.mintAllowed})
	}
	// The ALM's own pause: buyShares -> deposit is whenNotPaused while withdraw is not,
	// so a paused ALM stops leverage and leaves deleverage live.
	if se.UnderlyingCvamm != "" {
		tolerated[numCalls] = true
		add(&ethrpc.Call{ABI: cvammALMABI, Target: se.UnderlyingCvamm, Method: almMethodPaused},
			[]any{&rd.almPaused})
	}
	// Protocol core: CCR bounds every adjustment; paused() stops debt origination
	// (adjustPosition exempts a pure repayment, i.e. deleverage); isPeriphery is one of
	// the two ways the rebalancer may act for the managed vault.
	if se.Core != "" {
		for _, c := range []struct {
			method string
			out    **big.Int
		}{{coreMethodCcr, &rd.ccrWad}, {coreMethodPaused, &rd.corePaused}} {
			tolerated[numCalls] = true
			add(&ethrpc.Call{ABI: coreABI, Target: se.Core, Method: c.method}, []any{c.out})
		}
		tolerated[numCalls] = true
		add(&ethrpc.Call{ABI: coreABI, Target: se.Core, Method: coreMethodIsPeriphery,
			Params: []any{common.HexToAddress(se.Rebalancer)}}, []any{&rd.isPeriphery})
	}
	// The rebalancer must remain an approved delegate of the managed vault (or be
	// protocol periphery) or every adjustment reverts "Delegate not approved".
	if se.BorrowerOperations != "" && se.ManagedVault != "" {
		tolerated[numCalls] = true
		add(&ethrpc.Call{ABI: borrowerOperationsABI, Target: se.BorrowerOperations,
			Method: boMethodIsApprovedDelegate,
			Params: []any{common.HexToAddress(se.ManagedVault), common.HexToAddress(se.Rebalancer)}},
			[]any{&rd.delegated})
	}
	// The managed vault whose position this venue trades. exchangeState() follows a
	// rotation immediately while every CDP word the tracker reads is keyed by the vault
	// address from listing, so a rotation must block until the pool is relisted.
	tolerated[numCalls] = true
	add(&ethrpc.Call{ABI: rebalancerABI, Target: se.Rebalancer, Method: rebalancerMethodManagedVault},
		[]any{&rd.managedVault})
	// Owner-settable, so it is refreshed rather than resolved once: it bounds how much
	// debt a deleverage may retire (see VaultState.debtRepayCeiling).
	if se.BorrowerOperations != "" {
		add(&ethrpc.Call{
			ABI:    borrowerOperationsABI,
			Target: se.BorrowerOperations,
			Method: boMethodMinNetDebt,
		}, []any{&rd.minNetDebt})
	}
	// Governance-settable: non-zero halts leverage fills at the rebalancer.
	if se.PositionManager != "" {
		add(&ethrpc.Call{
			ABI:    positionManagerABI,
			Target: se.PositionManager,
			Method: pmMethodInterestRate,
		}, []any{&rd.interestRate})
		// CDP-health leg of the fill acceptance: post-fill ICR (coll*price/debt) may sit
		// below MCR*1.2 only when the fill did not decrease it.
		add(&ethrpc.Call{
			ABI:    positionManagerABI,
			Target: se.PositionManager,
			Method: pmMethodMcr,
		}, []any{&rd.mcrWad})
		add(&ethrpc.Call{
			ABI:    positionManagerABI,
			Target: se.PositionManager,
			Method: pmMethodFetchPrice,
		}, []any{&rd.icrPriceWad})
		tolerated[numCalls] = true
		add(&ethrpc.Call{
			ABI:    positionManagerABI,
			Target: se.PositionManager,
			Method: pmMethodEntireSystemBalances,
		}, []any{&rd.sysBalances})
		// Debt-origination gates: the collateral's pause and sunset flags, its system
		// debt ceiling, and the borrowing rate — the rebalancer draws debt with
		// maxFeePercentage 0, so any non-zero rate reverts the fill.
		for _, c := range []struct {
			method string
			out    **big.Int
		}{
			{pmMethodPaused, &rd.pmPaused},
			{pmMethodSunsetting, &rd.pmSunsetting},
			{pmMethodMaxSystemDebt, &rd.maxSysDebt},
			{pmMethodDefaultedDebt, &rd.defaultedDebt},
			{pmMethodTotalActiveDebt, &rd.activeDebt},
			{pmMethodBorrowingRate, &rd.borrowingRate},
		} {
			tolerated[numCalls] = true
			add(&ethrpc.Call{ABI: positionManagerABI, Target: se.PositionManager, Method: c.method}, []any{c.out})
		}
		// Interest-drift venue: once governance enables interest the debt compounds per
		// second with no event; rawDebt*activeIndex/posIndex reproduces it at any time.
		add(&ethrpc.Call{
			ABI:    positionManagerABI,
			Target: se.PositionManager,
			Method: pmMethodActiveInterestIndex,
		}, []any{&rd.activeIndex})
		add(&ethrpc.Call{
			ABI:    positionManagerABI,
			Target: se.PositionManager,
			Method: pmMethodLastActiveIndexUpdate,
		}, []any{&rd.lastIndexUpd})
		if se.ManagedVault != "" {
			add(&ethrpc.Call{
				ABI:    positionManagerABI,
				Target: se.PositionManager,
				Method: pmMethodPositions,
				Params: []any{common.HexToAddress(se.ManagedVault)},
			}, []any{&rd.position})
			add(&ethrpc.Call{
				ABI:    positionManagerABI,
				Target: se.PositionManager,
				Method: pmMethodPendingRewards,
				Params: []any{common.HexToAddress(se.ManagedVault)},
			}, []any{&rd.pending})
		}
	}
	// Re-attest the linked math against the local model on every refresh: listing proved
	// the two agree, but the rebalancer is a proxy and its library can be relinked under
	// a pool that is already listed. The probe reuses the PREVIOUS snapshot's debt (the
	// planner runs before any result is in), and buildPoolState blocks deleverage on a
	// mismatch — deleverage is the direction whose gross the executor re-derives against
	// this library on-chain.
	if mathProbe != nil && se.Math != "" {
		rd.mathProbe = mathProbe
		tolerated[numCalls] = true
		add(&ethrpc.Call{
			ABI:    mathABI,
			Target: se.Math,
			Method: mathMethodDeleverageQuote,
			Params: []any{mathProbe.Collateral, mathProbe.Debt, mathProbe.PriceWad,
				se.CurveParams.LeverageRatioWad, mathProbe.SpreadPpm, mathProbe.StableIn},
		}, []any{&rd.mathProbeOut})
	}
	return tolerated
}

// mathProbeArgs is one pure evaluation of deleverageQuote, fixed at plan time so the
// deployed library and the local model are asked exactly the same question.
type mathProbeArgs struct {
	Collateral, Debt, PriceWad, SpreadPpm, StableIn *big.Int
}

// mathProbeFor takes the re-attestation point from the snapshot the pool already
// carries: the previous state with a quarter of its debt as the lot, which is
// nondegenerate at any state the venue quotes. The inputs need not be current — the
// function is pure, so a relinked or reparameterised library shows up regardless.
// Returns nil right after listing (no Extra yet), where listing's own multi-point check
// has just run against live state.
func mathProbeFor(p entity.Pool) *mathProbeArgs {
	if p.Extra == "" {
		return nil
	}
	var prev Extra
	if json.Unmarshal([]byte(p.Extra), &prev) != nil {
		return nil
	}
	if prev.Debt == nil || prev.Debt.Sign() == 0 || prev.Collateral == nil ||
		prev.PriceWad == nil || prev.SpreadPpm == nil {
		return nil
	}
	return &mathProbeArgs{
		Collateral: prev.Collateral, Debt: prev.Debt, PriceWad: prev.PriceWad,
		SpreadPpm: prev.SpreadPpm, StableIn: new(big.Int).Div(prev.Debt, big.NewInt(4)),
	}
}

// LazyNewPoolState plans the refresh without executing it, so pool-service can batch
// these calls with other pools'. Every required word is validated in the closure:
// individual calls can revert independently, and a partial snapshot must fail rather
// than produce a corrupt vault state.
func (t *PoolTracker) LazyNewPoolState(ctx context.Context, p entity.Pool,
	_ pool.GetNewPoolStateParams) (pool.ILazyRequest, func(*big.Int) (entity.Pool, error), error) {
	var staticExtra StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &staticExtra); err != nil {
		return nil, nil, err
	}
	rd := newRPCState()
	req := pool.LazyRequest{Request: t.ethrpcClient.NewRequest().SetContext(ctx)}
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &staticExtra, rd, mathProbeFor(p))
	return &req, func(blockNumber *big.Int) (entity.Pool, error) {
		// Two reads cannot ride a multicall (a from-scoped call and a storage slot); they
		// run here, after the batch, so the batched path gates on the same facts as the
		// direct one rather than on listing-time assumptions.
		t.probeOutsideMulticall(ctx, &staticExtra, rd, blockNumber)
		return buildPoolState(p, &staticExtra, rd, blockNumber)
	}, nil
}

// getNewPoolState refreshes the full vault snapshot in ONE multicall (all reads pinned
// to the same block): exchangeState + the ALM/CollVault words the CR math and the
// token-leg preview need, incl. the reference reserves bounding the max leverage lot.
func (t *PoolTracker) getNewPoolState(ctx context.Context, p entity.Pool,
	overrides map[common.Address]gethclient.OverrideAccount) (entity.Pool, error) {
	var staticExtra StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &staticExtra); err != nil {
		return p, err
	}

	rd := newRPCState()
	req := t.ethrpcClient.NewRequest().SetContext(ctx)
	if overrides != nil {
		req.SetOverrides(overrides)
	}
	tolerated := addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &staticExtra, rd, mathProbeFor(p))

	// TryBlockAndAggregate: per-call tolerance like TryAggregate — getReservesAtReference may
	// revert on its own (reference oracle
	// stale / spot outside the oracle neighborhood) while the venue keeps filling
	// deleverage on-chain; every other read failing means the venue itself is down.
	resp, err := req.TryBlockAndAggregate()
	if err == nil {
		t.probeOutsideMulticall(ctx, &staticExtra, rd, resp.BlockNumber)
	}
	if err != nil {
		return p, err
	}
	for i, ok := range resp.Result {
		if !ok && !tolerated[i] {
			return p, ErrInvalidSnapshotWord
		}
	}
	var blockNumber *big.Int
	if resp.BlockNumber != nil {
		blockNumber = resp.BlockNumber
	}
	return buildPoolState(p, &staticExtra, rd, blockNumber)
}

// buildPoolState assembles the refreshed entity from decoded call results. On a batched
// path individual calls can revert independently, so every required word is validated
// here rather than trusted.
func buildPoolState(p entity.Pool, staticExtra *StaticExtra, rd *rpcState,
	blockNumber *big.Int) (entity.Pool, error) {
	// Required words, validated HERE rather than off the direct path's tolerated map: the
	// batched path never sees that map, so anything checked only there would fail closed
	// on one path and degrade silently on the other. The set below mirrors exactly which
	// calls addRPCCalls leaves un-tolerated, under the same conditions.
	required := []*big.Int{rd.exchangeState.Collateral, rd.exchangeState.Debt,
		rd.exchangeState.PriceWad, rd.exchangeState.SpreadPpm,
		rd.totalAmounts.StableReserve, rd.totalAmounts.VolatileReserve,
		rd.almSupply, rd.cvTotalAssets, rd.cvTotalSupply, rd.withdrawFeeBp, rd.rvpsWad}
	if staticExtra.BorrowerOperations != "" {
		required = append(required, rd.minNetDebt)
	}
	if staticExtra.PositionManager != "" {
		required = append(required, rd.interestRate, rd.mcrWad, rd.icrPriceWad,
			rd.activeIndex, rd.lastIndexUpd)
	}
	if staticExtra.UnderlyingCvamm != "" {
		required = append(required, rd.almResvPrice, rd.almIdleStable, rd.almIdleVol)
	}
	for _, v := range required {
		if v == nil || v.Sign() < 0 {
			return p, ErrInvalidSnapshotWord
		}
	}
	// getReservesAtReference gates only increaseLeverage on-chain: an unreadable
	// reference leg zeroes the ref words so leverage sizes to 0 while deleverage keeps
	// quoting, instead of failing the snapshot.
	if rd.refReserves.StableReserve == nil || rd.refReserves.AssetReserve == nil ||
		rd.refReserves.RawReferenceWad == nil ||
		rd.refReserves.StableReserve.Sign() < 0 || rd.refReserves.AssetReserve.Sign() < 0 ||
		rd.refReserves.RawReferenceWad.Sign() < 0 {
		rd.refReserves = reservesAtReferenceRaw{
			StableReserve: new(big.Int), AssetReserve: new(big.Int), RawReferenceWad: new(big.Int),
		}
	}

	// Redistribution debt must decode when a managed vault is tracked: a silent nil here
	// (the historic unnamed-outputs unpack failure) would drop pending debt from the
	// interest projection and overstate ICR.
	if staticExtra.ManagedVault != "" &&
		(rd.pending.CollReward == nil || rd.pending.DebtReward == nil ||
			rd.pending.CollReward.Sign() < 0 || rd.pending.DebtReward.Sign() < 0) {
		return p, ErrInvalidSnapshotWord
	}

	// Live curve overlay, frozen fallback: absent reads leave the frozen values.
	var liveCurve *CurveParams
	if rd.crFloorWad != nil && rd.crFloorWad.Sign() > 0 {
		lc := staticExtra.CurveParams
		lc.PhysicalCrFloorWad = rd.crFloorWad
		liveCurve = &lc
	}
	if rd.levCurve.HJoin != nil && rd.levCurve.HWall != nil {
		lc := staticExtra.CurveParams
		if liveCurve != nil {
			lc = *liveCurve
		}
		lc.HJoin = rd.levCurve.HJoin
		lc.HWall = rd.levCurve.HWall
		lc.Width = rd.levCurve.Width
		lc.DJoin = rd.levCurve.DJoin
		lc.DWall = rd.levCurve.DWall
		lc.BezierPhi = [4]*big.Int{rd.levCurve.P0, rd.levCurve.P1, rd.levCurve.P2, rd.levCurve.P3}
		lc.BezierIntegral = [5]*big.Int{new(big.Int), rd.levCurve.Q1, rd.levCurve.Q2, rd.levCurve.Q3, rd.levCurve.Q4}
		// The overlay REPLACES the validated frozen constants for every quote, so it has
		// to clear the same bar. A tolerated read can decode a partial or degenerate
		// tuple; adopting it would divide by zero or panic in the lerp on the next quote.
		if lc.usable() {
			liveCurve = &lc
		}
	}

	if staticExtra.UnderlyingCvamm != "" &&
		(rd.almResvPrice.Sign() <= 0 ||
			rd.almIdleStable.Cmp(rd.totalAmounts.StableReserve) > 0 ||
			rd.almIdleVol.Cmp(rd.totalAmounts.VolatileReserve) > 0) {
		return p, ErrInvalidSnapshotWord
	}
	levBlock, dlvBlock := gateReasons(staticExtra, rd)
	if reason := mathAttestation(staticExtra, rd); reason != "" {
		// the linked library prices BOTH directions
		if levBlock == "" {
			levBlock = reason
		}
		if dlvBlock == "" {
			dlvBlock = reason
		}
	}
	// CCR with the system totals, or none of them (an incomplete pair blocks both
	// directions above, so this only shapes what the predicate sees).
	var ccrWad, sysColl, sysDebt *big.Int
	if rd.ccrWad != nil && rd.ccrWad.Sign() > 0 &&
		rd.sysBalances.Coll != nil && rd.sysBalances.Debt != nil &&
		rd.sysBalances.Coll.Sign() >= 0 && rd.sysBalances.Debt.Sign() >= 0 {
		ccrWad, sysColl, sysDebt = rd.ccrWad, rd.sysBalances.Coll, rd.sysBalances.Debt
	}
	var maxSysDebt, sysTotalDebt *big.Int
	if rd.maxSysDebt != nil && rd.defaultedDebt != nil && rd.activeDebt != nil &&
		rd.maxSysDebt.Sign() >= 0 && rd.defaultedDebt.Sign() >= 0 && rd.activeDebt.Sign() >= 0 {
		maxSysDebt = rd.maxSysDebt
		sysTotalDebt = new(big.Int).Add(rd.activeDebt, rd.defaultedDebt)
	}

	extraBytes, err := json.Marshal(Extra{
		LiveCurve:  liveCurve,
		Collateral: rd.exchangeState.Collateral, Debt: rd.exchangeState.Debt,
		PriceWad: rd.exchangeState.PriceWad, SpreadPpm: rd.exchangeState.SpreadPpm,
		AlmStableReserve:   rd.totalAmounts.StableReserve,
		AlmVolatileReserve: rd.totalAmounts.VolatileReserve,
		AlmIdleStable:      rd.almIdleStable,
		AlmIdleVolatile:    rd.almIdleVol,
		AlmSupply:          rd.almSupply,
		CvTotalAssets:      rd.cvTotalAssets, CvTotalSupply: rd.cvTotalSupply,
		CvDecimalsOffset: staticExtra.CvDecimalsOffset, WithdrawFeeBp: rd.withdrawFeeBp,
		RefStableReserve: rd.refReserves.StableReserve, RefAssetReserve: rd.refReserves.AssetReserve,
		RefRawReferenceWad:    rd.refReserves.RawReferenceWad,
		MinNetDebt:            rd.minNetDebt,
		InterestRate:          rd.interestRate,
		DebtGasCompensation:   staticExtra.DebtGasCompensation,
		RvpsWad:               rd.rvpsWad,
		AlmResvPriceWad:       rd.almResvPrice,
		McrWad:                rd.mcrWad,
		IcrPriceWad:           rd.icrPriceWad,
		CcrWad:                ccrWad,
		SysCollateral:         sysColl,
		SysDebt:               sysDebt,
		LeverageBlockedBy:     levBlock,
		DeleverageBlockedBy:   dlvBlock,
		MaxSystemDebt:         maxSysDebt,
		SysTotalDebt:          sysTotalDebt,
		ActiveInterestIndex:   rd.activeIndex,
		LastActiveIndexUpdate: rd.lastIndexUpd,
		PosRawDebt:            rd.position.Debt,
		PosInterestIndex:      rd.position.ActiveInterestIndex,
		PendingDebtReward:     rd.pending.DebtReward,
	})
	if err != nil {
		return p, err
	}

	p.Extra = string(extraBytes)
	// The ALM reserves are the physical inventory both swap directions settle against —
	// the router-visible liquidity proxy.
	p.Reserves = entity.PoolReserves{rd.totalAmounts.StableReserve.String(),
		rd.totalAmounts.VolatileReserve.String()}
	p.Timestamp = time.Now().Unix()
	if blockNumber != nil {
		p.BlockNumber = blockNumber.Uint64()
	}
	return p, nil
}

// gateReasons folds the execution-gate reads into a reason per direction. Every gate is
// evaluated FAIL CLOSED: a word that did not decode disables the direction it guards,
// because against these known ABIs a missing answer means the venue moved under us —
// treating it as "gate open" is exactly how a quoted fill turns into a deterministic
// revert.
//
// Which gate binds which direction comes from the contracts:
//   - the settlement swapper, the borrower gates, the delegation and the flash-fee
//     exemption sit on the shared path (CollateralRebalancerSwapper._startSwap,
//     BorrowerOperations.callerOrDelegated, DebtToken.flashFee);
//   - debt origination is leverage-only: BorrowerOperations.adjustPosition exempts a
//     pure repayment from CORE.paused(), the ALM's whenNotPaused sits on deposit and not
//     on withdraw, the mint allowlist gates buyShares, and the CDP's pause, sunset,
//     debt ceiling and borrowing fee all bite only on the debt increase.
func gateReasons(se *StaticExtra, rd *rpcState) (leverage, deleverage string) {
	both := func(reason string) {
		if leverage == "" {
			leverage = reason
		}
		if deleverage == "" {
			deleverage = reason
		}
	}
	lev := func(reason string) {
		if leverage == "" {
			leverage = reason
		}
	}
	// bool words decode into a big.Int: nil means the read did not land.
	isTrue := func(v *big.Int) bool { return v != nil && v.Sign() != 0 }

	switch {
	case rd.settlementSw == (common.Address{}):
		both("settlementSwapper() unreadable")
	case !strings.EqualFold(rd.settlementSw.Hex(), se.Swapper):
		both("settlementSwapper() rotated away from this pool")
	}
	if se.Core != "" {
		if rd.ccrWad == nil || rd.ccrWad.Sign() <= 0 || rd.sysBalances.Coll == nil || rd.sysBalances.Debt == nil {
			both("CCR / system balances unreadable")
		}
		if rd.corePaused == nil {
			lev("CORE.paused() unreadable")
		} else if isTrue(rd.corePaused) {
			lev("the protocol core is paused")
		}
	}
	if se.BorrowerOperations != "" && se.ManagedVault != "" {
		switch {
		case rd.delegated == nil && rd.isPeriphery == nil:
			both("delegate authorization unreadable")
		case !isTrue(rd.delegated) && !isTrue(rd.isPeriphery):
			both("the rebalancer is no longer an approved delegate of the managed vault")
		}
	}
	if rd.flashFeeBp == nil {
		both("the swapper's flash-loan fee is unreadable")
	} else if rd.flashFeeBp.Sign() != 0 {
		both("the swapper lost its zero flash-loan fee")
	}
	if se.ManagedVault != "" {
		switch {
		case rd.managedVault == (common.Address{}):
			both("managedVault() unreadable")
		case !strings.EqualFold(rd.managedVault.Hex(), se.ManagedVault):
			both("the managed vault rotated; relist")
		}
	}
	if se.Implementation != "" {
		switch {
		case rd.implSlot == nil:
			both("the rebalancer's implementation slot is unreadable")
		case !strings.EqualFold(common.BigToAddress(rd.implSlot).Hex(), se.Implementation):
			both("the rebalancer was upgraded; the linked math must be re-attested")
		}
	}
	if se.MintAllowlist != "" {
		if rd.mintAllowed == nil {
			lev("the ALM mint allowlist is unreadable")
		} else if !isTrue(rd.mintAllowed) {
			lev("the swapper is not allowed to mint ALM shares")
		}
	}
	if se.UnderlyingCvamm != "" {
		if rd.almPaused == nil {
			lev("the ALM's paused() is unreadable")
		} else if isTrue(rd.almPaused) {
			lev("the ALM is paused")
		}
	}
	if se.PositionManager != "" {
		switch {
		case rd.pmPaused == nil || rd.pmSunsetting == nil:
			lev("the collateral's pause / sunset flags are unreadable")
		case isTrue(rd.pmPaused):
			lev("the collateral is paused")
		case isTrue(rd.pmSunsetting):
			lev("the collateral is sunsetting")
		}
		switch {
		case rd.maxSysDebt == nil || rd.defaultedDebt == nil || rd.activeDebt == nil:
			lev("the system debt ceiling is unreadable")
		case rd.maxSysDebt.Sign() == 0:
			lev("the system debt ceiling is zero")
		}
		if rd.borrowingRate == nil {
			lev("the borrowing rate is unreadable")
		} else if rd.borrowingRate.Sign() != 0 {
			// The rebalancer draws debt with maxFeePercentage 0, so any fee is refused.
			lev("the CDP charges a borrowing fee")
		}
	}
	return leverage, deleverage
}

// mathAttestation re-checks the deployed CollRebalancerMath against the local model on
// the plan-time probe. A mismatch means the library the executor will re-derive against
// is no longer the one this port models, which makes every partial deleverage a guess:
// the direction is disabled until listing re-attests it.
func mathAttestation(se *StaticExtra, rd *rpcState) string {
	if rd.mathProbe == nil || se.Math == "" {
		return ""
	}
	cp := se.CurveParams
	if rd.mathProbeOut.CollateralOut == nil {
		return "the linked CollRebalancerMath did not answer"
	}
	wantOut, wantColl, wantDebt := cp.deleverageQuote(rd.mathProbe.Collateral, rd.mathProbe.Debt,
		rd.mathProbe.PriceWad, cp.LeverageRatioWad, rd.mathProbe.SpreadPpm, rd.mathProbe.StableIn)
	if rd.mathProbeOut.CollateralOut.Cmp(wantOut) != 0 ||
		rd.mathProbeOut.NewColl.Cmp(wantColl) != 0 || rd.mathProbeOut.NewDebt.Cmp(wantDebt) != 0 {
		return "the linked CollRebalancerMath disagrees with the local model"
	}
	return ""
}

// probeOutsideMulticall performs the two reads a multicall cannot: the swapper's flash
// fee, which the debt token keys by msg.sender (MetaCore's getter behind it likewise
// answers only when asked by the token), and the rebalancer proxy's implementation
// slot. A failure leaves the word nil, which gateReasons treats as closed.
func (t *PoolTracker) probeOutsideMulticall(ctx context.Context, se *StaticExtra, rd *rpcState, block *big.Int) {
	if se.Swapper != "" && se.Stable() != "" {
		fee := new(big.Int)
		req := t.ethrpcClient.NewRequest().SetContext(ctx).SetFrom(common.HexToAddress(se.Swapper))
		if block != nil {
			req.SetBlockNumber(block)
		}
		req.AddCall(&ethrpc.Call{
			ABI:    debtTokenABI,
			Target: se.Stable(),
			Method: debtMethodFlashFee,
			Params: []any{common.HexToAddress(se.Stable()), bigWad},
		}, []any{&fee})
		if _, err := req.Call(); err == nil {
			rd.flashFeeBp = fee
		}
	}
	if se.Implementation != "" {
		if word, err := t.ethrpcClient.GetETHClient().StorageAt(ctx, common.HexToAddress(se.Rebalancer), eip1967ImplSlot, block); err == nil {
			rd.implSlot = new(big.Int).SetBytes(word)
		}
	}
}

// eip1967ImplSlot = keccak256("eip1967.proxy.implementation") - 1.
var eip1967ImplSlot = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")
