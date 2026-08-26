package everlongrebalancer

import (
	"context"
	"math/big"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	everlongcvamm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/cvamm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/forktest"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// TestAdapterParity is the executor-path proof for the rebalancer, COUPLED to its CVAMM
// base pool. Every hop is quoted from a fresh track (what the router does each block),
// encoded into the adapter's `data` exactly as the executor would from PoolMeta/SwapInfo,
// filled through the deployed EverlongRebalancerAdapter (EverlongCvammAdapter for the
// base hop) on one anvil fork, and must return the quoted amounts wei-exact. A second
// pair of simulators is advanced only by UpdateBalance across the whole sequence and
// must remain wei-exact wherever it remains quotable. A price-moving base swap makes
// leverage fail closed until a fresh snapshot re-attests the wrapper's external
// reference-oracle gate; no numeric tolerance substitutes for that missing fact.
func TestAdapterParity(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	f := forktest.Start(t, forktest.ForkURL(berachainRPCURL()))
	f.MintNECT(t, new(big.Int).Mul(big.NewInt(200), big.NewInt(1e18)))

	client := ethrpc.New(f.URL).SetMulticallContract(common.HexToAddress(forktest.Multicall3))
	cfg := berachainTestConfig()
	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &se))
	cvammCfg := &everlongcvamm.Config{DexID: everlongcvamm.DexType, ChainID: valueobject.ChainIDBerachain,
		ALMs: []everlongcvamm.ALMConfig{{Address: se.UnderlyingCvamm}}}
	cvammPools, _, err := everlongcvamm.NewPoolsListUpdater(cvammCfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)

	// fresh coupled simulators off the fork head
	track := func() (*PoolSimulator, *everlongcvamm.PoolSimulator) {
		tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
		require.NoError(t, err)
		cvammTracked, err := everlongcvamm.NewPoolTracker(cvammCfg, client).GetNewPoolStateAtBlock(
			ctx, cvammPools[0], new(big.Int).SetUint64(tracked.BlockNumber))
		require.NoError(t, err)
		base, err := everlongcvamm.NewPoolSimulator(cvammTracked)
		require.NoError(t, err)
		sim, err := NewPoolSimulatorWithBases(tracked, map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
		require.NoError(t, err)
		return sim, base
	}
	advanced, advancedBase := track()

	stable, volatile := advanced.Info.Tokens[0], advanced.Info.Tokens[1]
	meta := advanced.GetMetaInfo("", "").(PoolMeta)
	baseMeta := advancedBase.GetMetaInfo("", "").(everlongcvamm.PoolMeta)
	whale := common.HexToAddress(forktest.Whale)
	recipient := common.HexToAddress("0x00000000000000000000000000000000000000a3")
	wad := big.NewInt(1e18)

	// The executor's `data` for the rebalancer adapter, from PoolMeta + the quote:
	//   word 2: leverage -> CollVaultShares; deleverage -> GrossStableIn
	//   word 3: deleverage -> the stable the route delivers (amountIn - remaining)
	//   word 4/5: the deployed math and the ratio it validates against
	rebalancerData := func(q *pool.CalcAmountOutResult, amountIn *big.Int) []byte {
		si := q.SwapInfo.(SwapInfo)
		if si.IsLeverage {
			return forktest.Words(meta.Swapper, stable, si.CollVaultShares, new(big.Int), meta.Math, meta.LeverageRatioWad)
		}
		delivered := new(big.Int).Sub(amountIn, forktest.Remaining(q.RemainingTokenAmountIn.Amount))
		return forktest.Words(meta.Swapper, stable, si.GrossStableIn, delivered, meta.Math, meta.LeverageRatioWad)
	}
	quote := func(s pool.IPoolSimulator, tokenIn, tokenOut string, amountIn *big.Int) *pool.CalcAmountOutResult {
		q, err := s.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn}, TokenOut: tokenOut})
		require.NoError(t, err)
		return q
	}

	rebalancerFill := func(name, tokenIn string, amountIn *big.Int) {
		tokenOut := volatile
		if tokenIn == volatile {
			tokenOut = stable
		}
		fresh, _ := track()
		q := quote(fresh, tokenIn, tokenOut, amountIn)
		qAdv := quote(advanced, tokenIn, tokenOut, amountIn)

		adapter := f.DeployAdapter(t, "EverlongRebalancerAdapter")
		fill := f.Execute(t, adapter, "executeEverlongRebalancer", rebalancerData(q, amountIn), amountIn,
			common.HexToAddress(tokenIn), common.HexToAddress(tokenOut), whale, recipient)

		require.Zero(t, fill.AmountOut.Cmp(q.TokenAmountOut.Amount),
			"%s: amountOut adapter %s vs fresh quote %s", name, fill.AmountOut, q.TokenAmountOut.Amount)
		dust := new(big.Int).Sub(fill.AmountUnused, forktest.Remaining(q.RemainingTokenAmountIn.Amount))
		require.Zero(t, dust.Sign(),
			"%s: amountUnused adapter %s vs fresh quote %s", name, fill.AmountUnused, q.RemainingTokenAmountIn.Amount)
		require.LessOrEqual(t, fill.GasUsed, uint64(q.Gas), "%s: gas used above the quoted estimate", name)
		require.Zero(t, qAdv.TokenAmountOut.Amount.Cmp(fill.AmountOut),
			"%s: advanced (UpdateBalance-only) quote %s vs fill %s", name, qAdv.TokenAmountOut.Amount, fill.AmountOut)
		t.Logf("%s: out %s unused %s gas %d (quoted %d); advanced sim %s", name,
			fill.AmountOut, fill.AmountUnused, fill.GasUsed, q.Gas, qAdv.TokenAmountOut.Amount)
		advanced.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: qAdv.SwapInfo})
	}
	cvammFill := func(name string, amountIn *big.Int) {
		_, freshBase := track()
		q := quote(freshBase, stable, volatile, amountIn)
		qAdv := quote(advancedBase, stable, volatile, amountIn)
		adapter := f.DeployAdapter(t, "EverlongCvammAdapter")
		fill := f.Execute(t, adapter, "executeEverlongCvamm", forktest.Words(baseMeta.ALM), amountIn,
			common.HexToAddress(stable), common.HexToAddress(volatile), whale, recipient)
		require.Zero(t, fill.AmountOut.Cmp(q.TokenAmountOut.Amount), "%s: amountOut", name)
		require.Zero(t, fill.AmountUnused.Cmp(forktest.Remaining(q.RemainingTokenAmountIn.Amount)), "%s: amountUnused", name)
		require.Zero(t, qAdv.TokenAmountOut.Amount.Cmp(fill.AmountOut), "%s: the pushed base must quote the CVAMM hop exactly", name)
		t.Logf("%s: out %s unused %s gas %d (quoted %d)", name, fill.AmountOut, fill.AmountUnused, fill.GasUsed, q.Gas)
		advancedBase.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: qAdv.SwapInfo})
	}

	rebalancerFill("deleverage 5 NECT (hinted)", stable, new(big.Int).Mul(big.NewInt(5), wad))
	rebalancerFill("leverage 10k sats", volatile, big.NewInt(10_000))
	cvammFill("cvamm hop 15 NECT (moves the shared book)", new(big.Int).Mul(big.NewInt(15), wad))
	rebalancerFill("deleverage 20 NECT on the folded state", stable, new(big.Int).Mul(big.NewInt(20), wad))

	// The prior CVAMM fill changed x. Its reserves and rvps are replayed exactly, but the
	// wrapper's leverage-only external reference neighborhood is not a local state word.
	// The advanced simulator must refuse; a fresh same-block track re-attests it and its
	// large leverage quote must still execute wei-exactly through the adapter.
	large := big.NewInt(3_000_000)
	_, staleErr := advanced.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: volatile, Amount: large}, TokenOut: stable})
	require.ErrorIs(t, staleErr, ErrVenueGateClosed)
	require.ErrorIs(t, staleErr, ErrUnattestedReference)

	fresh, _ := track()
	q := quote(fresh, volatile, stable, large)
	adapter := f.DeployAdapter(t, "EverlongRebalancerAdapter")
	fill := f.Execute(t, adapter, "executeEverlongRebalancer", rebalancerData(q, large), large,
		common.HexToAddress(volatile), common.HexToAddress(stable), whale, recipient)
	require.Zero(t, fill.AmountOut.Cmp(q.TokenAmountOut.Amount),
		"fresh large leverage: amountOut adapter %s vs quote %s", fill.AmountOut, q.TokenAmountOut.Amount)
}
