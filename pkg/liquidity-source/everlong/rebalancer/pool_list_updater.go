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

	hasInitialized bool
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
func (u *PoolsListUpdater) GetNewPools(ctx context.Context, _ []byte) ([]entity.Pool, []byte, error) {
	if u.hasInitialized {
		return nil, nil, nil
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

	// Venue pointers the tracker gates on: BorrowerOperations' CORE (its CCR gates every
	// CDP adjustment) and the ALM mint allowlist (the swapper must stay on it). Best
	// effort — a build without a getter leaves that gate unmodelled, not the venue
	// unlisted.
	var core, mintAllowlist gethcommon.Address
	{
		gateReq := u.ethrpcClient.NewRequest().SetContext(ctx)
		if borrowerOperations != (gethcommon.Address{}) {
			gateReq.AddCall(&ethrpc.Call{
				ABI:    borrowerOperationsABI,
				Target: hexutil.Encode(borrowerOperations[:]),
				Method: boMethodCore,
			}, []any{&core})
		}
		gateReq.AddCall(&ethrpc.Call{
			ABI:    almABI,
			Target: hexutil.Encode(alm[:]),
			Method: almMethodMintAllowlist,
		}, []any{&mintAllowlist})
		if _, err := gateReq.TryAggregate(); err != nil {
			core, mintAllowlist = gethcommon.Address{}, gethcommon.Address{}
		}
	}

	// The swapper flash-mints the debt token on every fill and reverts NonZeroFlashFee
	// unless it is fee-exempt — an exemption keyed by msg.sender on the token, so it is
	// only observable from the swapper's own vantage point (a direct call with
	// from=swapper; through a multicall the token would answer for the multicall).
	// Listing is where that probe fits; a venue that lost the exemption is not listed.
	if err := u.verifyFlashFeeExempt(ctx, swapper); err != nil {
		return nil, nil, err
	}

	// A partial deleverage re-derives its gross against this library, so an unset or
	// wrong address does not degrade the venue — it reverts the fill. Listing a routable
	// pool on that is worse than not listing it, so prove the address answers now.
	if err := u.verifyMath(ctx, curveParams); err != nil {
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
		DebtGasCompensation: gasCompensation,
		ManagedVault:        hexutil.Encode(managedVault[:]),
	})
	if err != nil {
		return nil, nil, err
	}

	u.hasInitialized = true

	return []entity.Pool{
		{
			Address:   hexutil.Encode(swapper[:]),
			Exchange:  u.config.DexID,
			Type:      DexType,
			Timestamp: time.Now().Unix(),
			Tokens: []*entity.PoolToken{
				{Address: strings.ToLower(u.config.Stable), Swappable: true},
				{Address: strings.ToLower(u.config.Volatile), Swappable: true},
			},
			Reserves:    entity.PoolReserves{"0", "0"},
			StaticExtra: string(staticExtra),
			Extra:       "{}",
			BlockNumber: blockNumber,
		},
	}, nil, nil
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
// answers deleverageQuote through the expected ABI. A bare code-length check would pass
// any contract; probing the function proves the shape the executor depends on.
func (u *PoolsListUpdater) verifyMath(ctx context.Context, cp CurveParams) error {
	if !gethcommon.IsHexAddress(u.config.Math) {
		return ErrMathNotConfigured
	}
	math := gethcommon.HexToAddress(u.config.Math)
	if math == (gethcommon.Address{}) {
		return ErrMathNotConfigured
	}
	var out struct {
		CollateralOut *big.Int
		NewColl       *big.Int
		NewDebt       *big.Int
	}
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    mathABI,
		Target: math.Hex(),
		Method: mathMethodDeleverageQuote,
		// A probe on a tiny lot: the point is that the call decodes, not what it returns.
		Params: []any{big.NewInt(1), big.NewInt(1), bigWad, cp.LeverageRatioWad, big.NewInt(0), big.NewInt(1)},
	}, []any{&out})
	if _, err := req.Aggregate(); err != nil {
		return ErrMathNotConfigured
	}
	if out.CollateralOut == nil {
		return ErrMathNotConfigured
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
