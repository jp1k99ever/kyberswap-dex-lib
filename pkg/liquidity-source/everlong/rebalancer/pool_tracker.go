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
	mintAllowed   *big.Int // bool word; nil = the read did not land
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
func addRPCCalls(rawAdd func(*ethrpc.Call, []any), se *StaticExtra, rd *rpcState) (tolerated map[int]bool) {
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
	// so the coupled simulator can shift rvps/PriceWad exactly. Tolerated: only the
	// meta-coupling refinement is lost without it.
	if se.UnderlyingCvamm != "" {
		tolerated[numCalls] = true
		add(&ethrpc.Call{
			ABI:    cvammALMABI,
			Target: se.UnderlyingCvamm,
			Method: cvammMethodReservationPriceWad,
		}, []any{&rd.almResvPrice})
		// The idle split, so a redeem's physical legs reproduce the ALM's separate
		// floors. Tolerated: without them the legs fall back to the swapper's preview.
		tolerated[numCalls] = true
		add(&ethrpc.Call{
			ABI:    cvammALMABI,
			Target: se.UnderlyingCvamm,
			Method: cvammMethodIdleStable,
		}, []any{&rd.almIdleStable})
		tolerated[numCalls] = true
		add(&ethrpc.Call{
			ABI:    cvammALMABI,
			Target: se.UnderlyingCvamm,
			Method: cvammMethodIdleVolatile,
		}, []any{&rd.almIdleVol})
	}
	// Venue gates the swap path checks on every call: the registered settlement swapper
	// and the ALM mint allowlist. Tolerated — an absent word leaves that gate
	// unmodelled, never fails the snapshot.
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
	// BorrowerOperations' gates: CCR and the system totals. Tolerated: absent words skip
	// the gates rather than fail the snapshot.
	if se.Core != "" {
		tolerated[numCalls] = true
		add(&ethrpc.Call{
			ABI:    coreABI,
			Target: se.Core,
			Method: coreMethodCcr,
		}, []any{&rd.ccrWad})
	}
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
	return tolerated
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
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &staticExtra, rd)
	return &req, func(blockNumber *big.Int) (entity.Pool, error) {
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
	tolerated := addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &staticExtra, rd)

	// TryBlockAndAggregate: per-call tolerance like TryAggregate — getReservesAtReference may
	// revert on its own (reference oracle
	// stale / spot outside the oracle neighborhood) while the venue keeps filling
	// deleverage on-chain; every other read failing means the venue itself is down.
	resp, err := req.TryBlockAndAggregate()
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

	var resvPrice *big.Int
	if rd.almResvPrice != nil && rd.almResvPrice.Sign() > 0 {
		resvPrice = rd.almResvPrice
	}
	// Both idle words or neither: a half-decoded split would misplace a leg.
	var idleStable, idleVolatile *big.Int
	if rd.almIdleStable != nil && rd.almIdleVol != nil &&
		rd.almIdleStable.Sign() >= 0 && rd.almIdleVol.Sign() >= 0 &&
		rd.almIdleStable.Cmp(rd.totalAmounts.StableReserve) <= 0 &&
		rd.almIdleVol.Cmp(rd.totalAmounts.VolatileReserve) <= 0 {
		idleStable, idleVolatile = rd.almIdleStable, rd.almIdleVol
	}
	swapperRotated := rd.settlementSw != (common.Address{}) &&
		!strings.EqualFold(rd.settlementSw.Hex(), staticExtra.Swapper)
	mintDenied := rd.mintAllowed != nil && rd.mintAllowed.Sign() == 0
	// CCR with the system totals, or none of them.
	var ccrWad, sysColl, sysDebt *big.Int
	if rd.ccrWad != nil && rd.ccrWad.Sign() > 0 &&
		rd.sysBalances.Coll != nil && rd.sysBalances.Debt != nil &&
		rd.sysBalances.Coll.Sign() >= 0 && rd.sysBalances.Debt.Sign() >= 0 {
		ccrWad, sysColl, sysDebt = rd.ccrWad, rd.sysBalances.Coll, rd.sysBalances.Debt
	}

	extraBytes, err := json.Marshal(Extra{
		LiveCurve:  liveCurve,
		Collateral: rd.exchangeState.Collateral, Debt: rd.exchangeState.Debt,
		PriceWad: rd.exchangeState.PriceWad, SpreadPpm: rd.exchangeState.SpreadPpm,
		AlmStableReserve:   rd.totalAmounts.StableReserve,
		AlmVolatileReserve: rd.totalAmounts.VolatileReserve,
		AlmIdleStable:      idleStable,
		AlmIdleVolatile:    idleVolatile,
		AlmSupply:          rd.almSupply,
		CvTotalAssets:      rd.cvTotalAssets, CvTotalSupply: rd.cvTotalSupply,
		CvDecimalsOffset: staticExtra.CvDecimalsOffset, WithdrawFeeBp: rd.withdrawFeeBp,
		RefStableReserve: rd.refReserves.StableReserve, RefAssetReserve: rd.refReserves.AssetReserve,
		RefRawReferenceWad:    rd.refReserves.RawReferenceWad,
		MinNetDebt:            rd.minNetDebt,
		InterestRate:          rd.interestRate,
		DebtGasCompensation:   staticExtra.DebtGasCompensation,
		RvpsWad:               rd.rvpsWad,
		AlmResvPriceWad:       resvPrice,
		McrWad:                rd.mcrWad,
		IcrPriceWad:           rd.icrPriceWad,
		CcrWad:                ccrWad,
		SysCollateral:         sysColl,
		SysDebt:               sysDebt,
		SwapperRotated:        swapperRotated,
		MintDenied:            mintDenied,
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
