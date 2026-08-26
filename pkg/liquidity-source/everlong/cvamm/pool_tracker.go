package everlongcvamm

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/ethereum/go-ethereum/rpc"
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
	// The implementation slot, realized-variance slot and hook runtime are deliberately
	// read outside Multicall3 at the snapshot block. go-ethereum state overrides do not
	// apply to those raw probes, so accepting any non-empty override set would splice two
	// possible worlds into one entity. Refuse the composite snapshot rather than expose a
	// first quote whose fee/code/gates were never jointly observed.
	normalized, err := normalizeStateOverrides(params.Overrides)
	if err != nil {
		return p, ErrStateOverridesUnsupported
	}
	// An allocated-but-empty map is semantically the same snapshot as nil. Normalize it
	// here so the exact raw rv/code probes remain enabled; passing the empty map through
	// used to make getNewPoolState mistake it for an override world and silently disable
	// chained fee reconstruction.
	return t.getNewPoolState(ctx, p, normalized)
}

func normalizeStateOverrides(overrides map[common.Address]gethclient.OverrideAccount) (
	map[common.Address]gethclient.OverrideAccount, error) {
	if len(overrides) != 0 {
		return nil, ErrStateOverridesUnsupported
	}
	return nil, nil
}

// GetNewPoolStateAtBlock pins the whole refresh to one historical block (replay and
// coupling verification against settled fills).
func (t *PoolTracker) GetNewPoolStateAtBlock(ctx context.Context, p entity.Pool,
	block *big.Int) (entity.Pool, error) {
	var se StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &se); err != nil {
		return p, err
	}
	if err := validateEntityProfile(p, &se); err != nil {
		return p, err
	}
	rd := newRPCState()
	req := t.ethrpcClient.NewRequest().SetContext(ctx).SetBlockNumber(block)
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, p.Address, &se, rd)
	if _, err := req.Aggregate(); err != nil {
		return p, err
	}
	rd.pausedDecoded, rd.feeHookDecoded = true, true
	if err := t.probeExactInputs(ctx, p.Address, &se, rd, block, true); err != nil {
		return p, err
	}
	return buildPoolState(p, rd, block)
}

// getNewPoolState refreshes the whole venue state in ONE multicall (one block): the
// funded band, the authoritative inventory coordinate xWad (never the derived price),
// the anchor, kappa, accounted reserves, sampled directional fees and pause flag. The
// layout-bound realized variance is appended by probeExactInputs at the same block.
// rpcState holds one refresh's raw decode targets.
type rpcState struct {
	sup              supportRaw
	xWad             *big.Int
	anchor           *big.Int
	kappa            *big.Int
	reserveStable    *big.Int
	reserveVolatile  *big.Int
	feeStableIn      *big.Int
	feeVolatileIn    *big.Int
	paused           bool
	pausedDecoded    bool
	feeHook          common.Address
	feeHookDecoded   bool
	resvPrice        *big.Int
	midFee           *big.Int
	dirSkew          *big.Int
	invSkewKappa     *big.Int
	invSkewBand      *big.Int
	idleStable       *big.Int
	idleVolatile     *big.Int
	floorStableIn    *big.Int
	floorVolatileIn  *big.Int
	hotFloorsExact   bool
	ffad             ffadStateRaw
	curvature        *big.Int
	lpFee            *big.Int
	outFee           *big.Int
	sigmaRef         *big.Int
	volBeta          *big.Int
	volMin           *big.Int
	volMax           *big.Int
	realizedVariance *big.Int
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
// (solvency), sampled directional fees and the pause flag. The storage-only rv word is
// deliberately outside this plan and joined at the aggregate's returned block.
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
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodIdleStable}, []any{&rd.idleStable})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodIdleVolatile}, []any{&rd.idleVolatile})
	add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodReservationPrice},
		[]any{&rd.resvPrice})
	// Fee-law terms, read from the hook pinned at listing so they land in THIS round —
	// a second round cannot run on the batched path. rd.feeHook above is re-read from
	// the ALM and compared in buildPoolState, so a hook swap drops the terms rather
	// than pricing off the wrong one.
	if se != nil && se.FeeHook != "" && common.HexToAddress(se.FeeHook) != (common.Address{}) {
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
		// The push rate is the sole mutable ALM-side input to the optional floor. Once
		// the aggregate returns its block, probeExactInputs attests the live hook runtime
		// and evaluates that exact reviewed bytecode's immutable smoothstep law locally.
		add(&ethrpc.Call{ABI: almABI, Target: almAddress, Method: almMethodFfadState}, []any{&rd.ffad})
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
	if err := validateEntityProfile(p, &se); err != nil {
		return nil, nil, err
	}
	rd := newRPCState()
	req := pool.LazyRequest{Request: t.ethrpcClient.NewRequest().SetContext(ctx)}
	addRPCCalls(func(c *ethrpc.Call, o []any) {
		req.AddCall(c, o)
		if c.Method != almMethodPaused && c.Method != almMethodFeeHook {
			return
		}
		idx, method := len(req.Unpacks)-1, c.Method
		unpack := req.Unpacks[idx]
		req.Unpacks[idx] = func(raw []byte) error {
			if err := unpack(raw); err != nil {
				return err
			}
			if method == almMethodPaused {
				rd.pausedDecoded = true
			} else {
				rd.feeHookDecoded = true
			}
			return nil
		}
	}, p.Address, &se, rd)
	return &req, func(blockNumber *big.Int) (entity.Pool, error) {
		if err := t.probeExactInputs(ctx, p.Address, &se, rd, blockNumber, true); err != nil {
			return p, err
		}
		return buildPoolState(p, rd, blockNumber)
	}, nil
}

