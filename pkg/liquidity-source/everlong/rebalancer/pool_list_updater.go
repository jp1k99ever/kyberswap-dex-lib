package everlongrebalancer

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/KyberNetwork/ethrpc"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/goccy/go-json"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	poollist "github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool/list"
)

type PoolsListUpdater struct {
	config       *Config
	ethrpcClient *ethrpc.Client
}

// Metadata is what the updater carries between runs. The settlement swapper IS the pool
// address, and the rebalancer can rotate it: listing keys off the one it last emitted so
// a rotation produces a replacement pool instead of nothing (the tracker refuses the old
// one from the same refresh onward).
type Metadata struct {
	Swapper string `json:"swapper"`
	// The managed vault whose position the venue trades. exchangeState() follows a
	// setManagedVault rotation immediately, while every CDP word the tracker reads is
	// keyed by the vault resolved here — so a rotation has to relist, not just refresh.
	ManagedVault string `json:"managedVault"`
	// The UUPS implementation whose linked CollRebalancerMath was attested. A proxy
	// upgrade changes the executable math even when the swapper and managed vault stay
	// put, so it must produce a replacement pool with freshly-attested StaticExtra.
	Implementation string `json:"implementation"`
}

var _ = poollist.RegisterFactoryCE(DexType, NewPoolsListUpdater)

func NewPoolsListUpdater(cfg *Config, ethrpcClient *ethrpc.Client) *PoolsListUpdater {
	return &PoolsListUpdater{
		config:       cfg,
		ethrpcClient: ethrpcClient,
	}
}

