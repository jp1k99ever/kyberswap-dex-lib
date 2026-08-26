package everlongpsm

import (
	"context"
	"errors"
	"math/big"
	"os"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// TestLiveListTrackQuote runs lister -> tracker -> simulator against the deployed
// Berachain PermissionlessPSM. The deployment's capacity words are governance-settable,
// so quotes must either succeed or fail with a documented sentinel — a decode error or
// panic is the only failure mode this test rejects.
func TestLiveListTrackQuote(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{
		DexID:   DexType,
		ChainID: valueobject.ChainIDBerachain,
		PSM:     "0x0999417c0f9ded4356B099bcC83A16437B841323",
		Stables: []string{"0xFCBD14DC51f0A4d49d5E53C2E0950e0bC26d0Dce"}, // HONEY
	}

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1, "HONEY is whitelisted on the deployed PSM")

	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)

	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)
	debt, stable := sim.Info.Tokens[0], sim.Info.Tokens[1]

	// The rate legs must actually decode: the deployment's capacity is currently zero,
	// so every quote below short-circuits on a capacity sentinel and would pass even if
	// the fee reads had silently failed.
	e := sim.Extra
	require.NotNil(t, e.EntryFeeBp, "entry rate must decode through feeBpFor")
	require.NotNil(t, e.ExitFeeBp, "exit rate must decode through feeBpFor")
	require.True(t, e.EntryFeeBp.Sign() > 0, "the PSM refuses a zero entry toll")
	require.True(t, e.EntryFeeBp.Cmp(bigBp) < 0 && e.ExitFeeBp.Cmp(bigBp) < 0,
		"rates are bounded below 100%%: entry %s exit %s", e.EntryFeeBp, e.ExitFeeBp)
	require.NotNil(t, e.AvailableMint)
	require.NotNil(t, e.AvailableReserve)
	t.Logf("live psm: entry %s bp, exit %s bp, availableMint %s, availableReserve %s",
		e.EntryFeeBp, e.ExitFeeBp, e.AvailableMint, e.AvailableReserve)

	sentinels := []error{ErrPaused, ErrCapExhausted, ErrNothingToRedeem, ErrZeroAmountOut, ErrFeeUnavailable}
	quote := func(tokenIn, tokenOut string, amountIn *big.Int) {
		res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn},
			TokenOut:      tokenOut,
		})
		if err == nil {
			require.True(t, res.TokenAmountOut.Amount.Sign() > 0)
			return
		}
		for _, s := range sentinels {
			if errors.Is(err, s) {
				return
			}
		}
		t.Fatalf("quote %s->%s failed outside the documented sentinels: %v", tokenIn, tokenOut, err)
	}
	quote(stable, debt, big.NewInt(1e18)) // deposit: 1 HONEY -> NECT
	quote(debt, stable, big.NewInt(1e18)) // redeem: 1 NECT -> HONEY
}

func berachainPsmRPCURL() string {
	if url := os.Getenv("EVERLONG_PSM_RPC_URL"); url != "" {
		return url
	}
	return "https://rpc.berachain.com"
}

// TestFeeIsCallerInvariant guards the one assumption Config.FeeCaller rests on. The
// tracker prices for FeeCaller while the fill is made BY THE ADAPTER, so the two agree
// only while the hook charges every caller the same. The moment a per-caller entry
// appears this fails, and FeeCaller must be set to the deployed adapter address.
func TestFeeIsCallerInvariant(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	const honey = "0xFCBD14DC51f0A4d49d5E53C2E0950e0bC26d0Dce"

	var rates []string
	for _, caller := range []string{
		"0x0000000000000000000000000000000000000000", // the default
		"0x4A964e9658792f294AF4BF923ca1A38F6FBa0896", // a real trading EOA
		"0x1111111111111111111111111111111111111111", // an arbitrary address
	} {
		cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
			PSM:     "0x0999417c0f9ded4356B099bcC83A16437B841323",
			Stables: []string{honey}, FeeCaller: caller}
		pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
		require.NoError(t, err)
		tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
		require.NoError(t, err)
		sim, err := NewPoolSimulator(tracked)
		require.NoError(t, err)
		require.NotNil(t, sim.Extra.EntryFeeBp)
		rates = append(rates, sim.Extra.EntryFeeBp.String()+"/"+sim.Extra.ExitFeeBp.String())
	}
	for i := 1; i < len(rates); i++ {
		require.Equal(t, rates[0], rates[i],
			"the PSM now prices per caller — set Config.FeeCaller to the deployed adapter address")
	}
	t.Logf("fee is caller-invariant at %s bp; the zero-address default is safe", rates[0])
}

// TestTrackerStampsBlockNumber: the snapshot must carry its block — the cap-hook round
// is pinned to it, and TryAggregate returned none, leaving that round on `latest`.
func TestTrackerStampsBlockNumber(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		PSM:     "0x0999417c0f9ded4356B099bcC83A16437B841323",
		Stables: []string{"0xFCBD14DC51f0A4d49d5E53C2E0950e0bC26d0Dce"}}

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	p := pools[0]
	p.BlockNumber = 0

	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, p, pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	require.NotZero(t, tracked.BlockNumber, "the tracker must stamp the block it read at")
}

// TestListerCursorDoesNotRelist: with one listed and one un-whitelisted candidate, a latch
// either re-emits the listed pool on every poll or never picks the second one up. The
// cursor does neither.
func TestListerCursorDoesNotRelist(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		PSM: "0x0999417c0f9ded4356B099bcC83A16437B841323",
		Stables: []string{
			"0xFCBD14DC51f0A4d49d5E53C2E0950e0bC26d0Dce", // HONEY, whitelisted
			"0x549943e04f40284185054145c6E4e9568C1D3241", // not whitelisted
		}}

	u := NewPoolsListUpdater(cfg, client)
	first, meta, err := u.GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, first, 1, "only the whitelisted stable lists")

	second, meta2, err := u.GetNewPools(ctx, meta)
	require.NoError(t, err)
	require.Empty(t, second, "an already-listed pool must not be emitted again")

	// The un-whitelisted candidate stays a candidate, so it is picked up if it is listed later.
	var m Metadata
	require.NoError(t, json.Unmarshal(meta2, &m))
	require.True(t, m.Listed["0xfcbd14dc51f0a4d49d5e53c2e0950e0bc26d0dce"])
	require.False(t, m.Listed["0x549943e04f40284185054145c6e4e9568c1d3241"])
}
