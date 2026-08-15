package everlongcollvault

import (
	"context"
	"math/big"
	"testing"

	"github.com/goccy/go-json"

	"github.com/stretchr/testify/require"

	everlongcvamm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/cvamm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestBasePoolCoupling wires the live CollVault pool to the live everlong-cvamm pool it
// actually trades on (the swapper's ALM adapter wraps the CvammALM) and checks the meta
// coupling end to end:
//   - the listing resolves the underlying CvammALM from the adapter bytecode;
//   - with no base movement the coupled quote equals the uncoupled quote;
//   - after a CVAMM fill is applied to the base sim, the CollVault quote moves, and it
//     equals the quote of an uncoupled sim whose snapshot is shifted by the same ALM
//     deltas (the fold is exact, not directional);
//   - UpdateBalance absorbs the base deltas once (baseline advances — no double count).
var bigWadTest = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

func TestBasePoolCoupling(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	cfg := berachainTestConfig()
	client := berachainRPCClient()

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)

	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &se))
	require.NotEmpty(t, se.UnderlyingCvamm, "adapter bytecode scan must resolve the wrapped CvammALM")

	cvammCfg := &everlongcvamm.Config{DexID: "everlong-cvamm", ALMs: []everlongcvamm.ALMConfig{{Address: se.UnderlyingCvamm}}}
	cvammPools, _, err := everlongcvamm.NewPoolsListUpdater(cvammCfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, cvammPools, 1)

	// track both at head
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	cvammTracked, err := everlongcvamm.NewPoolTracker(cvammCfg, client).GetNewPoolState(ctx, cvammPools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)

	base, err := everlongcvamm.NewPoolSimulator(cvammTracked)
	require.NoError(t, err)

	coupled, err := NewPoolSimulatorWithBases(tracked, map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
	require.NoError(t, err)
	require.NotNil(t, coupled.basePool, "base must wire by the resolved address")
	uncoupled, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	stable, volatile := coupled.Info.Tokens[0], coupled.Info.Tokens[1]
	quoteIn := pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: stable, Amount: new(big.Int).Mul(big.NewInt(5), bigWadTest)}, // 5 NECT deleverage probe
		TokenOut:      volatile,
	}

	// 1) no base movement: coupled == uncoupled
	q0, err0 := coupled.CalcAmountOut(quoteIn)
	u0, uerr0 := uncoupled.CalcAmountOut(quoteIn)
	require.Equal(t, uerr0 == nil, err0 == nil)
	if err0 == nil {
		require.Equal(t, u0.TokenAmountOut.Amount, q0.TokenAmountOut.Amount)
	}

	// 2) apply a CVAMM fill to the base and expect the coupled quote to move and match a
	// manually shifted snapshot
	baseStable, baseVolatile := base.Info.Tokens[0], base.Info.Tokens[1]
	swap, err := base.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: baseStable, Amount: new(big.Int).Mul(big.NewInt(10), bigWadTest)}, // 10 NECT into the CVAMM
		TokenOut:      baseVolatile,
	})
	require.NoError(t, err)
	before := [2]*big.Int{new(big.Int).Set(base.GetReserves()[0]), new(big.Int).Set(base.GetReserves()[1])}
	base.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: swap.SwapInfo})
	dS := new(big.Int).Sub(base.GetReserves()[0], before[0])
	dV := new(big.Int).Sub(base.GetReserves()[1], before[1])
	require.True(t, dS.Sign() != 0 || dV.Sign() != 0)

	shifted, err := NewPoolSimulator(tracked)
	require.NoError(t, err)
	foldBaseDeltas(&shifted.Extra, dS, dV)

	q1, err1 := coupled.CalcAmountOut(quoteIn)
	s1, serr1 := shifted.CalcAmountOut(quoteIn)
	require.Equal(t, serr1 == nil, err1 == nil)
	if err1 == nil {
		require.Equal(t, s1.TokenAmountOut.Amount, q1.TokenAmountOut.Amount,
			"coupled quote must equal the exactly shifted snapshot")
		if err0 == nil {
			require.NotEqual(t, q0.TokenAmountOut.Amount, q1.TokenAmountOut.Amount,
				"a base fill must move the coupled quote")
		}
	}

	// 3) UpdateBalance absorbs the deltas once and advances the baseline
	if err1 == nil {
		coupled.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: q1.SwapInfo})
		_, _, moved := coupled.baseDeltas()
		require.False(t, moved, "baseline must advance so base movement is not recounted")
	}
}