// GetNewPools lists the single configured CollVault venue, resolving the contract graph
// (collVault, settlement swapper, ALM adapter, asset decimals) on-chain from the
// rebalancer so only the rebalancer address and the two swap legs are configuration.
func (u *PoolsListUpdater) GetNewPools(ctx context.Context, metadataBytes []byte) ([]entity.Pool, []byte, error) {
	var metadata Metadata
	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			return nil, nil, err
		}
	}

	curveParams, err := u.resolveCurveParams()
	if err != nil {
		return nil, nil, err
	}

	var (
		collVault, swapper, positionManager, managedVault gethcommon.Address
	)
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: u.config.Rebalancer,
		Method: rebalancerMethodCollVault,
	}, []any{&collVault}).AddCall(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: u.config.Rebalancer,
		Method: rebalancerMethodSettlementSwapper,
	}, []any{&swapper}).AddCall(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: u.config.Rebalancer,
		Method: rebalancerMethodPositionManager,
	}, []any{&positionManager}).AddCall(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: u.config.Rebalancer,
		Method: rebalancerMethodManagedVault,
	}, []any{&managedVault})
	if _, err := req.Aggregate(); err != nil {
		return nil, nil, err
	}

	var (
		alm                gethcommon.Address
		borrowerOperations gethcommon.Address
		gasCompensation    = new(big.Int)
		assetDecimals      uint8
		blockNumber        uint64
	)
	req = u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    swapperABI,
		Target: hexutil.Encode(swapper[:]),
		Method: swapperMethodAlm,
	}, []any{&alm}).AddCall(&ethrpc.Call{
		ABI:    collVaultABI,
		Target: hexutil.Encode(collVault[:]),
		Method: cvMethodAssetDecimals,
	}, []any{&assetDecimals}).AddCall(&ethrpc.Call{
		ABI:    positionManagerABI,
		Target: hexutil.Encode(positionManager[:]),
		Method: pmMethodBorrowerOperations,
	}, []any{&borrowerOperations}).AddCall(&ethrpc.Call{
		ABI:    positionManagerABI,
		Target: hexutil.Encode(positionManager[:]),
		Method: pmMethodDebtGasCompensation,
	}, []any{&gasCompensation})
	resp, err := req.Aggregate()
	if err != nil {
		return nil, nil, err
	}
	if resp.BlockNumber != nil {
		blockNumber = resp.BlockNumber.Uint64()
	}

	underlyingCvamm, err := u.resolveUnderlyingCvamm(ctx, alm)
	if err != nil {
		return nil, nil, err
	}

	// Venue pointers the tracker gates on: BorrowerOperations' CORE (whose CCR and pause
	// gate every adjustment) and the ALM's mint allowlist. Both getters exist on the
	// deployment this integrates, so a read that does not answer is a deployment this
	// code has not been verified against — listing fails rather than quietly dropping
	// the gate. A successful ZERO allowlist is different: the ALM documents address(0)
	// as "open", and is recorded as such.
	var core, mintAllowlist gethcommon.Address
	{
		gateReq := u.ethrpcClient.NewRequest().SetContext(ctx)
		gateReq.AddCall(&ethrpc.Call{
			ABI:    borrowerOperationsABI,
			Target: hexutil.Encode(borrowerOperations[:]),
			Method: boMethodCore,
		}, []any{&core})
		gateReq.AddCall(&ethrpc.Call{
			ABI:    almABI,
			Target: hexutil.Encode(alm[:]),
			Method: almMethodMintAllowlist,
		}, []any{&mintAllowlist})
		if _, err := gateReq.Aggregate(); err != nil {
			return nil, nil, ErrGateDiscovery
		}
		if core == (gethcommon.Address{}) {
			return nil, nil, ErrGateDiscovery
		}
	}

	// The rebalancer proxy's implementation: the CollRebalancerMath the executor
	// re-derives against is LINKED into this code, so pinning it is what makes the math
	// attestation below survive an upgrade.
	implementation, err := u.resolveImplementation(ctx)
	if err != nil {
		return nil, nil, err
	}

	// The swapper flash-mints the debt token on every fill and reverts NonZeroFlashFee
	// unless it is fee-exempt — an exemption keyed by msg.sender on the token, so it is
	// only observable from the swapper's own vantage point (a direct call with
	// from=swapper; through a multicall the token would answer for the multicall).
	// Listing is where that probe fits; a venue that lost the exemption is not listed.
	if err := u.verifyFlashFeeExempt(ctx, swapper); err != nil {
		return nil, nil, err
	}

	// Nothing rotates more quietly than the settlement swapper, and it IS the pool
	// address. The managed vault changes the state key and the implementation changes the
	// linked pricing code, so either one must emit a freshly-attested replacement too.
	if metadata.matches(swapper, managedVault, implementation) {
		return nil, metadataBytes, nil
	}

	// The token pair is DERIVED from the deployed swapper, never trusted from config:
	// the pair a pool advertises has to be the pair its settlement contract actually
	// moves. Config, when set, is a cross-check that must agree.
	stable, volatile, err := u.verifySwapperIdentity(ctx, swapper, collVault)
	if err != nil {
		return nil, nil, err
	}

	// A partial deleverage re-derives its gross against this library, so a wrong address
	// does not degrade the venue — it reverts the fill. Prove the deployed library
	// reproduces the local model at the live state before listing anything on it.
	if err := u.verifyMath(ctx, curveParams); err != nil {
		return nil, nil, err
	}
	if err := u.verifyMathLink(ctx, implementation); err != nil {
		return nil, nil, err
	}

	staticExtra, err := json.Marshal(StaticExtra{
		Rebalancer:          strings.ToLower(u.config.Rebalancer),
		Swapper:             hexutil.Encode(swapper[:]),
		CollVault:           hexutil.Encode(collVault[:]),
		ALM:                 hexutil.Encode(alm[:]),
		UnderlyingCvamm:     underlyingCvamm,
		CvDecimalsOffset:    18 - assetDecimals,
		Math:                strings.ToLower(u.config.Math),
		CurveParams:         curveParams,
		GasLeverage:         u.config.GasLeverage,
		GasDeleverage:       u.config.GasDeleverage,
		PositionManager:     hexutil.Encode(positionManager[:]),
		BorrowerOperations:  hexutil.Encode(borrowerOperations[:]),
		Core:                addrOrEmpty(core),
		MintAllowlist:       addrOrEmpty(mintAllowlist),
		Implementation:      hexutil.Encode(implementation[:]),
		StableToken:         stable,
		DebtGasCompensation: gasCompensation,
		ManagedVault:        hexutil.Encode(managedVault[:]),
	})
	if err != nil {
		return nil, nil, err
	}

	newMetadata, err := json.Marshal(Metadata{
		Swapper:        hexutil.Encode(swapper[:]),
		ManagedVault:   hexutil.Encode(managedVault[:]),
		Implementation: hexutil.Encode(implementation[:]),
	})
	if err != nil {
		return nil, nil, err
	}

	return []entity.Pool{
		{
			Address:   hexutil.Encode(swapper[:]),
			Exchange:  u.config.DexID,
			Type:      DexType,
			Timestamp: time.Now().Unix(),
			Tokens: []*entity.PoolToken{
				{Address: stable, Swappable: true},
				{Address: volatile, Swappable: true},
			},
			Reserves:    entity.PoolReserves{"0", "0"},
			StaticExtra: string(staticExtra),
			Extra:       "{}",
			BlockNumber: blockNumber,
		},
	}, newMetadata, nil
}

