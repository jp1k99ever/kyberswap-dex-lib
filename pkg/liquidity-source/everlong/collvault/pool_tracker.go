package everlongcollvault

import (
	"context"
	"math/big"
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
}

func newRPCState() *rpcState {
	return &rpcState{
		almSupply: new(big.Int), cvTotalAssets: new(big.Int), cvTotalSupply: new(big.Int),
		withdrawFeeBp: new(big.Int), minNetDebt: new(big.Int), interestRate: new(big.Int),
		rvpsWad: new(big.Int), mcrWad: new(big.Int), icrPriceWad: new(big.Int),
		activeIndex: new(big.Int), lastIndexUpd: new(big.Int),
	}
}

// addRPCCalls plans the whole refresh. Shared by the direct and batched paths so the
// two cannot drift. The returned index marks the getReservesAtReference call — the ONE
// read allowed to revert without killing the snapshot: the adapter fails closed when
// spot strays from the reference oracle, and the rebalancer consults it only inside
// increaseLeverage, so its failure gates leverage (zero ref words) while deleverage
// stays live — exactly the on-chain behavior.
func addRPCCalls(rawAdd func(*ethrpc.Call, []any), se *StaticExtra, rd *rpcState) (refReservesIdx int) {
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
	refReservesIdx = numCalls
	add(&ethrpc.Call{
		ABI:    almABI,
		Target: se.ALM,
		Method: almMethodGetReservesAtReference,
	}, []any{&rd.refReserves})
	// The endogenous per-ALM-share mark the reservation value derives from; needed to
	// recompute the post-fill reservation value in the fill-acceptance predicate.
	add(&ethrpc.Call{
		ABI:    almABI,
		Target: se.ALM,
		Method: almMethodRvpsWad,
	}, []any{&rd.rvpsWad})
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
	return refReservesIdx
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
	refReservesIdx := addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &staticExtra, rd)

	// TryAggregate because getReservesAtReference may revert on its own (reference oracle
	// stale / spot outside the oracle neighborhood) while the venue keeps filling
	// deleverage on-chain; every other read failing means the venue itself is down.
	resp, err := req.TryAggregate()
	if err != nil {
		return p, err
	}
	for i, ok := range resp.Result {
		if !ok && i != refReservesIdx {
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
	for _, v := range []*big.Int{rd.exchangeState.Collateral, rd.exchangeState.Debt,
		rd.exchangeState.PriceWad, rd.exchangeState.SpreadPpm,
		rd.totalAmounts.StableReserve, rd.totalAmounts.VolatileReserve,
		rd.almSupply, rd.cvTotalAssets, rd.cvTotalSupply, rd.withdrawFeeBp} {
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

	extraBytes, err := json.Marshal(Extra{
		Collateral: rd.exchangeState.Collateral, Debt: rd.exchangeState.Debt,
		PriceWad: rd.exchangeState.PriceWad, SpreadPpm: rd.exchangeState.SpreadPpm,
		AlmStableReserve:   rd.totalAmounts.StableReserve,
		AlmVolatileReserve: rd.totalAmounts.VolatileReserve,
		AlmSupply:          rd.almSupply,
		CvTotalAssets:      rd.cvTotalAssets, CvTotalSupply: rd.cvTotalSupply,
		CvDecimalsOffset: staticExtra.CvDecimalsOffset, WithdrawFeeBp: rd.withdrawFeeBp,
		RefStableReserve: rd.refReserves.StableReserve, RefAssetReserve: rd.refReserves.AssetReserve,
		RefRawReferenceWad:    rd.refReserves.RawReferenceWad,
		MinNetDebt:            rd.minNetDebt,
		InterestRate:          rd.interestRate,
		DebtGasCompensation:   staticExtra.DebtGasCompensation,
		RvpsWad:               rd.rvpsWad,
		McrWad:                rd.mcrWad,
		IcrPriceWad:           rd.icrPriceWad,
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
