package everlongpsm

import (
	"context"
	"math/big"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
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

var _ = pooltrack.RegisterFactoryCE0(DexType, NewPoolTracker)

func NewPoolTracker(cfg *Config, ethrpcClient *ethrpc.Client) *PoolTracker {
	return &PoolTracker{
		config:       cfg,
		ethrpcClient: ethrpcClient,
	}
}

// GetNewPoolState refreshes the snapshot in one block-pinned multicall plus one
// same-block eth_getCode. Rates come from feeBpFor for the exact execution caller. The
// reviewed flat-hook runtime is book-independent, which is what makes those sampled
// rates exact across route-local UpdateBalance calls.
func (t *PoolTracker) GetNewPoolState(ctx context.Context, p entity.Pool,
	_ pool.GetNewPoolStateParams) (entity.Pool, error) {
	return t.getNewPoolState(ctx, p, nil)
}

func (t *PoolTracker) GetNewPoolStateWithOverrides(ctx context.Context, p entity.Pool,
	params pool.GetNewPoolStateWithOverridesParams) (entity.Pool, error) {
	if len(params.Overrides) != 0 {
		return p, ErrStateOverridesUnsupported
	}
	return t.getNewPoolState(ctx, p, nil)
}

func (t *PoolTracker) getNewPoolState(ctx context.Context, p entity.Pool,
	overrides map[common.Address]gethclient.OverrideAccount) (entity.Pool, error) {
	var staticExtra StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &staticExtra); err != nil {
		return p, err
	}
	if err := staticExtra.validateProductionProfile(); err != nil {
		return p, err
	}
	psm, stable, feeCaller, err := configuredAddresses(t.config)
	if err != nil {
		return p, err
	}
	if len(p.Tokens) != 2 || !common.IsHexAddress(p.Tokens[0].Address) ||
		!common.IsHexAddress(p.Tokens[1].Address) ||
		common.HexToAddress(p.Tokens[0].Address) != common.HexToAddress(staticExtra.DebtToken) ||
		common.HexToAddress(p.Tokens[1].Address) != stable ||
		common.HexToAddress(staticExtra.PSM) != psm || common.HexToAddress(staticExtra.Stable) != stable ||
		common.HexToAddress(staticExtra.FeeCaller) != feeCaller {
		return p, ErrProfileChanged
	}
	if overrides != nil {
		// Kept defensive for direct internal callers; the public override method rejects
		// nonempty maps before reaching the raw same-block code probe below.
		return p, ErrInvalidSnapshot
	}

	var (
		debtToken, metaCore, feeHook, capHook, yieldHook, listedStable common.Address
		paused, metaPaused, psmBonded                                  bool
		listedStablesLength                                            = new(big.Int)
		wadOffset                                                      uint64
		entryFee, exitFee                                              = new(big.Int), new(big.Int)
		availMint, availReserve, minted                                = new(big.Int), new(big.Int), new(big.Int)
	)
	req := t.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodDebtToken},
		[]any{&debtToken})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodMetaCore},
		[]any{&metaCore})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodFeeHook},
		[]any{&feeHook})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodCapHook},
		[]any{&capHook})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodYieldHook},
		[]any{&yieldHook})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodListedLength},
		[]any{&listedStablesLength})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodListedStables,
		Params: []any{big.NewInt(0)}}, []any{&listedStable})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodPaused},
		[]any{&paused})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodStables,
		Params: []any{stable}}, []any{&wadOffset})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodFeeBpFor,
		Params: []any{feeCaller, stable, true}}, []any{&entryFee})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodFeeBpFor,
		Params: []any{feeCaller, stable, false}}, []any{&exitFee})
	// With capHook/yieldHook attested zero these are structural mint room and idle
	// reserve. Their route-local deltas are therefore exactly replayable.
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodAvailableMint,
		Params: []any{stable}}, []any{&availMint})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodAvailableReserve,
		Params: []any{stable}}, []any{&availReserve})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodDebtTokenMinted,
		Params: []any{stable}}, []any{&minted})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.MetaCore, Method: psmMethodPaused},
		[]any{&metaPaused})
	req.AddCall(&ethrpc.Call{ABI: debtTokenABI, Target: staticExtra.DebtToken,
		Method: debtTokenMethodPSMBonds, Params: []any{psm}}, []any{&psmBonded})

	// TryBlockAndAggregate exposes individual decode failures and returns the block used
	// by every leg. No call is optional under the supported profile.
	resp, err := req.TryBlockAndAggregate()
	if err != nil {
		return p, err
	}
	if resp.BlockNumber == nil {
		return p, ErrInvalidSnapshot
	}
	for _, ok := range resp.Result {
		if !ok {
			return p, ErrInvalidSnapshot
		}
	}

	psmCodeHash, hookCodeHash, feeCallerCodeHash, err := probeProfileCode(ctx, t.ethrpcClient,
		psm, feeHook, feeCaller,
		resp.BlockNumber)
	if err != nil {
		return p, err
	}
	snapshot := &profileSnapshot{
		PSM: psm, DebtToken: debtToken, MetaCore: metaCore, Stable: stable,
		FeeCaller: feeCaller, FeeHook: feeHook, CapHook: capHook, YieldHook: yieldHook,
		ListedStable: listedStable, ListedStablesLength: listedStablesLength,
		WadOffset: wadOffset, EntryFeeBp: entryFee, ExitFeeBp: exitFee,
		PSMCodeHash: psmCodeHash, FeeHookCodeHash: hookCodeHash, FeeCallerCodeHash: feeCallerCodeHash,
		PSMBonded: psmBonded,
	}
	if err := snapshot.validate(); err != nil {
		return p, err
	}
	if !liveProfileMatchesStatic(snapshot, &staticExtra) {
		return p, ErrProfileChanged
	}

	for _, v := range []*big.Int{availMint, availReserve, minted} {
		if v == nil || v.Sign() < 0 {
			return p, ErrInvalidSnapshot
		}
	}

	extraBytes, err := json.Marshal(Extra{
		Paused:           paused || metaPaused,
		PSMBonded:        psmBonded,
		EntryFeeBp:       entryFee,
		ExitFeeBp:        exitFee,
		AvailableMint:    availMint,
		DebtTokenMinted:  minted,
		AvailableReserve: availReserve,
	})
	if err != nil {
		return p, err
	}

	p.Extra = string(extraBytes)
	// Router-visible liquidity: debt side bounded by the remaining mint room, stable
	// side by what the PSM could actually pay out.
	p.Reserves = entity.PoolReserves{availMint.String(), availReserve.String()}
	p.Timestamp = time.Now().Unix()
	p.BlockNumber = resp.BlockNumber.Uint64()
	return p, nil
}

func liveProfileMatchesStatic(snapshot *profileSnapshot, static *StaticExtra) bool {
	if snapshot == nil || static == nil {
		return false
	}
	return snapshot.PSM == common.HexToAddress(static.PSM) &&
		snapshot.DebtToken == common.HexToAddress(static.DebtToken) &&
		snapshot.MetaCore == common.HexToAddress(static.MetaCore) &&
		snapshot.Stable == common.HexToAddress(static.Stable) &&
		snapshot.FeeCaller == common.HexToAddress(static.FeeCaller) &&
		snapshot.FeeCallerCodeHash == common.HexToHash(static.FeeCallerCodeHash) &&
		snapshot.PSMCodeHash == common.HexToHash(static.PSMCodeHash) &&
		snapshot.FeeHook == common.HexToAddress(static.FeeHook) &&
		snapshot.FeeHookCodeHash == common.HexToHash(static.FeeHookCodeHash) &&
		new(big.Int).SetUint64(snapshot.WadOffset).Cmp(static.WadOffset) == 0 &&
		snapshot.ListedStablesLength.Cmp(big.NewInt(int64(static.ListedStableCount))) == 0 &&
		hexutil.Encode(snapshot.ListedStable[:]) == static.Stable
}