func (m Metadata) matches(swapper, managedVault, implementation gethcommon.Address) bool {
	return strings.EqualFold(m.Swapper, hexutil.Encode(swapper[:])) &&
		strings.EqualFold(m.ManagedVault, hexutil.Encode(managedVault[:])) &&
		strings.EqualFold(m.Implementation, hexutil.Encode(implementation[:]))
}

// verifySwapperIdentity derives the pair from the deployed swapper and proves the
// swapper belongs to the configured rebalancer. Everything here is an assertion about
// the deployment, not a preference: a swapper whose core() is some other exchange, or
// whose collVault() is not the one the rebalancer reported, is a different venue wearing
// this pool's address.
func (u *PoolsListUpdater) verifySwapperIdentity(ctx context.Context, swapper, collVault gethcommon.Address) (
	stable, volatile string, err error) {
	var (
		swapperCore, swapperCollVault, debtToken, volatileToken gethcommon.Address
	)
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	for _, c := range []struct {
		method string
		out    *gethcommon.Address
	}{
		{swapperMethodCore, &swapperCore},
		{swapperMethodCollVault, &swapperCollVault},
		{swapperMethodDebtToken, &debtToken},
		{swapperMethodVolatile, &volatileToken},
	} {
		req.AddCall(&ethrpc.Call{ABI: swapperABI, Target: hexutil.Encode(swapper[:]), Method: c.method}, []any{c.out})
	}
	if _, err := req.Aggregate(); err != nil {
		return "", "", ErrSwapperIdentity
	}
	if !strings.EqualFold(swapperCore.Hex(), u.config.Rebalancer) ||
		swapperCollVault != collVault ||
		debtToken == (gethcommon.Address{}) || volatileToken == (gethcommon.Address{}) ||
		debtToken == volatileToken {
		return "", "", ErrSwapperIdentity
	}
	stable, volatile = hexutil.Encode(debtToken[:]), hexutil.Encode(volatileToken[:])
	// Config, when supplied, must agree with the chain rather than override it.
	if u.config.Stable != "" && !strings.EqualFold(u.config.Stable, stable) {
		return "", "", ErrSwapperIdentity
	}
	if u.config.Volatile != "" && !strings.EqualFold(u.config.Volatile, volatile) {
		return "", "", ErrSwapperIdentity
	}
	return stable, volatile, nil
}

