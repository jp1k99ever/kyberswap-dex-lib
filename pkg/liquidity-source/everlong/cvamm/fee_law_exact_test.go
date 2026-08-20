package everlongcvamm

import (
	"bytes"
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/KyberNetwork/msgpack/v5"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/goccy/go-json"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestExactFeeLawMatchesChain: the ported law must reproduce poolFeeDirectional at the
// LIVE book, in both directions, to the wei. That is the whole premise — the vol scalar
// is solved from those two samples, so a law that does not reproduce them has solved for
// nothing, and reSampleFeesExact must decline rather than quote it.
func TestExactFeeLawMatchesChain(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client, cfg := liveClient()

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	e := &sim.Extra
	require.True(t, e.feeLawExactTracked(), "every term of the exact law must be tracked")
	x, rs, rv := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()

	c := newFeeCtx(e, x, rs, rv)
	require.NotNil(t, c, "the live book must be inside the law's domain")
	s, ok := solveVolScalar(e, c)
	require.True(t, ok, "no vol scalar reproduces the sampled fees — the law has moved")

	// Exact in both directions, or the solve landed on a floor-dominated sample and the
	// scalar is only bounded. Either is sound, but only the first proves the port.
	gotS, gotV := c.raw(e, s, true), c.raw(e, s, false)
	require.Equal(t, e.FeeStableInWad.ToBig().String(), gotS.String(), "stable-in fee is not reproduced")
	require.Equal(t, e.FeeVolatileInWad.ToBig().String(), gotV.String(), "volatile-in fee is not reproduced")
	t.Logf("exact law reproduces chain wei-exactly: stable-in %s, volatile-in %s (scalar %s); "+
		"the saturated bound would have quoted %s / %s",
		gotS, gotV, s, feeUpperBoundWad(e, x, rs, rv, true), feeUpperBoundWad(e, x, rs, rv, false))
}

// TestPostFillFeeParityAgainstChain is the claim the port exists for: a scalar solved at
// the block BEFORE a settled swap must reprice the post-fill book to the fee the venue
// actually charges after it.
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
				// The safety claim: a re-derived fee BELOW the venue's would hand the
				// router a fill it will not honour.
				require.False(t, d.derived.Lt(d.chain),
					"%s: re-derived %s is BELOW chain %s", d.name, d.derived, d.chain)
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
		&Config{DexID: DexType, ALMs: []ALMConfig{{Address: alm}}}
}

func trackAt(t *testing.T, ctx context.Context, client *ethrpc.Client, p entity.Pool,
	se *StaticExtra, block *big.Int) *PoolSimulator {
	t.Helper()
	rd := newRPCState()
	req := client.NewRequest().SetContext(ctx).SetBlockNumber(block)
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, p.Address, se, rd)
	_, err := req.Aggregate()
	require.NoError(t, err)
	built, err := buildPoolState(p, rd, block)
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

// seedExactLaw installs the deployed Berachain hook's terms, with the anchor derived from
// the fixture's own sqrt price the way the ALM derives it — a mismatched anchor reads as a
// vast dislocation and pins the vol term to its clamp, which is not the regime under test.
func seedExactLaw(e *Extra) {
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
}

// TestExactLawPricesUnderTheSaturatedBound is the regression the port exists for. Off the
// cap the bound substitutes MidFee for a base the venue prices lower, so a revisit was
// quoted the whole gap too wide. The re-derived fee must sit under that bound and still
// never fall below what the venue would charge at the scalar the sample was drawn from.
func TestExactLawPricesUnderTheSaturatedBound(t *testing.T) {
	stableInCases, _ := fillableCases(t)
	require.NotEmpty(t, stableInCases)
	sim := simFromFixture(t, stableInCases[0], nil)

	e := &sim.Extra
	seedExactLaw(e)
	require.True(t, e.feeLawExactTracked())

	x, rs, rv := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	pre := newFeeCtx(e, x, rs, rv)
	require.NotNil(t, pre)
	// Low enough that the base stays OFF the cap at this fixture's dislocation, which is
	// the only regime where the two derivations differ.
	trueScalar := big.NewInt(400_000_000_000_000_000)
	e.FeeStableInWad, _ = uint256.FromBig(pre.raw(e, trueScalar, true))
	e.FeeVolatileInWad, _ = uint256.FromBig(pre.raw(e, trueScalar, false))
	require.Positive(t, feeUpperBoundWad(e, x, rs, rv, true).Cmp(e.FeeStableInWad.ToBig()),
		"the fixture must be OFF the cap or this proves nothing")

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: stableInCases[0].amountIn.ToBig()},
		TokenOut:      testVol,
	})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})

	postX, postS, postV := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	post := newFeeCtx(e, postX, postS, postV)
	require.NotNil(t, post)
	require.False(t, e.FeeStableInWad.Eq(e.FeeVolatileInWad), "the legs must stay directional")

	for _, d := range []struct {
		name     string
		stableIn bool
		got      *uint256.Int
	}{
		{"stable-in", true, e.FeeStableInWad}, {"volatile-in", false, e.FeeVolatileInWad},
	} {
		truth := post.raw(e, trueScalar, d.stableIn)
		bound := feeUpperBoundWad(e, postX, postS, postV, d.stableIn)
		require.False(t, d.got.ToBig().Cmp(truth) < 0,
			"%s: re-derived %s is BELOW the venue's %s", d.name, d.got, truth)
		require.False(t, d.got.ToBig().Cmp(bound) > 0,
			"%s: re-derived %s is WIDER than the saturated bound %s", d.name, d.got, bound)
		t.Logf("%s: re-derived %s vs venue %s (bound would have quoted %s)", d.name, d.got, truth, bound)
	}
}

