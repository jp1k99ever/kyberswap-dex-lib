package everlongcvamm

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

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

// GetNewPoolStateAtBlock pins the whole refresh to one historical block (replay and
// coupling verification against settled fills).
func (t *PoolTracker) GetNewPoolStateAtBlock(ctx context.Context, p entity.Pool,
	block *big.Int) (entity.Pool, error) {
	var se StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &se); err != nil {
		return p, err
	}
	rd := newRPCState()
	req := t.ethrpcClient.NewRequest().SetContext(ctx).SetBlockNumber(block)
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, p.Address, &se, rd)
	if _, err := req.Aggregate(); err != nil {
		return p, err
	}
	return buildPoolState(p, rd, block)
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
	feeHook         common.Address
	resvPrice       *big.Int
	midFee          *big.Int
	dirSkew         *big.Int
	invSkewKappa    *big.Int
	invSkewBand     *big.Int
	floorStableIn   *big.Int
	floorVolatileIn *big.Int
	ffad            ffadStateRaw
	ffadEnabled     bool
	ffadRefRate     *big.Int
	ffadSatRate     *big.Int
	ffadFloorS      *big.Int
	ffadFloorV      *big.Int
	curvature       *big.Int
	lpFee           *big.Int
	outFee          *big.Int
	sigmaRef        *big.Int
	volBeta         *big.Int
	volMin          *big.Int
	volMax          *big.Int
}

// newRPCState deliberately pre-allocates NOTHING — the decoder allocates on success, so
// nil means "this read did not land". That matters most for the two fee words: pre-
// allocated they would decode-fail to ZERO, the fee gate reads a non-nil pointer, the
// haircut silently vanishes, and the pool advertises a better price than the venue pays.
// Left nil they fail the snapshot in u256() instead.
func newRPCState() *rpcState {
	return &rpcState{}
}

// addRPCCalls plans the whole refresh: the funded band, the authoritative inventory
// coordinate xWad (never the derived price), the anchor, kappa, the accounted reserves
// (solvency), both directional fees (the fee law's realized-variance input is
// unobservable off-chain, so the fee is read, never computed) and the pause flag.
// Shared by the direct and batched paths so the two cannot drift.
func addRPCCalls(add func(*ethrpc.Call, []any), almAddress string, se *StaticExtra, rd *rpcState) {
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
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodFeeHook}, []any{&rd.feeHook})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodReservationPrice},
		[]any{&rd.resvPrice})
	// Fee-law terms, read from the hook pinned at listing so they land in THIS round —
	// a second round cannot run on the batched path. rd.feeHook above is re-read from
	// the ALM and compared in buildPoolState, so a hook swap drops the terms rather
	// than pricing off the wrong one.
	if se != nil && se.FeeHook != "" {
		for _, c := range []struct {
			method string
			out    **big.Int
		}{
			{hookMethodMidFee, &rd.midFee},
			{hookMethodDirSkew, &rd.dirSkew},
			{hookMethodInvSkewKappa, &rd.invSkewKappa},
			{hookMethodInvSkewBand, &rd.invSkewBand},
			{hookMethodCurvature, &rd.curvature},
			{hookMethodLpFee, &rd.lpFee},
			{hookMethodOutFee, &rd.outFee},
			{hookMethodVolSigmaRef, &rd.sigmaRef},
			{hookMethodVolBeta, &rd.volBeta},
			{hookMethodVolMin, &rd.volMin},
			{hookMethodVolMax, &rd.volMax},
		} {
			add(&ethrpc.Call{ABI: feeHookABI, Target: se.FeeHook, Method: c.method}, []any{c.out})
		}
		satRate := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
		add(&ethrpc.Call{ABI: feeHookABI, Target: se.FeeHook, Method: hookMethodHotFeeFloor,
			Params: []any{true, satRate}}, []any{&rd.floorStableIn})
		add(&ethrpc.Call{ABI: feeHookABI, Target: se.FeeHook, Method: hookMethodHotFeeFloor,
			Params: []any{false, satRate}}, []any{&rd.floorVolatileIn})
		// The live push rate and the hook's FFAD constants, so the floor the swap will
		// actually apply (hotFeeFloorWad(dir, rateWad)) is computed exactly in this same
		// round — a second round cannot run on the batched path. Same footing as every
		// hook read above: strict on the direct path's aggregate, and on the batched
		// path a hook build without them simply leaves the saturated bound.
		add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodFfadState}, []any{&rd.ffad})
		add(&ethrpc.Call{ABI: feeHookABI, Target: se.FeeHook, Method: hookMethodFfadEnabled}, []any{&rd.ffadEnabled})
		add(&ethrpc.Call{ABI: feeHookABI, Target: se.FeeHook, Method: hookMethodFfadRefRate}, []any{&rd.ffadRefRate})
		add(&ethrpc.Call{ABI: feeHookABI, Target: se.FeeHook, Method: hookMethodFfadSatRate}, []any{&rd.ffadSatRate})
		add(&ethrpc.Call{ABI: feeHookABI, Target: se.FeeHook, Method: hookMethodFfadStableFloor}, []any{&rd.ffadFloorS})
		add(&ethrpc.Call{ABI: feeHookABI, Target: se.FeeHook, Method: hookMethodFfadVolatileFloor}, []any{&rd.ffadFloorV})
	}
}