func (u *PoolsListUpdater) resolveCurveParams() (CurveParams, error) {
	if u.config.CurveParams != nil {
		cp := *u.config.CurveParams
		if cp.LeverageRatioWad == nil || cp.HZero == nil || cp.HJoin == nil || cp.HWall == nil ||
			cp.Width == nil || cp.DJoin == nil || cp.DWall == nil || cp.RescueSpreadPpm == nil ||
			cp.PhysicalCrFloorWad == nil {
			return CurveParams{}, ErrInvalidCurveParams
		}
		for _, v := range cp.BezierPhi {
			if v == nil {
				return CurveParams{}, ErrInvalidCurveParams
			}
		}
		for _, v := range cp.BezierIntegral {
			if v == nil {
				return CurveParams{}, ErrInvalidCurveParams
			}
		}
		return cp, nil
	}
	builtin, ok := curveParamsByChain[u.config.ChainID]
	if !ok {
		return CurveParams{}, ErrNoCurveParams
	}
	return builtin(), nil
}

// verifyMath fails listing unless config.Math is a non-zero address whose deployed code
// REPRODUCES the local model. The executor re-derives a partial deleverage against this
// library on-chain while the router quotes with the Go port, so the two agreeing is the
// premise of the whole integration — a probe that only proves the call decodes would
// pass any contract carrying the right function signature, including a differently
// parameterised deployment of the same library.
//
// The check runs at the venue's LIVE state across several nondegenerate lot sizes (a
// degenerate lot answers zero on both sides and proves nothing) and every one must match
// to the wei. The tracker re-runs a single-point version each refresh, so a library
// swapped under an already-listed pool is caught too.
func (u *PoolsListUpdater) verifyMath(ctx context.Context, cp CurveParams) error {
	if !gethcommon.IsHexAddress(u.config.Math) {
		return ErrMathNotConfigured
	}
	math := gethcommon.HexToAddress(u.config.Math)
	if math == (gethcommon.Address{}) {
		return ErrMathNotConfigured
	}

	var st exchangeStateRaw
	stReq := u.ethrpcClient.NewRequest().SetContext(ctx)
	stReq.AddCall(&ethrpc.Call{ABI: rebalancerABI, Target: u.config.Rebalancer,
		Method: rebalancerMethodExchangeState}, []any{&st})
	if _, err := stReq.Aggregate(); err != nil {
		return ErrMathNotConfigured
	}
	if st.Collateral == nil || st.Debt == nil || st.PriceWad == nil || st.SpreadPpm == nil ||
		st.Debt.Sign() == 0 {
		return ErrMathNotConfigured
	}

	// Lots spread across the book: small enough that the venue still quotes them, large
	// enough not to round to nothing.
	probes := make([]*big.Int, 0, 4)
	for _, denom := range []int64{64, 16, 4, 2} {
		probes = append(probes, new(big.Int).Div(st.Debt, big.NewInt(denom)))
	}
	outs := make([]struct {
		CollateralOut *big.Int
		NewColl       *big.Int
		NewDebt       *big.Int
	}, len(probes))
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	for i, p := range probes {
		req.AddCall(&ethrpc.Call{
			ABI:    mathABI,
			Target: math.Hex(),
			Method: mathMethodDeleverageQuote,
			Params: []any{st.Collateral, st.Debt, st.PriceWad, cp.LeverageRatioWad, st.SpreadPpm, p},
		}, []any{&outs[i]})
	}
	if _, err := req.Aggregate(); err != nil {
		return ErrMathNotConfigured
	}

	var compared int
	for i, p := range probes {
		if p.Sign() == 0 || outs[i].CollateralOut == nil {
			return ErrMathNotConfigured
		}
		wantOut, wantColl, wantDebt := cp.deleverageQuote(st.Collateral, st.Debt, st.PriceWad,
			cp.LeverageRatioWad, st.SpreadPpm, p)
		if outs[i].CollateralOut.Sign() == 0 && wantOut.Sign() == 0 {
			continue // the venue refuses this lot and the model agrees it is not a fill
		}
		if outs[i].CollateralOut.Cmp(wantOut) != 0 ||
			outs[i].NewColl.Cmp(wantColl) != 0 || outs[i].NewDebt.Cmp(wantDebt) != 0 {
			return ErrMathMismatch
		}
		compared++
	}
	if compared == 0 {
		return ErrMathNotConfigured // nothing nondegenerate answered: the link is unproven
	}
	return nil
}