func (t *PoolTracker) getNewPoolState(ctx context.Context, p entity.Pool,
	overrides map[common.Address]gethclient.OverrideAccount) (entity.Pool, error) {
	var se StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &se); err != nil {
		return p, err
	}
	if err := validateEntityProfile(p, &se); err != nil {
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
	rd.pausedDecoded, rd.feeHookDecoded = true, true
	var blockNumber *big.Int
	if resp.BlockNumber != nil {
		blockNumber = resp.BlockNumber
	}
	// Raw storage reads cannot honor geth state overrides. The contract-sampled first
	// fee remains exact under overrides, but leaving rv absent makes any chained quote
	// fail closed after the first book move.
	if err := t.probeExactInputs(ctx, p.Address, &se, rd, blockNumber, overrides == nil); err != nil {
		return p, err
	}
	return buildPoolState(p, rd, blockNumber)
}

// probeExactInputs fetches every non-ABI attestation in one JSON-RPC batch after the
// Multicall3 snapshot: proxy implementation, layout-bound rv, and live hook bytecode.
// Every request is pinned to the aggregate's exact block. A stale listed implementation
// is disabled until the list updater republishes it, and unknown hook bytecode keeps the
// directly sampled first quote but cannot authorize chained fee reconstruction.
func (t *PoolTracker) probeExactInputs(ctx context.Context, almAddress string, se *StaticExtra,
	rd *rpcState, block *big.Int, includeExact bool) error {
	rd.hotFloorsExact = false
	if se == nil || se.Implementation == "" {
		return ErrImplementationUnpinned
	}
	if !strings.EqualFold(se.ImplementationCodeHash, supportedImplementationCodeHash.Hex()) {
		return ErrUnsupportedImplementation
	}
	if block == nil {
		return ErrInvalidSnapshotWord
	}
	if !rd.feeHookDecoded {
		return ErrInvalidSnapshotWord
	}

	alm := common.HexToAddress(almAddress)
	blockArg := hexutil.EncodeBig(block)
	var implementationWord, rvWord common.Hash
	var hookCode hexutil.Bytes
	batch := []rpc.BatchElem{{
		Method: "eth_getStorageAt",
		Args:   []any{alm, cvammEIP1967ImplSlot, blockArg},
		Result: &implementationWord,
	}}
	if includeExact {
		batch = append(batch, rpc.BatchElem{
			Method: "eth_getStorageAt",
			Args:   []any{alm, cvammRealizedVarianceSlot, blockArg},
			Result: &rvWord,
		})
	}
	probeHook := includeExact && rd.feeHook != (common.Address{}) &&
		strings.EqualFold(rd.feeHook.Hex(), se.FeeHook) && rd.ffad.RateWad != nil &&
		rd.ffad.RateWad.Sign() >= 0 && rd.ffad.RateWad.BitLen() <= 128
	if probeHook {
		batch = append(batch, rpc.BatchElem{
			Method: "eth_getCode",
			Args:   []any{rd.feeHook, blockArg},
			Result: &hookCode,
		})
	}
	if err := t.ethrpcClient.GetETHClient().Client().BatchCallContext(ctx, batch); err != nil {
		return err
	}
	for _, elem := range batch {
		if elem.Error != nil {
			return elem.Error
		}
	}

	implementation := common.BytesToAddress(implementationWord.Bytes())
	if implementation == (common.Address{}) ||
		!strings.EqualFold(implementation.Hex(), se.Implementation) {
		return ErrImplementationChanged
	}
	if !includeExact {
		return nil
	}
	rd.realizedVariance = new(big.Int).SetBytes(rvWord.Bytes())

	if !probeHook || crypto.Keccak256Hash(hookCode) != supportedFeeHookCodeHash {
		return nil
	}
	// Runtime identity includes this hook's immutable FFAD activation and endpoints.
	// Evaluating the reviewed law locally removes two sequential eth_call round trips;
	// it is exact only under the code-hash attestation above.
	rd.floorStableIn = supportedHookHotFloor(true, rd.ffad.RateWad)
	rd.floorVolatileIn = supportedHookHotFloor(false, rd.ffad.RateWad)
	rd.hotFloorsExact = true
	return nil
}

