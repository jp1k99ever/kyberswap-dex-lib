package everlongcvamm

import (
	"context"
	"math/big"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/goccy/go-json"
	"github.com/holiman/uint256"

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

// getNewPoolState refreshes the whole venue state in ONE multicall (one block): the
// funded band, the authoritative inventory coordinate xWad (never the derived price),
// the anchor, kappa, the accounted reserves (solvency), both directional fees (the fee
// law's realized-variance input is unobservable off-chain, so the fee is read, never
// computed) and the pause flag.
// rpcState holds one refresh's raw decode targets.
type rpcState struct {
	sup             supportRaw
	xWad            *big.Int
	anchor          *big.Int
	kappa           *big.Int
	reserveStable   *big.Int
	reserveVolatile *big.Int
	feeStableIn     *big.Int
	feeVolatileIn   *big.Int
	paused          bool
}

func newRPCState() *rpcState {
	return &rpcState{
		xWad: new(big.Int), anchor: new(big.Int), kappa: new(big.Int),
		reserveStable: new(big.Int), reserveVolatile: new(big.Int),
		feeStableIn: new(big.Int), feeVolatileIn: new(big.Int),
	}
}

// addRPCCalls plans the whole refresh: the funded band, the authoritative inventory
// coordinate xWad (never the derived price), the anchor, kappa, the accounted reserves
// (solvency), both directional fees (the fee law's realized-variance input is
// unobservable off-chain, so the fee is read, never computed) and the pause flag.
// Shared by the direct and batched paths so the two cannot drift.
func addRPCCalls(add func(*ethrpc.Call, []any), almAddress string, rd *rpcState) {
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodGetSupport}, []any{&rd.sup})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodXWad}, []any{&rd.xWad})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodAnchorSqrtCurveX96}, []any{&rd.anchor})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodKappa}, []any{&rd.kappa})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodReserveStable}, []any{&rd.reserveStable})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodReserveVolatile}, []any{&rd.reserveVolatile})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodPoolFeeDirectional,
		Params: []any{true}}, []any{&rd.feeStableIn})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodPoolFeeDirectional,
		Params: []any{false}}, []any{&rd.feeVolatileIn})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodPaused}, []any{&rd.paused})
}

// LazyNewPoolState plans the refresh without executing it, so pool-service can batch
// these calls with other pools'. Every field is validated in the closure: individual
// calls can revert independently, and a half-decoded book must fail rather than quote.
func (t *PoolTracker) LazyNewPoolState(ctx context.Context, p entity.Pool,
	_ pool.GetNewPoolStateParams) (pool.ILazyRequest, func(*big.Int) (entity.Pool, error), error) {
	rd := newRPCState()
	req := pool.LazyRequest{Request: t.ethrpcClient.NewRequest().SetContext(ctx)}
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, p.Address, rd)
	return &req, func(blockNumber *big.Int) (entity.Pool, error) {
		return buildPoolState(p, rd, blockNumber)
	}, nil
}

func (t *PoolTracker) getNewPoolState(ctx context.Context, p entity.Pool,
	overrides map[common.Address]gethclient.OverrideAccount) (entity.Pool, error) {
	rd := newRPCState()
	req := t.ethrpcClient.NewRequest().SetContext(ctx)
	if overrides != nil {
		req.SetOverrides(overrides)
	}
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, p.Address, rd)

	resp, err := req.Aggregate()
	if err != nil {
		return p, err
	}
	var blockNumber *big.Int
	if resp.BlockNumber != nil {
		blockNumber = resp.BlockNumber
	}
	return buildPoolState(p, rd, blockNumber)
}

// buildPoolState assembles the refreshed entity from decoded call results. Individual
// calls can revert independently on a batched path, so every word is validated here
// rather than trusted: a partially decoded book must error, never quote.
func buildPoolState(p entity.Pool, rd *rpcState, blockNumber *big.Int) (entity.Pool, error) {
	support, err := supportFromRaw(&rd.sup)
	if err != nil {
		return p, err
	}
	xWadU, err := u256(rd.xWad)
	if err != nil {
		return p, err
	}
	anchorU, err := u256(rd.anchor)
	if err != nil {
		return p, err
	}
	kappaU, err := u256(rd.kappa)
	if err != nil {
		return p, err
	}
	feeStableInU, err := u256(rd.feeStableIn)
	if err != nil {
		return p, err
	}
	feeVolatileInU, err := u256(rd.feeVolatileIn)
	if err != nil {
		return p, err
	}
	if rd.reserveStable == nil || rd.reserveVolatile == nil ||
		rd.reserveStable.Sign() < 0 || rd.reserveVolatile.Sign() < 0 {
		return p, ErrOverflow
	}

	extraBytes, err := json.Marshal(Extra{
		Support:          support,
		XWad:             xWadU,
		AnchorSqrtX96:    anchorU,
		Kappa:            kappaU,
		FeeStableInWad:   feeStableInU,
		FeeVolatileInWad: feeVolatileInU,
		Paused:           rd.paused,
	})
	if err != nil {
		return p, err
	}

	p.Extra = string(extraBytes)
	p.Reserves = entity.PoolReserves{rd.reserveStable.String(), rd.reserveVolatile.String()}
	p.Timestamp = time.Now().Unix()
	if blockNumber != nil {
		p.BlockNumber = blockNumber.Uint64()
	}
	return p, nil
}

func supportFromRaw(raw *supportRaw) (Support, error) {
	aWad, err := u256(raw.AWad)
	if err != nil {
		return Support{}, err
	}
	xLo, err := u256(raw.XLo)
	if err != nil {
		return Support{}, err
	}
	xHi, err := u256(raw.XHi)
	if err != nil {
		return Support{}, err
	}
	yHi, err := u256(raw.YHi)
	if err != nil {
		return Support{}, err
	}
	return Support{AWad: aWad, XLo: xLo, XHi: xHi, YHi: yHi}, nil
}

func u256(v *big.Int) (*uint256.Int, error) {
	if v == nil {
		return nil, ErrOverflow
	}
	res, overflow := uint256.FromBig(v)
	if overflow {
		return nil, ErrOverflow
	}
	return res, nil
}
