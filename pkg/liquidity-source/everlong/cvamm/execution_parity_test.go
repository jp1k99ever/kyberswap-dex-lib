package everlongcvamm

import (
	"math/big"
	"os"
	"testing"

	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

// venueFixture pins a live Berachain venue state together with the venue's own answers
// at that state, obtained by eth_call on CvammALM.swap under state overrides. Unlike
// the pure-curve grid in cvamm_fixtures.csv these vectors exercise the WHOLE pipeline:
// the curve fill, the solvency clamp against the accounted reserves, and the
// directional output-fee haircut at the venue's real (off-chain-unreproducible) rates.
// No quoter contract is involved — the swap is the oracle.
type venueFixture struct {
	Block   uint64 `json:"block"`
	ALM     string `json:"alm"`
	Token0  string `json:"token0"`
	Token1  string `json:"token1"`
	Support struct {
		AWad string `json:"aWad"`
		XLo  string `json:"xLo"`
		XHi  string `json:"xHi"`
		YHi  string `json:"yHi"`
	} `json:"support"`
	XWad               string `json:"xWad"`
	AnchorSqrtCurveX96 string `json:"anchorSqrtCurveX96"`
	Kappa              string `json:"kappa"`
	ReserveStable      string `json:"reserveStable"`
	ReserveVolatile    string `json:"reserveVolatile"`
	FeeStableInWad     string `json:"feeStableInWad"`
	FeeVolatileInWad   string `json:"feeVolatileInWad"`
	Paused             bool   `json:"paused"`
	Executions         []struct {
		StableIn     bool   `json:"stableIn"`
		AmountIn     string `json:"amountIn"`
		AmountInUsed string `json:"amountInUsed"`
		AmountOut    string `json:"amountOut"`
	} `json:"executions"`
}

func (f *venueFixture) newSimulator(t *testing.T) *PoolSimulator {
	t.Helper()
	extraBytes, err := json.Marshal(map[string]any{
		"sup": map[string]string{
			"a": f.Support.AWad, "xLo": f.Support.XLo, "xHi": f.Support.XHi, "yHi": f.Support.YHi,
		},
		"x":      f.XWad,
		"anchor": f.AnchorSqrtCurveX96,
		"kappa":  f.Kappa,
		"feeS":   f.FeeStableInWad,
		"feeV":   f.FeeVolatileInWad,
		"paused": f.Paused,
	})
	require.NoError(t, err)

	sim, err := NewPoolSimulator(entity.Pool{
		Address:  f.ALM,
		Exchange: "everlong-cvamm",
		Type:     DexType,
		Tokens: []*entity.PoolToken{
			{Address: f.Token0, Swappable: true},
			{Address: f.Token1, Swappable: true},
		},
		Reserves:    entity.PoolReserves{f.ReserveStable, f.ReserveVolatile},
		StaticExtra: "{}",
		Extra:       string(extraBytes),
		BlockNumber: f.Block,
	})
	require.NoError(t, err)
	return sim
}

func loadVenueFixture(t *testing.T) venueFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/berachain_block_24712088.json")
	require.NoError(t, err)
	var f venueFixture
	require.NoError(t, json.Unmarshal(raw, &f))
	return f
}

// TestExecutionParity is the end of the evidence chain: these are REAL settled
// CvammALM.swap fills at the pinned state — the venue's own (amountInUsed, amountOut)
// return values, not a view function. The simulator must reproduce both, so a partial
// fill's charge (amountIn - RemainingTokenAmountIn) is asserted too.
func TestExecutionParity(t *testing.T) {
	f := loadVenueFixture(t)
	require.NotEmpty(t, f.Executions)
	sim := f.newSimulator(t)

	reverted := 0
	for i, e := range f.Executions {
		amountIn, ok := new(big.Int).SetString(e.AmountIn, 10)
		require.True(t, ok)
		tokenIn, tokenOut := f.Token0, f.Token1
		if !e.StableIn {
			tokenIn, tokenOut = tokenOut, tokenIn
		}
		res, calcErr := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn},
			TokenOut:      tokenOut,
		})
		if e.AmountOut == "0" {
			require.Error(t, calcErr,
				"case %d (stableIn=%v in=%s): the swap reverts on-chain", i, e.StableIn, e.AmountIn)
			reverted++
			continue
		}
		require.NoError(t, calcErr, "case %d (stableIn=%v in=%s)", i, e.StableIn, e.AmountIn)
		require.Equal(t, e.AmountOut, res.TokenAmountOut.Amount.String(),
			"case %d (stableIn=%v in=%s) amountOut", i, e.StableIn, e.AmountIn)
		used := new(big.Int).Sub(amountIn, res.RemainingTokenAmountIn.Amount)
		require.Equal(t, e.AmountInUsed, used.String(),
			"case %d (stableIn=%v in=%s) amountInUsed", i, e.StableIn, e.AmountIn)
	}
	require.Equal(t, 1, reverted, "fixture must pin the dust fill that reverts on-chain")
}
