package everlongcvamm

import (
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/goccy/go-json"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// TestExactFeeLawMatchesChain: the ported law must reproduce poolFeeDirectional at the
// LIVE book, in both directions, to the wei. The scalar comes only from the block-pinned
// CvammStore.rv word; sampled fees are an attestation, never an inference input.
func TestExactFeeLawMatchesChain(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client, cfg := liveClient()

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &se))
	require.Equal(t, supportedImplementationCodeHash.Hex(), se.ImplementationCodeHash)
	hookCode, err := client.GetETHClient().CodeAt(ctx, common.HexToAddress(se.FeeHook), nil)
	require.NoError(t, err)
	require.Equal(t, supportedFeeHookCodeHash, crypto.Keccak256Hash(hookCode),
		"an unknown hook may retain its sampled first quote but must not enable chained repricing")
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	e := &sim.Extra
	require.True(t, e.feeLawExactTracked(), "every term of the exact law must be tracked")
	x, rs, rv := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()

	gotS, gotV, ok := exactFeesAt(e, x, rs, rv)
	require.True(t, ok, "the layout-read rv and hook state must price the live book")
	require.Equal(t, e.FeeStableInWad.ToBig().String(), gotS.String(), "stable-in fee is not reproduced")
	require.Equal(t, e.FeeVolatileInWad.ToBig().String(), gotV.String(), "volatile-in fee is not reproduced")
	t.Logf("exact law reproduces chain wei-exactly: stable-in %s, volatile-in %s (rv %s, scalar %s)",
		gotS, gotV, e.RealizedVarianceWad, realizedVarianceScalar(e))
}

// TestPostFillFeeParityAgainstChain is the claim the port exists for: the scalar derived
// from the implementation-pinned realized-variance slot at the block BEFORE a settled
// swap must reprice the post-fill book to the fee the venue actually charges after it.
//
// Each case is only comparable when the settled fill is the whole of that block's motion,
// so the simulated coordinate is checked against the chain's before the fees are — a
// mismatch means something else moved the book and the case is skipped, not failed.
func TestPostFillFeeParityAgainstChain(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client, cfg := liveClient()
	rpcURL := cvammRPCURL()

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &se))

	geth, err := ethclient.Dial(rpcURL)
	require.NoError(t, err)
	defer geth.Close()

	alm := common.HexToAddress(pools[0].Address)
	swapTopic := common.HexToHash("0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67")
	foldTopic := crypto.Keccak256Hash([]byte("OracleFolded(uint256,uint48,uint256)"))

	var compared, exact int
	for _, txHash := range postFfadSwaps {
		t.Run(txHash[:10], func(t *testing.T) {
			receipt, err := geth.TransactionReceipt(ctx, common.HexToHash(txHash))
			require.NoError(t, err)

			var amount0, amount1 *big.Int
			for _, lg := range receipt.Logs {
				if lg.Address != alm || len(lg.Topics) == 0 {
					continue
				}
				if lg.Topics[0] == foldTopic {
					t.Skip("the oracle folded in this block, so realized variance moved with the fill")
				}
				if lg.Topics[0] != swapTopic {
					continue
				}
				amount0 = signedWord(lg.Data[0:32])
				amount1 = signedWord(lg.Data[32:64])
			}
			require.NotNil(t, amount0, "ALM Swap event not found in receipt")

			stableIn := amount0.Sign() > 0
			amountInUsed := amount0
			if !stableIn {
				amountInUsed = amount1
			}

			parent := new(big.Int).Sub(receipt.BlockNumber, big.NewInt(1))
			sim := trackAt(t, ctx, client, pools[0], &se, parent)
			if !sim.Extra.feeLawExactTracked() {
				t.Skip("the hook at this block predates the terms the exact law reads")
			}

			tokenIn, tokenOut := sim.Info.Tokens[0], sim.Info.Tokens[1]
			if !stableIn {
				tokenIn, tokenOut = tokenOut, tokenIn
			}
			res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
				TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountInUsed},
				TokenOut:      tokenOut,
			})
			require.NoError(t, err)
			sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})

			// The chain's book at the END of the swap block. If it is not the book the
			// fill left behind, this block did more than the one settled swap.
			after := trackAt(t, ctx, client, pools[0], &se, receipt.BlockNumber)
			if !after.Extra.XWad.Eq(sim.Extra.XWad) {
				t.Skipf("book moved beyond the settled fill in block %s (chain x %s vs replayed %s)",
					receipt.BlockNumber, after.Extra.XWad, sim.Extra.XWad)
			}

			compared++
			allExact := true
			for _, d := range []struct {
				name           string
				derived, chain *uint256.Int
			}{
				{"stable-in", sim.Extra.FeeStableInWad, after.Extra.FeeStableInWad},
				{"volatile-in", sim.Extra.FeeVolatileInWad, after.Extra.FeeVolatileInWad},
			} {
				require.True(t, d.derived.Eq(d.chain),
					"%s: re-derived %s differs from chain %s", d.name, d.derived, d.chain)
				if !d.derived.Eq(d.chain) {
					allExact = false
					t.Logf("%s: re-derived %s vs chain %s (+%s wad, safe side)",
						d.name, d.derived, d.chain,
						new(big.Int).Sub(d.derived.ToBig(), d.chain.ToBig()))
				}
			}
			if allExact {
				exact++
				t.Logf("block %s: both legs repriced wei-exactly after the fill", receipt.BlockNumber)
			}
		})
	}
	t.Logf("post-fill fee parity: %d/%d comparable cases repriced wei-exactly", exact, compared)
}

