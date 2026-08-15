package everlongcollvault

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum"
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

	staticExtra, err := json.Marshal(StaticExtra{
		Rebalancer:          strings.ToLower(u.config.Rebalancer),
		Swapper:             hexutil.Encode(swapper[:]),
		CollVault:           hexutil.Encode(collVault[:]),
		ALM:                 hexutil.Encode(alm[:]),
		UnderlyingCvamm:     u.resolveUnderlyingCvamm(ctx, alm),
		CvDecimalsOffset:    18 - assetDecimals,
		CurveParams:         curveParams,
		GasLeverage:         u.config.GasLeverage,
		GasDeleverage:       u.config.GasDeleverage,
		PositionManager:     hexutil.Encode(positionManager[:]),
		BorrowerOperations:  hexutil.Encode(borrowerOperations[:]),
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

// resolveUnderlyingCvamm identifies the CvammALM the swapper's ALM adapter wraps, for
// meta/base coupling with the everlong-cvamm source. The adapter exposes no accessor for
// it but carries it as an immutable, so: scan the runtime code for PUSH20/PUSH32 address
// operands and keep the single candidate that answers BOTH xWad() and kappa() — the
// CvammALM read surface (the token/hook immutables answer neither). Best-effort: an
// empty result (RPC failure, no candidate, ambiguity) leaves the pools uncoupled, which
// is exactly the pre-coupling behavior.
func (u *PoolsListUpdater) resolveUnderlyingCvamm(ctx context.Context, adapter gethcommon.Address) string {
	eth := u.ethrpcClient.GetETHClient()
	code, err := eth.CodeAt(ctx, adapter, nil)
	if err != nil || len(code) == 0 {
		return ""
	}
	candidates := map[gethcommon.Address]struct{}{}
	for i := 0; i < len(code); {
		op := code[i]
		if op == 0x73 && i+21 <= len(code) { // PUSH20
			candidates[gethcommon.BytesToAddress(code[i+1:i+21])] = struct{}{}
		} else if op == 0x7f && i+33 <= len(code) { // PUSH32 with an address-shaped operand
			w := code[i+1 : i+33]
			allZero, anySet := true, false
			for _, b := range w[:12] {
				if b != 0 {
					allZero = false
					break
				}
			}
			for _, b := range w[12:] {
				if b != 0 {
					anySet = true
					break
				}
			}
			if allZero && anySet {
				candidates[gethcommon.BytesToAddress(w[12:])] = struct{}{}
			}
		}
		if 0x60 <= op && op <= 0x7f {
			i += int(op) - 0x5f + 1
		} else {
			i++
		}
	}
	var (
		xWadSelector  = []byte{0x6e, 0x95, 0x42, 0x66} // xWad()
		kappaSelector = []byte{0x6b, 0xba, 0x3f, 0x2f} // kappa()
		match         gethcommon.Address
		found         bool
	)
	for cand := range candidates {
		cand := cand
		xw, err1 := eth.CallContract(ctx, ethereum.CallMsg{To: &cand, Data: xWadSelector}, nil)
		if err1 != nil || len(xw) != 32 {
			continue
		}
		kp, err2 := eth.CallContract(ctx, ethereum.CallMsg{To: &cand, Data: kappaSelector}, nil)
		if err2 != nil || len(kp) != 32 {
			continue
		}
		if found {
			return "" // ambiguous — refuse to guess
		}
		match, found = cand, true
	}
	if !found {
		return ""
	}
	return strings.ToLower(match.Hex())
}
