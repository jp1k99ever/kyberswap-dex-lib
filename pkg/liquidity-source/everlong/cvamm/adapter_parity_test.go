package everlongcvamm

import (
	"context"
	"math/big"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/forktest"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestAdapterParity is the executor-path proof: the real lister/tracker/simulator
// quote, encoded into `data` the way the executor does from PoolMeta, filled through
// the deployed EverlongCvammAdapter on an anvil fork, must return exactly the quoted
// amountOut and RemainingTokenAmountIn — fill after fill on ONE simulator advanced only
// by UpdateBalance, so the second and later hops prove the state transition too.
func TestAdapterParity(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	f := forktest.Start(t, forktest.ForkURL(cvammRPCURL()))
	f.MintNECT(t, new(big.Int).Mul(big.NewInt(60_000), big.NewInt(1e18)))

	client := ethrpc.New(f.URL).SetMulticallContract(common.HexToAddress(forktest.Multicall3))
	_, cfg := liveClient()
	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	meta := sim.GetMetaInfo("", "").(PoolMeta)
	data := forktest.Words(meta.ALM) // word 0: the ALM
	stable, volatile := sim.Info.Tokens[0], sim.Info.Tokens[1]
	whale := common.HexToAddress(forktest.Whale)
	recipient := common.HexToAddress("0x00000000000000000000000000000000000000a1")

	wad := big.NewInt(1e18)
	fills := []struct {
		name     string
		tokenIn  string
		amountIn *big.Int
	}{
		{"stable-in small", stable, new(big.Int).Div(wad, big.NewInt(2))},
		{"stable-in 50", stable, new(big.Int).Mul(big.NewInt(50), wad)},
		{"volatile-in 10k sats", volatile, big.NewInt(10_000)},
		{"stable-in oversized (band-truncated partial)", stable, new(big.Int).Mul(big.NewInt(50_000), wad)},
		{"volatile-in after the reversal", volatile, big.NewInt(25_000)},
	}
	for _, c := range fills {
		tokenOut := volatile
		if c.tokenIn == volatile {
			tokenOut = stable
		}
		q, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: c.tokenIn, Amount: c.amountIn}, TokenOut: tokenOut})
		require.NoError(t, err, c.name)

		adapter := f.DeployAdapter(t, "EverlongCvammAdapter")
		fill := f.Execute(t, adapter, "executeEverlongCvamm", data, c.amountIn,
			common.HexToAddress(c.tokenIn), common.HexToAddress(tokenOut), whale, recipient)

		require.Zero(t, fill.AmountOut.Cmp(q.TokenAmountOut.Amount),
			"%s: amountOut adapter %s vs quote %s", c.name, fill.AmountOut, q.TokenAmountOut.Amount)
		require.Zero(t, fill.AmountUnused.Cmp(forktest.Remaining(q.RemainingTokenAmountIn.Amount)),
			"%s: amountUnused adapter %s vs quote %s", c.name, fill.AmountUnused, q.RemainingTokenAmountIn.Amount)
		require.LessOrEqual(t, fill.GasUsed, uint64(q.Gas), "%s: gas used above the quoted estimate", c.name)
		if c.name == "stable-in oversized (band-truncated partial)" {
			require.Positive(t, fill.AmountUnused.Sign(), "the oversized fill must actually truncate")
		}
		t.Logf("%s: out %s unused %s gas %d (quoted %d)", c.name, fill.AmountOut, fill.AmountUnused, fill.GasUsed, q.Gas)
		sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: q.SwapInfo})
	}
}