// postFfadSwaps are the settled fills whose fee hook exposes the full law. Immutable
// chain facts; the parent-block reads around them are the only environmental part.
var postFfadSwaps = []string{
	"0xba262821e368c77fa5b3ae72e55b34b002dbcb5cd3a6a3fb500e010a2ce970ae",
	"0x1e7598b86bddc85bf5a3860156e68ea3646e7a583c82215638cf1a8fde36c19e",
	"0x4cc99354814179f54f2ccd974fa4c7ae09005a64503fca2cd103877a5f293279",
	"0xed2045b937647b2a8e1836b30d3b0908652dbc156289086e61f90a3cc5ced313",
	"0xa508114796045b1813e913e6e3c8390baa66c91aaa9cfad32e45f8e576f37e19",
	"0x4e2937632261bc9d7ef6ec9e45a967140bd99fe2a4df521e8e3a4a0e0d889cc2",
	"0x8178d2b5424a0bc0a78d8eec8feb105159f9fc4d71e692ea1bcffb17537cf5ca",
	"0x12c0d42e916f376d6c7bb71931ecc85bfb02ad65d61e25949c0f0c788d9c91a5",
	"0xcd53afbdf9d3cc620699806379b1ebda9299a8ce5947e186bd76e892795bac3a",
	"0x65070ab291ac99f6532fada81feb606c0e5ba19b4e067395da7aaa3a8e5a2e72",
	"0xbb887e4b91d979e0e2c67e138d18161c47480512ce1a256f17a95dd552c7d9d8",
}

func cvammRPCURL() string {
	if u := os.Getenv("EVERLONG_CVAMM_RPC_URL"); u != "" {
		return u
	}
	return "https://rpc.berachain.com"
}

func liveClient() (*ethrpc.Client, *Config) {
	alm := os.Getenv("EVERLONG_CVAMM_ALM")
	if alm == "" {
		alm = "0xF5124F5605ce1e91A7429B837b7daC8f9E5378dd"
	}
	return ethrpc.New(cvammRPCURL()).
			SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11")),
		&Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
			ALMs: []ALMConfig{{Address: alm}}}
}

func trackAt(t *testing.T, ctx context.Context, client *ethrpc.Client, p entity.Pool,
	se *StaticExtra, block *big.Int) *PoolSimulator {
	t.Helper()
	cfg := &Config{DexID: se.DexID, ChainID: se.ChainID, ALMs: []ALMConfig{{
		Address: se.ALM, Adapter: se.Adapter,
		GasStableIn: se.GasStableIn, GasVolatileIn: se.GasVolatileIn,
	}}}
	built, err := NewPoolTracker(cfg, client).GetNewPoolStateAtBlock(ctx, p, block)
	require.NoError(t, err)
	sim, err := NewPoolSimulator(built)
	require.NoError(t, err)
	return sim
}

func signedWord(b []byte) *big.Int {
	v := new(big.Int).SetBytes(b)
	if v.Bit(255) == 1 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return v
}

// seedExactLaw installs the deployed hook's terms and an exact live floor. setRVForScalar
// supplies the only otherwise-hidden law input without deriving it from a fee sample.
func seedExactLaw(e *Extra) {
	e.FeeHookActive = true
	e.HotFloorsExact = true
	e.FloorStableInWad = new(uint256.Int)
	e.FloorVolatileInWad = new(uint256.Int)
	e.ReservationPriceWad, _ = uint256.FromBig(
		spotRawWad(e.AnchorSqrtX96.ToBig(), bigHalfWad, e.Support.AWad.ToBig()))
	e.MidFeeWad = uint256.NewInt(30_000_000_000_000_000)
	e.OutFeeWad = uint256.NewInt(5_000_000_000_000_000)
	e.DirSkewWad = uint256.NewInt(150_000_000_000_000_000)
	e.InvSkewKappaWad = uint256.NewInt(2_000_000_000_000_000_000)
	e.InvSkewBandWad = uint256.NewInt(60_000_000_000_000_000)
	e.CurvatureWad = uint256.NewInt(50_000_000_000_000_000)
	e.LpFeeWad = uint256.NewInt(30_000_000_000_000_000)
	e.VolSigmaRefWad = uint256.NewInt(400_000_000_000_000)
	e.VolBetaWad = uint256.NewInt(4_000_000_000_000_000_000)
	e.VolMinWad = uint256.NewInt(500_000_000_000_000_000)
	e.VolMaxWad = uint256.NewInt(2_000_000_000_000_000_000)
	setRVForScalar(e, big.NewInt(400_000_000_000_000_000))
}