// verifyMathLink proves identity, while verifyMath above proves behavior. New
// implementations expose their link directly through mathLibrary(); the deployed legacy
// implementation predates that getter, so its Solidity PUSH20 relocation is recovered
// from executable bytecode instead. Either route prevents a different, merely
// ABI-compatible library from being probed while the rebalancer executes another one.
func (u *PoolsListUpdater) verifyMathLink(ctx context.Context, implementation gethcommon.Address) error {
	math := gethcommon.HexToAddress(u.config.Math)
	if math == (gethcommon.Address{}) {
		return ErrMathNotConfigured
	}

	var reported gethcommon.Address
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    rebalancerABI,
		Target: hexutil.Encode(implementation[:]),
		Method: rebalancerMethodMathLibrary,
	}, []any{&reported})
	if _, err := req.Call(); err == nil {
		if reported != math {
			return ErrMathNotLinked
		}
		return nil
	}

	code, err := u.ethrpcClient.GetETHClient().CodeAt(ctx, implementation, nil)
	if err != nil {
		return err
	}
	if !runtimeLinksLibrary(code, math) {
		return ErrMathNotLinked
	}
	return nil
}

// resolveUnderlyingCvamm reads the CvammALM the ALM adapter wraps via its public alm()
// getter. REQUIRED: an unresolved underlying would silently restore the uncoupled
// (double-counting) simulator, so listing fails instead.
func (u *PoolsListUpdater) resolveUnderlyingCvamm(ctx context.Context, adapter gethcommon.Address) (string, error) {
	var underlying gethcommon.Address
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    almABI,
		Target: hexutil.Encode(adapter[:]),
		Method: adapterMethodAlm,
	}, []any{&underlying})
	if _, err := req.Call(); err != nil {
		return "", err
	}
	if underlying == (gethcommon.Address{}) {
		return "", ErrUnderlyingCvamm
	}
	return strings.ToLower(hexutil.Encode(underlying[:])), nil
}

// verifyFlashFeeExempt asks the debt token for the flash fee the SWAPPER would pay.
func (u *PoolsListUpdater) verifyFlashFeeExempt(ctx context.Context, swapper gethcommon.Address) error {
	fee := new(big.Int)
	req := u.ethrpcClient.NewRequest().SetContext(ctx).SetFrom(swapper)
	req.AddCall(&ethrpc.Call{
		ABI:    debtTokenABI,
		Target: u.config.Stable,
		Method: debtMethodFlashFee,
		Params: []any{gethcommon.HexToAddress(u.config.Stable), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)},
	}, []any{&fee})
	if _, err := req.Call(); err != nil {
		return err
	}
	if fee.Sign() != 0 {
		return ErrFlashFeeNotExempt
	}
	return nil
}

func addrOrEmpty(a gethcommon.Address) string {
	if a == (gethcommon.Address{}) {
		return ""
	}
	return hexutil.Encode(a[:])
}

// resolveImplementation reads the rebalancer proxy's EIP-1967 implementation slot. It is
// the identity of the code that links CollRebalancerMath, so the tracker can detect an
// upgrade — the one event that can change the math under an already-listed pool — by
// re-reading one storage word instead of re-running the whole attestation.
func (u *PoolsListUpdater) resolveImplementation(ctx context.Context) (gethcommon.Address, error) {
	word, err := u.ethrpcClient.GetETHClient().StorageAt(ctx,
		gethcommon.HexToAddress(u.config.Rebalancer), eip1967ImplSlot, nil)
	if err != nil {
		return gethcommon.Address{}, err
	}
	impl := gethcommon.BytesToAddress(word)
	if impl == (gethcommon.Address{}) {
		return gethcommon.Address{}, ErrGateDiscovery
	}
	return impl, nil
}