var (
	// keccak256("eip1967.proxy.implementation") - 1.
	cvammEIP1967ImplSlot = common.HexToHash("0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc")
	// CvammStore STORAGE_LOCATION + 10: CvammStorage.rv in the pinned implementation.
	cvammRealizedVarianceSlot = common.HexToHash("0x77698b472b5fc9a6aa219adffee58d096b1c7df6ef9e3d50be47325f268e150a")
	// Runtime bytecode hash of the verified Berachain CvammALM implementation whose
	// swap/fee law and CvammStore layout this package mirrors. An address change alone
	// does not authorize reusing STORAGE_LOCATION+10: unknown code stays blocked until
	// explicitly reviewed.
	supportedImplementationCodeHash = common.HexToHash("0xcc6532930b94e24d165751accb1e4283bf6effe09e19e894ae2be5aa0643eba3")
	// Runtime bytecode hash of the reviewed deployed ClammFeeHook. Unknown replacement
	// hooks keep their directly sampled first quote but cannot enable chained repricing.
	supportedFeeHookCodeHash = common.HexToHash("0x5023be0997b765c80d67f9ac494c8937861462ea6eccabf00fd0379371617c64")
)

// buildPoolState assembles the refreshed entity from decoded call results. Individual
// calls can revert independently on a batched path, so every word is validated here
// rather than trusted: a partially decoded book must error, never quote.
func buildPoolState(p entity.Pool, rd *rpcState, blockNumber *big.Int) (entity.Pool, error) {
	if rd == nil || !rd.pausedDecoded || !rd.feeHookDecoded {
		return p, ErrInvalidSnapshotWord
	}
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
	resvP, err := u256(rd.resvPrice)
	if err != nil || resvP.IsZero() {
		return p, ErrInvalidSnapshotWord
	}
	if rd.reserveStable == nil || rd.reserveVolatile == nil ||
		rd.reserveStable.Sign() < 0 || rd.reserveVolatile.Sign() < 0 {
		return p, ErrOverflow
	}

	// Both idle words or neither: a half-decoded pair would misplace a leg.
	var idleStableU, idleVolatileU *uint256.Int
	if rd.idleStable != nil && rd.idleVolatile != nil &&
		rd.idleStable.Sign() >= 0 && rd.idleVolatile.Sign() >= 0 {
		idleStableU, _ = uint256.FromBig(rd.idleStable)
		idleVolatileU, _ = uint256.FromBig(rd.idleVolatile)
	}

	var midFee, dirSkew, invK, invBand, floorS, floorV, curv, lpFee *uint256.Int
	var outFee, sigmaRef, volBeta, volMin, volMax *uint256.Int
	liveFeeHook := hookOrEmpty(rd.feeHook)
	hookMoved := !strings.EqualFold(staticExtraFeeHook, liveFeeHook)
	if hookMoved {
		// The terms above were read from the old hook, so this refresh prices without
		// them; re-pin so the next one reads the live hook instead of staying degraded.
		var se StaticExtra
		if json.Unmarshal([]byte(p.StaticExtra), &se) == nil {
			se.FeeHook = liveFeeHook
			if b, err := json.Marshal(se); err == nil {
				p.StaticExtra = string(b)
			}
		}
	}
	feeTermsDecoded := nonNegativeWords(rd.midFee, rd.dirSkew, rd.invSkewKappa,
		rd.invSkewBand, rd.curvature, rd.lpFee, rd.outFee, rd.sigmaRef,
		rd.volBeta, rd.volMin, rd.volMax)
	if !hookMoved && feeTermsDecoded {
		midFee, _ = uint256.FromBig(rd.midFee)
		dirSkew, _ = uint256.FromBig(rd.dirSkew)
		invK, _ = uint256.FromBig(rd.invSkewKappa)
		invBand, _ = uint256.FromBig(rd.invSkewBand)
		if rd.hotFloorsExact && nonNegativeWords(rd.floorStableIn, rd.floorVolatileIn) {
			floorS, _ = uint256.FromBig(rd.floorStableIn)
			floorV, _ = uint256.FromBig(rd.floorVolatileIn)
		}
		curv, _ = uint256.FromBig(rd.curvature)
		lpFee, _ = uint256.FromBig(rd.lpFee)
		outFee, _ = uint256.FromBig(rd.outFee)
		sigmaRef, _ = uint256.FromBig(rd.sigmaRef)
		volBeta, _ = uint256.FromBig(rd.volBeta)
		volMin, _ = uint256.FromBig(rd.volMin)
		volMax, _ = uint256.FromBig(rd.volMax)
	}
	var realizedVariance *uint256.Int
	if rd.realizedVariance != nil && rd.realizedVariance.Sign() >= 0 {
		realizedVariance, _ = uint256.FromBig(rd.realizedVariance)
	}

	extraBytes, err := json.Marshal(Extra{
		Support:             support,
		XWad:                xWadU,
		AnchorSqrtX96:       anchorU,
		Kappa:               kappaU,
		FeeStableInWad:      feeStableInU,
		FeeVolatileInWad:    feeVolatileInU,
		FeeHookActive:       rd.feeHook != (common.Address{}),
		Paused:              rd.paused,
		IdleStable:          idleStableU,
		IdleVolatile:        idleVolatileU,
		MidFeeWad:           midFee,
		DirSkewWad:          dirSkew,
		InvSkewKappaWad:     invK,
		InvSkewBandWad:      invBand,
		ReservationPriceWad: resvP,
		FloorStableInWad:    floorS,
		FloorVolatileInWad:  floorV,
		HotFloorsExact:      rd.hotFloorsExact && floorS != nil && floorV != nil,
		CurvatureWad:        curv,
		LpFeeWad:            lpFee,
		OutFeeWad:           outFee,
		VolSigmaRefWad:      sigmaRef,
		VolBetaWad:          volBeta,
		VolMinWad:           volMin,
		VolMaxWad:           volMax,
		RealizedVarianceWad: realizedVariance,
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

func nonNegativeWords(values ...*big.Int) bool {
	for _, value := range values {
		if value == nil || value.Sign() < 0 || value.BitLen() > 256 {
			return false
		}
	}
	return true
}
