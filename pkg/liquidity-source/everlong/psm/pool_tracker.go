package everlongpsm

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

var _ = pooltrack.RegisterFactoryCE0(DexType, NewPoolTracker)

func NewPoolTracker(cfg *Config, ethrpcClient *ethrpc.Client) *PoolTracker {
	return &PoolTracker{
		config:       cfg,
		ethrpcClient: ethrpcClient,
	}
}

// GetNewPoolState refreshes the snapshot in one block-pinned multicall. Rates come from
// the PSM's own feeBpFor, not the fee hook: the hook returns a policy struct and the
// arithmetic lives in the PSM's library, so feeBpFor avoids porting the fee law.
func (t *PoolTracker) GetNewPoolState(ctx context.Context, p entity.Pool,
	_ pool.GetNewPoolStateParams) (entity.Pool, error) {
	return t.getNewPoolState(ctx, p, nil)
}

func (t *PoolTracker) GetNewPoolStateWithOverrides(ctx context.Context, p entity.Pool,
	params pool.GetNewPoolStateWithOverridesParams) (entity.Pool, error) {
	return t.getNewPoolState(ctx, p, params.Overrides)
}

func (t *PoolTracker) getNewPoolState(ctx context.Context, p entity.Pool,
	overrides map[common.Address]gethclient.OverrideAccount) (entity.Pool, error) {
	var staticExtra StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &staticExtra); err != nil {
		return p, err
	}
	if len(p.Tokens) != 2 {
		return p, ErrInvalidSnapshot
	}
	stable := common.HexToAddress(p.Tokens[1].Address)
	feeCaller := common.HexToAddress(strings.ToLower(t.config.FeeCaller))

	var (
		paused       bool
		metaPaused   bool
		capHook      common.Address
		wadOffset    uint64
		entryFee     = new(big.Int)
		exitFee      = new(big.Int)
		availMint    = new(big.Int)
		availReserve = new(big.Int)
		minted       = new(big.Int)
	)
	req := t.ethrpcClient.NewRequest().SetContext(ctx)
	if overrides != nil {
		req.SetOverrides(overrides)
	}
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodPaused},
		[]any{&paused})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodCapHook},
		[]any{&capHook})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodStables,
		Params: []any{stable}}, []any{&wadOffset})
	// Tolerated: the PSM reverts rather than return an unusable rate, so a failed leg
	// means that direction is closed on-chain — carried as a nil rate.
	const entryFeeIdx, exitFeeIdx = 3, 4
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodFeeBpFor,
		Params: []any{feeCaller, stable, true}}, []any{&entryFee})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodFeeBpFor,
		Params: []any{feeCaller, stable, false}}, []any{&exitFee})
	// Hook-aware capacity: both views fold the cap and yield hooks.
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodAvailableMint,
		Params: []any{stable}}, []any{&availMint})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodAvailableReserve,
		Params: []any{stable}}, []any{&availReserve})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodDebtTokenMinted,
		Params: []any{stable}}, []any{&minted})
	// The PSM's own `notPaused` is `paused || metaCore.paused()`: a protocol-wide pause
	// closes every PSM without touching its local flag. Same selector, other target.
	hasMetaCore := staticExtra.MetaCore != "" && common.HexToAddress(staticExtra.MetaCore) != (common.Address{})
	if hasMetaCore {
		req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.MetaCore, Method: psmMethodPaused},
			[]any{&metaPaused})
	}

	// TryBlockAndAggregate, not TryAggregate: the latter returns no block number, which
	// silently left the cap-hook round below unpinned and reading `latest`.
	resp, err := req.TryBlockAndAggregate()
	if err != nil {
		return p, err
	}
	for i, ok := range resp.Result {
		if !ok && i != entryFeeIdx && i != exitFeeIdx {
			return p, ErrInvalidSnapshot
		}
	}
	entryBp, exitBp := entryFee, exitFee
	if !resp.Result[entryFeeIdx] {
		entryBp = nil
	}
	if !resp.Result[exitFeeIdx] {
		exitBp = nil
	}

	// De-whitelisted mid-flight: both directions revert NotListedToken on-chain.
	if wadOffset == 0 || metaPaused {
		paused = true
	}

	// Exit-side hook ceiling at the same block; skipped when no cap hook is set.
	var maxRedeem, maxOutflow *big.Int
	if capHook != (common.Address{}) {
		hookReq := t.ethrpcClient.NewRequest().SetContext(ctx)
		if overrides != nil {
			hookReq.SetOverrides(overrides)
		}
		if resp.BlockNumber != nil {
			hookReq.SetBlockNumber(resp.BlockNumber)
		}
		burnCeiling, outflowCeiling := new(big.Int), new(big.Int)
		hookReq.AddCall(&ethrpc.Call{ABI: capHookABI, Target: capHook.Hex(),
			Method: capHookMethodMaxRedeem, Params: []any{stable}}, []any{&burnCeiling})
		hookReq.AddCall(&ethrpc.Call{ABI: capHookABI, Target: capHook.Hex(),
			Method: capHookMethodMaxOutflow,
			Params: []any{feeCaller, stable, false}}, []any{&outflowCeiling})
		if _, err := hookReq.Aggregate(); err != nil {
			return p, err
		}
		maxRedeem, maxOutflow = burnCeiling, outflowCeiling
	}

	for _, v := range []*big.Int{availMint, availReserve, minted, entryBp, exitBp, maxRedeem, maxOutflow} {
		if v != nil && v.Sign() < 0 {
			return p, ErrInvalidSnapshot
		}
	}

	extraBytes, err := json.Marshal(Extra{
		Paused:           paused,
		EntryFeeBp:       entryBp,
		ExitFeeBp:        exitBp,
		AvailableMint:    availMint,
		DebtTokenMinted:  minted,
		MaxRedeem:        maxRedeem,
		MaxOutflow:       maxOutflow,
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
	if resp.BlockNumber != nil {
		p.BlockNumber = resp.BlockNumber.Uint64()
	}
	return p, nil
}
