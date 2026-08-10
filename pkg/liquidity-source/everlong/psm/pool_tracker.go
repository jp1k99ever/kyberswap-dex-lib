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

// GetNewPoolState refreshes the PSM snapshot in one block-pinned multicall. The fee is
// read through the CURRENT feeHook (owner-settable, so resolved every refresh) for the
// configured caller — the shipped hook keys fees per caller with a zero default.
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
	psm := common.HexToAddress(staticExtra.PSM)

	var (
		paused        bool
		feeHook       common.Address
		wadOffset     uint64
		mintCap       = new(big.Int)
		minted        = new(big.Int)
		stableReserve = new(big.Int)
	)
	req := t.ethrpcClient.NewRequest().SetContext(ctx)
	if overrides != nil {
		req.SetOverrides(overrides)
	}
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodPaused},
		[]any{&paused})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodFeeHook},
		[]any{&feeHook})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodStables,
		Params: []any{stable}}, []any{&wadOffset})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodMintCap,
		Params: []any{stable}}, []any{&mintCap})
	req.AddCall(&ethrpc.Call{ABI: psmABI, Target: staticExtra.PSM, Method: psmMethodDebtTokenMinted,
		Params: []any{stable}}, []any{&minted})
	req.AddCall(&ethrpc.Call{ABI: erc20ABI, Target: p.Tokens[1].Address, Method: erc20MethodBalanceOf,
		Params: []any{psm}}, []any{&stableReserve})
	resp, err := req.Aggregate()
	if err != nil {
		return p, err
	}

	// De-whitelisted mid-flight: both directions revert NotListedToken on-chain.
	if wadOffset == 0 {
		paused = true
	}

	// Second round pinned to the same block: the fee through the just-resolved hook.
	entryFee, exitFee := new(big.Int), new(big.Int)
	feeCaller := common.HexToAddress(strings.ToLower(t.config.FeeCaller))
	feeReq := t.ethrpcClient.NewRequest().SetContext(ctx)
	if overrides != nil {
		feeReq.SetOverrides(overrides)
	}
	if resp.BlockNumber != nil {
		feeReq.SetBlockNumber(resp.BlockNumber)
	}
	hookTarget := common.Address(feeHook)
	feeReq.AddCall(&ethrpc.Call{ABI: feeHookABI, Target: hookTarget.Hex(), Method: feeHookMethodCalcFee,
		Params: []any{feeCaller, stable, big.NewInt(0), actionDeposit}}, []any{&entryFee})
	feeReq.AddCall(&ethrpc.Call{ABI: feeHookABI, Target: hookTarget.Hex(), Method: feeHookMethodCalcFee,
		Params: []any{feeCaller, stable, big.NewInt(0), actionRedeem}}, []any{&exitFee})
	if _, err := feeReq.Aggregate(); err != nil {
		return p, err
	}

	for _, v := range []*big.Int{mintCap, minted, stableReserve, entryFee, exitFee} {
		if v == nil || v.Sign() < 0 {
			return p, ErrInvalidSnapshot
		}
	}

	extraBytes, err := json.Marshal(Extra{
		Paused:          paused,
		EntryFeeBp:      entryFee,
		ExitFeeBp:       exitFee,
		MintCap:         mintCap,
		DebtTokenMinted: minted,
		StableReserve:   stableReserve,
	})
	if err != nil {
		return p, err
	}

	p.Extra = string(extraBytes)
	// Router-visible liquidity: debt side bounded by the remaining mint room, stable
	// side by the PSM's physical reserve.
	capRoom := new(big.Int).Sub(mintCap, minted)
	if capRoom.Sign() < 0 {
		capRoom = new(big.Int)
	}
	p.Reserves = entity.PoolReserves{capRoom.String(), stableReserve.String()}
	p.Timestamp = time.Now().Unix()
	if resp.BlockNumber != nil {
		p.BlockNumber = resp.BlockNumber.Uint64()
	}
	return p, nil
}
