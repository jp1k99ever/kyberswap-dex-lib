package everlongrebalancer

import (
	"context"
	"math"
	"math/big"
	"os"
	"strconv"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	everlongcvamm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/cvamm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/forktest"
	everlongpsm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/psm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestDifferentialFuzz is the executor-path proof at scale: on one anvil fork, a seeded
// random sequence of fills across all three venues — direction and size (log-uniform,
// from dust to past the book) drawn at random — each quoted by the real lister/tracker/
// simulator at the fork head, encoded into the adapter's `data` as the executor would
// and settled through the deployed adapter. Every fill must return the quoted amountOut
// and RemainingTokenAmountIn wei for wei (the rebalancer's deleverage may refund up to
// two wei of headroom, as documented on the adapter). A quote the simulator refuses is
// recorded, never executed; the run requires a minimum of settled fills per venue so a
// simulator that refuses everything cannot pass by silence.
//
// EVERLONG_FUZZ_FILLS sets the fill count (default 45), EVERLONG_PROP_SEED the seed.
func TestDifferentialFuzz(t *testing.T) {
	test.SkipCI(t)
	r := propRand(t)
	fills := 45
	if s := os.Getenv("EVERLONG_FUZZ_FILLS"); s != "" {
		v, err := strconv.Atoi(s)
		require.NoError(t, err)
		fills = v
	}
	ctx := context.Background()
	f := forktest.Start(t, forktest.ForkURL(berachainRPCURL()))
	f.MintNECT(t, new(big.Int).Mul(big.NewInt(80_000), big.NewInt(1e18)))
	client := ethrpc.New(f.URL).SetMulticallContract(common.HexToAddress(forktest.Multicall3))

	// listings (static) once
	rebCfg := berachainTestConfig()
	rebPools, _, err := NewPoolsListUpdater(rebCfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(rebPools[0].StaticExtra), &se))
	cvCfg := &everlongcvamm.Config{DexID: everlongcvamm.DexType, ALMs: []everlongcvamm.ALMConfig{{Address: se.UnderlyingCvamm}}}
	cvPools, _, err := everlongcvamm.NewPoolsListUpdater(cvCfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	psmCfg := &everlongpsm.Config{DexID: everlongpsm.DexType, PSM: forktest.PSM, Stables: []string{forktest.HONEY}}
	psmPools, _, err := everlongpsm.NewPoolsListUpdater(psmCfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, psmPools, 1)

	whale := common.HexToAddress(forktest.Whale)
	recipient := common.HexToAddress("0x00000000000000000000000000000000000000f2")
	// log-uniform size: 10^e for e in [lo, hi]
	size := func(lo, hi float64) *big.Int {
		x, _ := new(big.Float).SetPrec(200).SetFloat64(math.Pow(10, lo+r.Float64()*(hi-lo))).Int(nil)
		if x.Sign() == 0 {
			x = big.NewInt(1)
		}
		return x
	}

	type venue struct {
		name     string
		adapter  string
		method   string
		quote    func() (q *pool.CalcAmountOutResult, data []byte, tokenIn, tokenOut string, amountIn *big.Int, err error)
		hintFree func(tokenIn, tokenOut string) []byte
	}
	rebMeta := func() PoolMeta {
		tracked, err := NewPoolTracker(rebCfg, client).GetNewPoolState(ctx, rebPools[0], pool.GetNewPoolStateParams{})
		require.NoError(t, err)
		sim, err := NewPoolSimulator(tracked)
		require.NoError(t, err)
		return sim.GetMetaInfo("", "").(PoolMeta)
	}()
	settled, refused := map[string]int{}, map[string]int{}

	venues := []venue{
		{name: "cvamm", adapter: "EverlongCvammAdapter", method: "executeEverlongCvamm", hintFree: func(string, string) []byte {
			return forktest.Words(se.UnderlyingCvamm)
		}, quote: func() (*pool.CalcAmountOutResult, []byte, string, string, *big.Int, error) {
			tracked, err := everlongcvamm.NewPoolTracker(cvCfg, client).GetNewPoolState(ctx, cvPools[0], pool.GetNewPoolStateParams{})
			require.NoError(t, err)
			sim, err := everlongcvamm.NewPoolSimulator(tracked)
			require.NoError(t, err)
			meta := sim.GetMetaInfo("", "").(everlongcvamm.PoolMeta)
			tokenIn, tokenOut, amountIn := sim.Info.Tokens[0], sim.Info.Tokens[1], size(15, 22.7) // 1e15 .. 5e22 NECT
			if r.Intn(2) == 0 {
				tokenIn, tokenOut, amountIn = sim.Info.Tokens[1], sim.Info.Tokens[0], size(2, 7.5) // 100 sats .. 0.3 BTC
			}
			q, err := sim.CalcAmountOut(pool.CalcAmountOutParams{TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn}, TokenOut: tokenOut})
			return q, forktest.Words(meta.ALM), tokenIn, tokenOut, amountIn, err
		}},
		{name: "psm", adapter: "EverlongPsmAdapter", method: "executeEverlongPsm", hintFree: func(string, string) []byte {
			return forktest.Words(forktest.PSM, forktest.NECT)
		}, quote: func() (*pool.CalcAmountOutResult, []byte, string, string, *big.Int, error) {
			tracked, err := everlongpsm.NewPoolTracker(psmCfg, client).GetNewPoolState(ctx, psmPools[0], pool.GetNewPoolStateParams{})
			require.NoError(t, err)
			sim, err := everlongpsm.NewPoolSimulator(tracked)
			require.NoError(t, err)
			debt, stable := sim.Info.Tokens[0], sim.Info.Tokens[1]
			tokenIn, tokenOut, amountIn := stable, debt, size(12, 22.5) // deposit
			if r.Intn(2) == 0 {
				tokenIn, tokenOut, amountIn = debt, stable, size(12, 21.5) // redeem (bounded by the reserve)
			}
			meta := sim.GetMetaInfo(tokenIn, tokenOut).(everlongpsm.PoolMeta)
			q, err := sim.CalcAmountOut(pool.CalcAmountOutParams{TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn}, TokenOut: tokenOut})
			return q, forktest.Words(meta.PSM, debt), tokenIn, tokenOut, amountIn, err
		}},
		{name: "rebalancer", adapter: "EverlongRebalancerAdapter", method: "executeEverlongRebalancer", hintFree: func(string, string) []byte {
			return forktest.Words(rebMeta.Swapper, rebPools[0].Tokens[0].Address, new(big.Int), new(big.Int), rebMeta.Math, rebMeta.LeverageRatioWad)
		}, quote: func() (*pool.CalcAmountOutResult, []byte, string, string, *big.Int, error) {
			tracked, err := NewPoolTracker(rebCfg, client).GetNewPoolState(ctx, rebPools[0], pool.GetNewPoolStateParams{})
			require.NoError(t, err)
			cvTracked, err := everlongcvamm.NewPoolTracker(cvCfg, client).GetNewPoolState(ctx, cvPools[0], pool.GetNewPoolStateParams{})
			require.NoError(t, err)
			base, err := everlongcvamm.NewPoolSimulator(cvTracked)
			require.NoError(t, err)
			sim, err := NewPoolSimulatorWithBases(tracked, map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
			require.NoError(t, err)
			stable, volatile := sim.Info.Tokens[0], sim.Info.Tokens[1]
			meta := sim.GetMetaInfo("", "").(PoolMeta)
			tokenIn, tokenOut, amountIn := stable, volatile, size(16, 21.5) // deleverage 0.01 .. 300k NECT
			if r.Intn(2) == 0 {
				tokenIn, tokenOut, amountIn = volatile, stable, size(2.5, 7) // leverage 300 sats .. 0.1 BTC
			}
			q, err := sim.CalcAmountOut(pool.CalcAmountOutParams{TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn}, TokenOut: tokenOut})
			if err != nil {
				return nil, nil, tokenIn, tokenOut, amountIn, err
			}
			si := q.SwapInfo.(SwapInfo)
			var data []byte
			if si.IsLeverage {
				data = forktest.Words(meta.Swapper, stable, si.CollVaultShares, new(big.Int), meta.Math, meta.LeverageRatioWad)
			} else {
				delivered := new(big.Int).Sub(amountIn, forktest.Remaining(q.RemainingTokenAmountIn.Amount))
				data = forktest.Words(meta.Swapper, stable, si.GrossStableIn, delivered, meta.Math, meta.LeverageRatioWad)
			}
			return q, data, tokenIn, tokenOut, amountIn, nil
		}},
	}

	for i := 0; i < fills; i++ {
		v := venues[r.Intn(len(venues))]
		q, data, tokenIn, tokenOut, amountIn, err := v.quote()
		if err != nil {
			// The converse proof: a fill the simulator refuses must fail on chain too, else
			// the simulator is hiding liquidity. Probed hint-free through the adapter.
			if f.Balance(t, common.HexToAddress(tokenIn), whale).Cmp(amountIn) >= 0 {
				probe := f.DeployAdapter(t, v.adapter)
				callErr := f.TryExecute(t, probe, v.method, v.hintFree(tokenIn, tokenOut), amountIn,
					common.HexToAddress(tokenIn), common.HexToAddress(tokenOut), whale, recipient)
				require.Error(t, callErr, "#%d %s: simulator refused (%v) but the venue fills %s of %s", i, v.name, err, amountIn, tokenIn)
			}
			refused[v.name]++
			t.Logf("#%d %s %s in %s: refused (%v); venue agrees", i, v.name, tokenIn[:10], amountIn, err)
			continue
		}
		// the whale cannot fund what the fork does not hold; skip those lots
		if f.Balance(t, common.HexToAddress(tokenIn), whale).Cmp(amountIn) < 0 {
			refused[v.name]++
			continue
		}
		adapter := f.DeployAdapter(t, v.adapter)
		fill := f.Execute(t, adapter, v.method, data, amountIn,
			common.HexToAddress(tokenIn), common.HexToAddress(tokenOut), whale, recipient)
		require.Zero(t, fill.AmountOut.Cmp(q.TokenAmountOut.Amount),
			"#%d %s: amountOut adapter %s vs quote %s (in %s of %s)", i, v.name, fill.AmountOut, q.TokenAmountOut.Amount, amountIn, tokenIn)
		dust := new(big.Int).Sub(fill.AmountUnused, forktest.Remaining(q.RemainingTokenAmountIn.Amount))
		maxDust := big.NewInt(0)
		if v.name == "rebalancer" && tokenIn == rebPools[0].Tokens[0].Address { // deleverage headroom
			maxDust = big.NewInt(2)
		}
		require.True(t, dust.Sign() >= 0 && dust.Cmp(maxDust) <= 0,
			"#%d %s: amountUnused adapter %s vs quote %s", i, v.name, fill.AmountUnused, q.RemainingTokenAmountIn.Amount)
		settled[v.name]++
		t.Logf("#%d %s in %s -> out %s unused %s gas %d", i, v.name, amountIn, fill.AmountOut, fill.AmountUnused, fill.GasUsed)
	}
	t.Logf("settled %v refused %v", settled, refused)
	for _, v := range venues {
		require.GreaterOrEqual(t, settled[v.name], 5, "%s: too few settled fills to mean anything", v.name)
	}
}