// LazyNewPoolState plans the refresh without executing it, so pool-service can batch
// these calls with other pools'. Every field is validated in the closure: individual
// calls can revert independently, and a half-decoded book must fail rather than quote.
func (t *PoolTracker) LazyNewPoolState(ctx context.Context, p entity.Pool,
	_ pool.GetNewPoolStateParams) (pool.ILazyRequest, func(*big.Int) (entity.Pool, error), error) {
	var se StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &se); err != nil {
		return nil, nil, err
	}
	rd := newRPCState()
	req := pool.LazyRequest{Request: t.ethrpcClient.NewRequest().SetContext(ctx)}
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, p.Address, &se, rd)
	return &req, func(blockNumber *big.Int) (entity.Pool, error) {
		return buildPoolState(p, rd, blockNumber)
	}, nil
}

func (t *PoolTracker) getNewPoolState(ctx context.Context, p entity.Pool,
	overrides map[common.Address]gethclient.OverrideAccount) (entity.Pool, error) {
	var se StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &se); err != nil {
		return p, err
	}
	rd := newRPCState()
	req := t.ethrpcClient.NewRequest().SetContext(ctx)
	if overrides != nil {
		req.SetOverrides(overrides)
	}
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, p.Address, &se, rd)

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
	var staticExtraFeeHook string
	if p.StaticExtra != "" {
		var se StaticExtra
		if json.Unmarshal([]byte(p.StaticExtra), &se) == nil {
			staticExtraFeeHook = se.FeeHook
		}
	}
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

	var midFee, dirSkew, invK, invBand, resvP, floorS, floorV, curv, lpFee *uint256.Int
	var outFee, sigmaRef, volBeta, volMin, volMax *uint256.Int
	hookMoved := staticExtraFeeHook != "" &&
		!strings.EqualFold(staticExtraFeeHook, rd.feeHook.Hex())
	if hookMoved {
		// The terms above were read from the old hook, so this refresh prices without
		// them; re-pin so the next one reads the live hook instead of staying degraded.
		var se StaticExtra
		if json.Unmarshal([]byte(p.StaticExtra), &se) == nil {
			se.FeeHook = hexutil.Encode(rd.feeHook[:])
			if b, err := json.Marshal(se); err == nil {
				p.StaticExtra = string(b)
			}
		}
	}
	if !hookMoved && rd.midFee != nil && rd.midFee.Sign() > 0 &&
		rd.resvPrice != nil && rd.resvPrice.Sign() > 0 {
		midFee, _ = uint256.FromBig(rd.midFee)
		dirSkew, _ = uint256.FromBig(rd.dirSkew)
		invK, _ = uint256.FromBig(rd.invSkewKappa)
		invBand, _ = uint256.FromBig(rd.invSkewBand)
		resvP, _ = uint256.FromBig(rd.resvPrice)
		floorS, _ = uint256.FromBig(rd.floorStableIn)
		floorV, _ = uint256.FromBig(rd.floorVolatileIn)
		if liveS, liveV, ok := rd.liveHotFloors(); ok {
			floorS, _ = uint256.FromBig(liveS)
			floorV, _ = uint256.FromBig(liveV)
		}
		curv, _ = uint256.FromBig(rd.curvature)
		lpFee, _ = uint256.FromBig(rd.lpFee)
		outFee, _ = uint256.FromBig(rd.outFee)
		sigmaRef, _ = uint256.FromBig(rd.sigmaRef)
		volBeta, _ = uint256.FromBig(rd.volBeta)
		volMin, _ = uint256.FromBig(rd.volMin)
		volMax, _ = uint256.FromBig(rd.volMax)
	}

	extraBytes, err := json.Marshal(Extra{
		Support:             support,
		XWad:                xWadU,
		AnchorSqrtX96:       anchorU,
		Kappa:               kappaU,
		FeeStableInWad:      feeStableInU,
		FeeVolatileInWad:    feeVolatileInU,
		Paused:              rd.paused,
		MidFeeWad:           midFee,
		DirSkewWad:          dirSkew,
		InvSkewKappaWad:     invK,
		InvSkewBandWad:      invBand,
		ReservationPriceWad: resvP,
		FloorStableInWad:    floorS,
		FloorVolatileInWad:  floorV,
		CurvatureWad:        curv,
		LpFeeWad:            lpFee,
		OutFeeWad:           outFee,
		VolSigmaRefWad:      sigmaRef,
		VolBetaWad:          volBeta,
		VolMinWad:           volMin,
		VolMaxWad:           volMax,
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

// liveHotFloors is ClammFeeHook.hotFeeFloorWad at the LIVE push rate, for both
// directions: zero at or below the reference rate, the full level at or above the
// saturation rate, and level * smoothstep(t) between, t = (rate - ref) / (sat - ref).
// ok is false whenever any input did not decode, which leaves the saturated bound.
func (rd *rpcState) liveHotFloors() (stableIn, volatileIn *big.Int, ok bool) {
	if rd.ffad.RateWad == nil || rd.ffadRefRate == nil || rd.ffadSatRate == nil ||
		rd.ffadFloorS == nil || rd.ffadFloorV == nil || rd.ffadSatRate.Cmp(rd.ffadRefRate) <= 0 {
		return nil, nil, false
	}
	return hotFloorAt(rd.ffadEnabled, rd.ffad.RateWad, rd.ffadRefRate, rd.ffadSatRate, rd.ffadFloorS),
		hotFloorAt(rd.ffadEnabled, rd.ffad.RateWad, rd.ffadRefRate, rd.ffadSatRate, rd.ffadFloorV), true
}

func hotFloorAt(enabled bool, rate, ref, sat, level *big.Int) *big.Int {
	if !enabled || rate.Cmp(ref) <= 0 {
		return new(big.Int)
	}
	if rate.Cmp(sat) >= 0 {
		return new(big.Int).Set(level)
	}
	wad := bigWadFee
	t := new(big.Int).Sub(rate, ref)
	t.Mul(t, wad).Div(t, new(big.Int).Sub(sat, ref))
	tSq := new(big.Int).Mul(t, t)
	tSq.Div(tSq, wad)
	coef := new(big.Int).Mul(wad, big.NewInt(3))
	coef.Sub(coef, new(big.Int).Mul(t, big.NewInt(2)))
	smooth := new(big.Int).Mul(tSq, coef)
	smooth.Div(smooth, wad)
	out := new(big.Int).Mul(level, smooth)
	return out.Div(out, wad)
}