// TestExactLawRespectsHookFloor: the floor is never read, it is inferred from the sample
// that already carries it. A sample the law cannot reach on its own is a floor, and both
// legs must stay at it after the fill rather than dropping to the unfloored law.
func TestExactLawRespectsHookFloor(t *testing.T) {
	stableInCases, _ := fillableCases(t)
	require.NotEmpty(t, stableInCases)
	sim := simFromFixture(t, stableInCases[0], nil)

	e := &sim.Extra
	seedExactLaw(e)
	const floor = 900_000_000_000_000_000
	e.FeeStableInWad = uint256.NewInt(floor)
	e.FeeVolatileInWad = uint256.NewInt(floor)
	e.FloorStableInWad = uint256.NewInt(floor)
	e.FloorVolatileInWad = uint256.NewInt(floor)

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: stableInCases[0].amountIn.ToBig()},
		TokenOut:      testVol,
	})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})

	require.True(t, e.FeeStableInWad.Eq(uint256.NewInt(floor)),
		"a floored sample must not be repriced under the floor")
	require.True(t, e.FeeVolatileInWad.Eq(uint256.NewInt(floor)))
}

// TestExactLawDeclinesWhenSampleIsUnreachable: a sample the modelled law cannot produce at
// ANY scalar means the law no longer describes the venue, and the solve must refuse rather
// than bracket nonsense. The saturated bound still stands behind it.
func TestExactLawDeclinesWhenSampleIsUnreachable(t *testing.T) {
	stableInCases, _ := fillableCases(t)
	require.NotEmpty(t, stableInCases)
	sim := simFromFixture(t, stableInCases[0], nil)

	e := &sim.Extra
	seedExactLaw(e)
	x, rs, rv := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	// Below the imbalanced fee, which is the law's floor at every scalar.
	e.FeeStableInWad = uint256.NewInt(1)
	e.FeeVolatileInWad = uint256.NewInt(1)

	_, _, ok := reSampleFeesExact(e, &feeSolve{}, x, rs, rv, x, rs, rv)
	require.False(t, ok, "an unreachable sample must not be solved for")
	_, _, ok = reSampleFeesBest(e, &feeSolve{}, x, rs, rv, x, rs, rv)
	require.True(t, ok, "the saturated bound must still stand behind the exact law")
}

// TestSolveCacheSurvivesTheServiceHop: the solved scalar and the block's own fee samples
// live in UNEXPORTED simulator state, so they cross pool-service -> router-service only
// because pkg/msgpack sets IncludeUnexported(true). Dropped there, the next hop would
// re-solve from an already re-derived fee and ratchet the haircut up instead of repricing.
func TestSolveCacheSurvivesTheServiceHop(t *testing.T) {
	cases, _ := fillableCases(t)
	require.NotEmpty(t, cases)
	sim := simFromFixture(t, cases[0], nil)
	seedExactLaw(&sim.Extra)

	x, rs, rv := sim.Extra.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	c := newFeeCtx(&sim.Extra, x, rs, rv)
	require.NotNil(t, c)
	sim.Extra.FeeStableInWad, _ = uint256.FromBig(c.raw(&sim.Extra, big.NewInt(400_000_000_000_000_000), true))
	sim.Extra.FeeVolatileInWad, _ = uint256.FromBig(c.raw(&sim.Extra, big.NewInt(400_000_000_000_000_000), false))
	require.True(t, sim.feeSolve.solve(&sim.Extra, x, rs, rv), "the fixture must solve")

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.IncludeUnexported(true)
	enc.SetForceAsArray(true)
	require.NoError(t, enc.Encode(sim))
	dec := msgpack.NewDecoder(&buf)
	dec.IncludeUnexported(true)
	var decoded PoolSimulator
	require.NoError(t, dec.Decode(&decoded))

	require.True(t, decoded.feeSolve.tried, "the solve must not be re-attempted after the hop")
	require.NotNil(t, decoded.feeSolve.scalar)
	require.Equal(t, sim.feeSolve.scalar.String(), decoded.feeSolve.scalar.String())

	// And the re-derivation must land on the same fees on both sides of the hop.
	for _, s := range []*PoolSimulator{sim, &decoded} {
		res, err := s.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: cases[0].amountIn.ToBig()},
			TokenOut:      testVol,
		})
		require.NoError(t, err)
		s.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
	}
	require.Equal(t, sim.Extra.FeeStableInWad.Dec(), decoded.Extra.FeeStableInWad.Dec())
	require.Equal(t, sim.Extra.FeeVolatileInWad.Dec(), decoded.Extra.FeeVolatileInWad.Dec())
}