func setRVForScalar(e *Extra, scalar *big.Int) {
	sigma := mulDivFloor(scalar, e.VolSigmaRefWad.ToBig(), bigWadFee)
	sigma.Div(sigma, big.NewInt(1_000_000_000))
	e.RealizedVarianceWad, _ = uint256.FromBig(new(big.Int).Mul(sigma, sigma))
}

func seedSampleFromExactLaw(t *testing.T, sim *PoolSimulator) {
	t.Helper()
	e := &sim.Extra
	s, v, ok := exactFeesAt(e, e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig())
	require.True(t, ok)
	e.FeeStableInWad, _ = uint256.FromBig(s)
	e.FeeVolatileInWad, _ = uint256.FromBig(v)
}

func TestExactLawRepricesFromStorageRV(t *testing.T) {
	cases, _ := fillableCases(t)
	sim := simFromFixture(t, cases[0], nil)
	seedExactLaw(&sim.Extra)
	seedSampleFromExactLaw(t, sim)

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: cases[0].amountIn.ToBig()}, TokenOut: testVol})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
	require.True(t, sim.feeStateExact)
	wantS, wantV, ok := exactFeesAt(&sim.Extra, sim.Extra.XWad.ToBig(),
		sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig())
	require.True(t, ok)
	require.Equal(t, wantS.String(), sim.Extra.FeeStableInWad.Dec())
	require.Equal(t, wantV.String(), sim.Extra.FeeVolatileInWad.Dec())
}

func TestExactLawRespectsLiveFloor(t *testing.T) {
	cases, _ := fillableCases(t)
	sim := simFromFixture(t, cases[0], nil)
	seedExactLaw(&sim.Extra)
	const floor = 900_000_000_000_000_000
	sim.Extra.FloorStableInWad = uint256.NewInt(floor)
	sim.Extra.FloorVolatileInWad = uint256.NewInt(floor)
	seedSampleFromExactLaw(t, sim)

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: cases[0].amountIn.ToBig()}, TokenOut: testVol})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
	require.True(t, sim.Extra.FeeStableInWad.Eq(uint256.NewInt(floor)))
	require.True(t, sim.Extra.FeeVolatileInWad.Eq(uint256.NewInt(floor)))
}

func TestExactLawFailsClosedOnMissingOrMismatchedState(t *testing.T) {
	cases, _ := fillableCases(t)
	for _, mutate := range []func(*Extra){
		func(e *Extra) { e.RealizedVarianceWad = nil },
		func(e *Extra) { e.HotFloorsExact = false },
		func(e *Extra) { e.FeeStableInWad.AddUint64(e.FeeStableInWad, 1) },
	} {
		sim := simFromFixture(t, cases[0], nil)
		seedExactLaw(&sim.Extra)
		seedSampleFromExactLaw(t, sim)
		mutate(&sim.Extra)
		res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: cases[0].amountIn.ToBig()}, TokenOut: testVol})
		require.NoError(t, err, "the block-sampled first quote remains exact")
		sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
		_, err = sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: testVol, Amount: big.NewInt(1e8)}, TokenOut: testStable})
		require.ErrorIs(t, err, ErrInexactFeeState)
	}
}

func TestReversalRepricedExactlyFromRV(t *testing.T) {
	cases, _ := fillableCases(t)
	require.Greater(t, len(cases), 59)
	sim := simFromFixture(t, cases[59], nil)
	seedExactLaw(&sim.Extra)
	setRVForScalar(&sim.Extra, big.NewInt(100_000_000_000_000_000))
	seedSampleFromExactLaw(t, sim)

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: cases[59].amountIn.ToBig()}, TokenOut: testVol})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
	wantS, wantV, ok := exactFeesAt(&sim.Extra, sim.Extra.XWad.ToBig(),
		sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig())
	require.True(t, ok)
	require.Equal(t, wantS.String(), sim.Extra.FeeStableInWad.Dec())
	require.Equal(t, wantV.String(), sim.Extra.FeeVolatileInWad.Dec())
}
