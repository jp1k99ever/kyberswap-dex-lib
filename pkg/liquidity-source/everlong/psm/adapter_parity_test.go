package everlongpsm

import (
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/forktest"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// TestAdapterParity: quote -> executor `data` -> EverlongPsmAdapter fill on an anvil
// fork, wei-exact both directions (the cap clamps are pinned by the unit and forge
// suites; the whale cannot out-fund the live mint room).
func TestAdapterParity(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	f := forktest.Start(t, forktest.ForkURL(psmForkFallback()))
	f.MintNECT(t, new(big.Int).Mul(big.NewInt(500), big.NewInt(1e18)))
	// The PSM policy is keyed by the calling contract. Deploy once before tracking,
	// price that exact address and reuse it for every sequential fill, matching one
	// production executor transaction rather than sampling address(0) and executing as
	// a succession of unrelated adapters.
	adapter := f.DeployAdapter(t, "EverlongPsmAdapter")

	client := ethrpc.New(f.URL).SetMulticallContract(common.HexToAddress(forktest.Multicall3))
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		PSM: forktest.PSM, Stables: []string{forktest.HONEY},
		FeeCaller: adapter.Hex()}
	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)
	// This fork deploys the exact execution adapter before the snapshot, so these are
	// today's live rates for the caller that actually settles every fill below.
	require.Equal(t, "5", sim.Extra.EntryFeeBp.String())
	require.Equal(t, "5", sim.Extra.ExitFeeBp.String())

	debt, stable := sim.Info.Tokens[0], sim.Info.Tokens[1]
	meta := sim.GetMetaInfo(stable, debt).(PoolMeta)
	data := forktest.Words(meta.PSM, debt) // word 0: PSM, word 1: debt token
	whale := common.HexToAddress(forktest.Whale)
	recipient := common.HexToAddress("0x00000000000000000000000000000000000000a2")

	wad := big.NewInt(1e18)
	fills := []struct {
		name     string
		tokenIn  string
		amountIn *big.Int
	}{
		{"deposit 100", stable, new(big.Int).Mul(big.NewInt(100), wad)},
		{"redeem 100", debt, new(big.Int).Mul(big.NewInt(100), wad)},
		{"deposit 1.5M (a large lot well inside the mint room)", stable, new(big.Int).Mul(big.NewInt(1_500_000), wad)},
	}
	for _, c := range fills {
		tokenOut := debt
		if c.tokenIn == debt {
			tokenOut = stable
		}
		q, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: c.tokenIn, Amount: c.amountIn}, TokenOut: tokenOut})
		require.NoError(t, err, c.name)

		fill := f.Execute(t, adapter, "executeEverlongPsm", data, c.amountIn,
			common.HexToAddress(c.tokenIn), common.HexToAddress(tokenOut), whale, recipient)

		require.Zero(t, fill.AmountOut.Cmp(q.TokenAmountOut.Amount),
			"%s: amountOut adapter %s vs quote %s", c.name, fill.AmountOut, q.TokenAmountOut.Amount)
		require.Zero(t, fill.AmountUnused.Cmp(forktest.Remaining(q.RemainingTokenAmountIn.Amount)),
			"%s: amountUnused adapter %s vs quote %s", c.name, fill.AmountUnused, q.RemainingTokenAmountIn.Amount)
		require.LessOrEqual(t, fill.GasUsed, uint64(q.Gas), "%s: gas used above the quoted estimate", c.name)
		t.Logf("%s: out %s unused %s gas %d (quoted %d)", c.name, fill.AmountOut, fill.AmountUnused, fill.GasUsed, q.Gas)
		sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: q.SwapInfo})
	}
}

func psmForkFallback() string {
	if u := os.Getenv("EVERLONG_PSM_RPC_URL"); u != "" {
		return u
	}
	return "https://rpc.berachain.com"
}