// TestReversalRepricedExactly is the regression the port exists for, and the one the
// floor bound nearly cost: a fill that flips which direction restores the price must
// reprice the REVERSING leg to what the venue would charge, not carry forward the higher
// pre-fill sample it had for the opposite direction. Carrying it forward re-imposed the
// fee the fill had just moved away from, which is exactly the revisit the reviewer
// measured at 58-59 bps.
func TestReversalRepricedExactly(t *testing.T) {
	stableInCases, _ := fillableCases(t)
	require.Greater(t, len(stableInCases), 59)
	// This case is chosen to make the assertion below meaningful in three ways at once:
	// the fill carries the price ACROSS the anchor (so the restoring leg swaps over), the
	// pre-fill sample sits off the cap (so it identifies the scalar rather than merely
	// bounding it), and the post-fill book is off the cap too (so the saturated bound is
	// loose and cannot mask a conservative re-derivation through the min).
	c := stableInCases[59]
	sim := simFromFixture(t, c, nil)

	e := &sim.Extra
	seedExactLaw(e)
	// Saturated-rate floors well below anything the law produces here, so the floor is
	// present but never the binding term — the reversal must be priced by the law.
	e.FloorStableInWad = uint256.NewInt(1)
	e.FloorVolatileInWad = uint256.NewInt(1)

	x, rs, rv := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	pre := newFeeCtx(e, x, rs, rv)
	require.NotNil(t, pre)
	// Low enough that the post-fill book stays OFF the cap, so the saturated bound is
	// loose there too and cannot mask a conservative re-derivation.
	trueScalar := big.NewInt(100_000_000_000_000_000)
	e.FeeStableInWad, _ = uint256.FromBig(pre.raw(e, trueScalar, true))
	e.FeeVolatileInWad, _ = uint256.FromBig(pre.raw(e, trueScalar, false))

	anchor := e.ReservationPriceWad.ToBig()
	aWad := e.Support.AWad.ToBig()
	preBelow := spotRawWad(e.AnchorSqrtX96.ToBig(), x, aWad).Cmp(anchor) < 0

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: c.amountIn.ToBig()},
		TokenOut:      testVol,
	})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})

	postX, postS, postV := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	post := newFeeCtx(e, postX, postS, postV)
	require.NotNil(t, post)
	postBelow := spotRawWad(e.AnchorSqrtX96.ToBig(), postX, aWad).Cmp(anchor) < 0
	require.NotEqual(t, preBelow, postBelow, "the fill must cross the anchor for this to mean anything")

	// The reversing leg is the one whose fee the fill LOWERS; it is the one a revisit pays.
	for _, d := range []struct {
		name     string
		stableIn bool
		got      *uint256.Int
	}{
		{"stable-in", true, e.FeeStableInWad}, {"volatile-in", false, e.FeeVolatileInWad},
	} {
		truth := post.raw(e, trueScalar, d.stableIn)
		drift := new(big.Int).Sub(d.got.ToBig(), truth)
		// Never under the venue's fee, and within a wei or two of it: the scalar is solved
		// as the largest the sample admits, so the fee it produces is the venue's rounded
		// up, not a conservative substitute. Before the floor bound was tightened, the
		// reversing leg came back a whole directional step high instead.
		require.False(t, drift.Sign() < 0,
			"%s: repriced %s BELOW the venue's %s", d.name, d.got, truth)
		require.True(t, drift.Cmp(big.NewInt(2)) <= 0,
			"%s: repriced %s vs the venue's %s (%s wei high)", d.name, d.got, truth, drift)
		// And the saturated bound must genuinely be loose here, or the min() with it
		// would mask a conservative re-derivation and this would prove nothing.
		require.Positive(t, feeUpperBoundWad(e, postX, postS, postV, d.stableIn).Cmp(truth),
			"%s: the bound is tight here, so the exact law is not under test", d.name)
	}
}
