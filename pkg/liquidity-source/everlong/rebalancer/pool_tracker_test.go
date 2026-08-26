package everlongrebalancer

import (
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	everlongcvamm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/cvamm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

func TestStateOverridesFailClosed(t *testing.T) {
	original := entity.Pool{Address: "0x0000000000000000000000000000000000000001"}
	got, err := (&PoolTracker{}).GetNewPoolStateWithOverrides(context.Background(), original,
		pool.GetNewPoolStateWithOverridesParams{Overrides: map[common.Address]gethclient.OverrideAccount{
			common.HexToAddress(original.Address): {},
		}})
	require.ErrorIs(t, err, ErrStateOverridesUnsupported)
	require.Equal(t, original, got)
}

func berachainTestConfig() *Config {
	return &Config{
		DexID:      DexType,
		ChainID:    valueobject.ChainIDBerachain,
		Rebalancer: "0xA6b848d899189d263a9398F1DF4534Af7B06d6b3",
		Stable:     "0x1cE0a25D13CE4d52071aE7e02Cf1F6606F4C79d3", // NECT
		Volatile:   "0x0555E30da8f98308EdB960aa94C0Db47230d2B9c", // WBTC
		Math:       "0x4eBD7A6543Ace6076F089082931c380a3675bC5c", // CollRebalancerMath
	}
}

func berachainRPCURL() string {
	if url := os.Getenv("EVERLONG_COLLVAULT_RPC_URL"); url != "" {
		return url
	}
	return "https://rpc.berachain.com"
}

func berachainRPCClient() *ethrpc.Client {
	client := ethrpc.New(berachainRPCURL())
	client.SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	return client
}

// TestLiveListTrackQuote runs the full lister -> tracker -> simulator pipeline against
// live Berachain: resolve the contract graph on-chain, refresh the vault snapshot
// through the tracker's multicall, and quote whichever direction the live state accepts
// off the refreshed entity.
func TestLiveListTrackQuote(t *testing.T) {
	test.SkipCI(t)

	cfg := berachainTestConfig()
	client := berachainRPCClient()

	lister := NewPoolsListUpdater(cfg, client)
	pools, metadata, err := lister.GetNewPools(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)
	again, metadataAgain, err := lister.GetNewPools(context.Background(), metadata)
	require.NoError(t, err)
	require.Empty(t, again, "the exact current cursor must not relist every updater run")
	require.Equal(t, metadata, metadataAgain)

	var staticExtra StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &staticExtra))
	require.Equal(t, "0x27775ec38e2b394738b73c0d25f63e20063df054", staticExtra.Swapper)
	require.Equal(t, "0x9e7f375c351a251e80eb89ad33ca62b270fd9b4a", staticExtra.CollVault)
	require.Equal(t, "0xbd10884d6b55eda1d872cd5108b8aabdc0c3f6ca", staticExtra.ALM)

	tracker := NewPoolTracker(cfg, client)
	p, err := tracker.GetNewPoolState(context.Background(), pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	require.NotEqual(t, "{}", p.Extra)
	require.NotZero(t, p.BlockNumber)

	var extra Extra
	require.NoError(t, json.Unmarshal([]byte(p.Extra), &extra))
	require.True(t, extra.Collateral.Sign() > 0)
	require.True(t, extra.PriceWad.Sign() > 0)

	p.Tokens[0].Decimals = 18
	p.Tokens[1].Decimals = 8
	cvCfg := &everlongcvamm.Config{DexID: everlongcvamm.DexType,
		ALMs: []everlongcvamm.ALMConfig{{Address: staticExtra.UnderlyingCvamm}}}
	cvPools, _, err := everlongcvamm.NewPoolsListUpdater(cvCfg, client).GetNewPools(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, cvPools, 1)
	// Exact meta coupling requires both snapshots at the same block; a second "latest"
	// call can cross a live Berachain block boundary even when no venue state moved.
	cvTracked, err := everlongcvamm.NewPoolTracker(cvCfg, client).GetNewPoolStateAtBlock(
		context.Background(), cvPools[0], new(big.Int).SetUint64(p.BlockNumber))
	require.NoError(t, err)
	base, err := everlongcvamm.NewPoolSimulator(cvTracked)
	require.NoError(t, err)
	sim, err := NewPoolSimulatorWithBases(p,
		map[string]pool.IPoolSimulator{staticExtra.UnderlyingCvamm: base})
	require.NoError(t, err)

	nect, wbtc := p.Tokens[0].Address, p.Tokens[1].Address

	// The CR curve realigns one way at a time — quote whichever direction the live
	// state accepts and log it.
	cp := sim.StaticExtra.CurveParams
	dir, fullIn, fullOut := cp.rebalanceState(&sim.Extra)
	t.Logf("block=%d dir=%d fullIn=%s fullOut=%s", p.BlockNumber, dir, fullIn, fullOut)
	require.NotZero(t, dir, "live state should accept a fill in one direction")

	if dir == 2 { // deleverage: NECT in
		stableOut, _, ok := cp.deleverageLegsAt(&sim.Extra, fullIn)
		require.True(t, ok)
		net := new(big.Int).Sub(fullIn, stableOut)
		res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: nect, Amount: net},
			TokenOut:      wbtc,
		})
		require.NoError(t, err)
		require.True(t, res.TokenAmountOut.Amount.Sign() > 0)
		t.Logf("deleverage %s NECT net -> %s WBTC sat", net, res.TokenAmountOut.Amount)
	} else { // leverage: WBTC in
		_, volatileIn, ok := sim.Extra.previewTokenAmounts(fullIn, true)
		require.True(t, ok)
		res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: wbtc, Amount: volatileIn},
			TokenOut:      nect,
		})
		require.NoError(t, err)
		require.True(t, res.TokenAmountOut.Amount.Sign() > 0)
		t.Logf("leverage %s WBTC sat -> %s NECT wei", volatileIn, res.TokenAmountOut.Amount)
	}
}
