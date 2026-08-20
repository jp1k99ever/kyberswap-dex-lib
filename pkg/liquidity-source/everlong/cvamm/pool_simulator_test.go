package everlongcvamm

import (
	"math/big"
	"testing"

	"github.com/goccy/go-json"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/util/big256"
)

const (
	testALM     = "0x00000000000000000000000000000000000cva77"
	testStable  = "0x0000000000000000000000000000000000000001"
	testVol     = "0x0000000000000000000000000000000000000002"
	testFeeSIn  = 400000000000000  // 4 bps, stable-in
	testFeeVIn  = 1100000000000000 // 11 bps, volatile-in
	hugeReserve = "100000000000000000000000000000000000000"
)

// simFromFixture builds a PoolSimulator on a fixture state. Reserves default to
// effectively-unbounded so the solvency clamp does not bind unless a test wants it to.
func simFromFixture(t *testing.T, c fixtureCase, reserves entity.PoolReserves) *PoolSimulator {
	t.Helper()
	extraBytes, err := json.Marshal(Extra{
		Support:          c.sup,
		XWad:             c.x,
		AnchorSqrtX96:    c.anchor,
		Kappa:            c.kappa,
		FeeStableInWad:   uint256.NewInt(testFeeSIn),
		FeeVolatileInWad: uint256.NewInt(testFeeVIn),
	})
	require.NoError(t, err)
	if reserves == nil {
		reserves = entity.PoolReserves{hugeReserve, hugeReserve}
	}
	sim, err := NewPoolSimulator(entity.Pool{
		Address:  testALM,
		Exchange: "everlong-cvamm",
		Type:     DexType,
		Tokens: []*entity.PoolToken{
			{Address: testStable, Swappable: true},
			{Address: testVol, Swappable: true},
		},
		Reserves:    reserves,
		StaticExtra: "{}",
		Extra:       string(extraBytes),
	})
	require.NoError(t, err)
	return sim
}

// fillableCases picks fixture cases that actually fill (used > 0, gross > 0), keyed by
// direction, so behavior tests exercise real fills on chain-derived ground truth.
func fillableCases(t *testing.T) (stableIn, volatileIn []fixtureCase) {
	t.Helper()
	swaps, _ := loadFixtures(t)
	for _, c := range swaps {
		var used uint256.Int
		used.Sub(c.amountIn, c.unspent)
		if used.IsZero() || c.gross.IsZero() {
			continue
		}
		if c.stableIn {
			stableIn = append(stableIn, c)
		} else {
			volatileIn = append(volatileIn, c)
		}
	}
	require.NotEmpty(t, stableIn)
	require.NotEmpty(t, volatileIn)
	return
}

func calc(sim *PoolSimulator, c fixtureCase) (*pool.CalcAmountOutResult, error) {
	tokenIn, tokenOut := testStable, testVol
	if !c.stableIn {
		tokenIn, tokenOut = testVol, testStable
	}
	return sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: c.amountIn.ToBig()},
		TokenOut:      tokenOut,
	})
}

// TestCalcAmountOutMatchesFixtures replays every fillable fixture case through the
// simulator and checks net-out, fee, and remaining input against the bytecode-derived
// gross/unspent plus the venue's exact fee pipeline (floored fee, net rounds up).
func TestCalcAmountOutMatchesFixtures(t *testing.T) {
	sIn, vIn := fillableCases(t)
	for _, cases := range [][]fixtureCase{sIn, vIn} {
		for i, c := range cases {
			sim := simFromFixture(t, c, nil)
			res, err := calc(sim, c)
			require.NoError(t, err, "case %d", i)

			feeWad := uint256.NewInt(testFeeVIn)
			if c.stableIn {
				feeWad = uint256.NewInt(testFeeSIn)
			}
			var fee, netOut uint256.Int
			big256.MulDivDown(&fee, c.gross, feeWad, uWad)
			netOut.Sub(c.gross, &fee)

			require.Equal(t, netOut.Dec(), res.TokenAmountOut.Amount.String(), "case %d net", i)
			require.Equal(t, fee.Dec(), res.Fee.Amount.String(), "case %d fee", i)
			require.Equal(t, c.unspent.Dec(), res.RemainingTokenAmountIn.Amount.String(),
				"case %d remaining", i)

			si, ok := res.SwapInfo.(SwapInfo)
			require.True(t, ok)
			require.Equal(t, c.xAfter.Dec(), si.XAfter.Dec(), "case %d xAfter", i)
			require.Equal(t, c.gross.Dec(), si.GrossOut.Dec(), "case %d gross", i)
		}
	}
}

// TestSolvencyClamp: when the accounted output reserve is below the curve's gross quote,
// the payout is clamped to the reserve — exactly as CvammSwapLib.execute does.
func TestSolvencyClamp(t *testing.T) {
	sIn, _ := fillableCases(t)
	// stable in -> volatile out; pick a case with a sizeable gross and clamp the
	// volatile reserve below it.
	var c fixtureCase
	for _, cand := range sIn {
		if cand.gross.GtUint64(1_000_000) {
			c = cand
			break
		}
	}
	require.NotNil(t, c.gross, "no fixture with a sizeable gross")
	var clamped uint256.Int
	clamped.Rsh(c.gross, 1) // half the curve's gross
	sim := simFromFixture(t, c, entity.PoolReserves{hugeReserve, clamped.Dec()})
	res, err := calc(sim, c)
	require.NoError(t, err)

	var fee, netOut uint256.Int
	big256.MulDivDown(&fee, &clamped, uint256.NewInt(testFeeSIn), uWad)
	netOut.Sub(&clamped, &fee)
	require.Equal(t, netOut.Dec(), res.TokenAmountOut.Amount.String())

	// The zero-reserve book refuses the fill outright.
	sim = simFromFixture(t, c, entity.PoolReserves{hugeReserve, "0"})
	_, err = calc(sim, c)
	require.ErrorIs(t, err, ErrSwapExhausted)
}

// TestDustInputFullyUnspent: an input below normalized resolution (~kappa/WAD in the
// leg's own units) buys nothing and must be rejected, not quoted as a free fill.
func TestDustInputFullyUnspent(t *testing.T) {
	swaps, _ := loadFixtures(t)
	found := false
	for _, c := range swaps {
		if !c.unspent.Eq(c.amountIn) || c.amountIn.IsZero() {
			continue
		}
		found = true
		sim := simFromFixture(t, c, nil)
		_, err := calc(sim, c)
		require.ErrorIs(t, err, ErrSwapExhausted)
	}
	require.True(t, found, "fixtures must include fully-unspent dust cases")
}

func TestPausedAndRetracted(t *testing.T) {
	sIn, _ := fillableCases(t)
	c := sIn[0]

	sim := simFromFixture(t, c, nil)
	sim.Extra.Paused = true
	_, err := calc(sim, c)
	require.ErrorIs(t, err, ErrPaused)

	sim = simFromFixture(t, c, nil)
	sim.Extra.Kappa = uint256.NewInt(0)
	_, err = calc(sim, c)
	require.ErrorIs(t, err, ErrRetractedBook)
}

func TestExactOutRejected(t *testing.T) {
	sIn, _ := fillableCases(t)
	sim := simFromFixture(t, sIn[0], nil)
	_, err := sim.CalcAmountIn(pool.CalcAmountInParams{})
	require.ErrorIs(t, err, ErrExactOutNotSupported)
}

// TestUpdateBalanceAndClone: UpdateBalance replays the SwapInfo transition (input leg
// +used, output leg -gross, coordinate -> xAfter), quoting is deterministic, and a
// clone taken before the update is not affected by it.
func TestUpdateBalanceAndClone(t *testing.T) {
	sIn, _ := fillableCases(t)
	c := sIn[0]
	sim := simFromFixture(t, c, nil)

	res1, err := calc(sim, c)
	require.NoError(t, err)
	res2, err := calc(sim, c)
	require.NoError(t, err)
	require.Equal(t, res1.TokenAmountOut.Amount.String(), res2.TokenAmountOut.Amount.String(),
		"CalcAmountOut must be pure and deterministic")

	clone := sim.CloneState().(*PoolSimulator)

	si := res1.SwapInfo.(SwapInfo)
	sim.UpdateBalance(pool.UpdateBalanceParams{
		TokenAmountIn:  pool.TokenAmount{Token: testStable, Amount: c.amountIn.ToBig()},
		TokenAmountOut: *res1.TokenAmountOut,
		Fee:            *res1.Fee,
		SwapInfo:       si,
	})

	require.Equal(t, si.XAfter.Dec(), sim.Extra.XWad.Dec())
	var wantStable uint256.Int
	base, _ := uint256.FromDecimal(hugeReserve)
	wantStable.Add(base, si.AmountInUsed)
	require.Equal(t, wantStable.Dec(), sim.reserveStable.Dec())
	var wantVol uint256.Int
	wantVol.Sub(base, si.GrossOut)
	require.Equal(t, wantVol.Dec(), sim.reserveVolatile.Dec())
	require.Equal(t, wantStable.ToBig().String(), sim.Info.Reserves[0].String())
	require.Equal(t, wantVol.ToBig().String(), sim.Info.Reserves[1].String())

	// The clone still prices from the pre-update state.
	require.Equal(t, c.x.Dec(), clone.Extra.XWad.Dec())
	resClone, err := calc(clone, c)
	require.NoError(t, err)
	require.Equal(t, res1.TokenAmountOut.Amount.String(), resClone.TokenAmountOut.Amount.String())

	// And updating the clone does not touch the (already updated) original.
	simX := sim.Extra.XWad.Dec()
	clone.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: resClone.SwapInfo.(SwapInfo)})
	require.Equal(t, simX, sim.Extra.XWad.Dec())
}

func TestMetaAndApproval(t *testing.T) {
	sIn, _ := fillableCases(t)
	sim := simFromFixture(t, sIn[0], nil)
	meta, ok := sim.GetMetaInfo(testStable, testVol).(PoolMeta)
	require.True(t, ok)
	require.Equal(t, testALM, meta.ALM)
	require.Equal(t, testALM, sim.GetApprovalAddress(testStable, testVol))
}

// TestReversalFallsBackWhenFeeLawUntracked: without the hook terms the post-fill fee is
// unknowable, so a revisit takes the worse sampled fee — understating, never overstating.
func TestReversalFallsBackWhenFeeLawUntracked(t *testing.T) {
	stableIn, _ := fillableCases(t)
	require.NotEmpty(t, stableIn)
	sim := simFromFixture(t, stableIn[0], nil)

	worseBefore := sim.Extra.FeeStableInWad
	if sim.Extra.FeeVolatileInWad.Gt(worseBefore) {
		worseBefore = sim.Extra.FeeVolatileInWad
	}
	require.False(t, sim.Extra.FeeStableInWad.Eq(sim.Extra.FeeVolatileInWad),
		"the fixture must have a directional spread for this to mean anything")

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: stableIn[0].amountIn.ToBig()},
		TokenOut:      testVol,
	})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})

	require.True(t, sim.Extra.FeeStableInWad.Eq(worseBefore), "both legs take the worse fee")
	require.True(t, sim.Extra.FeeVolatileInWad.Eq(worseBefore), "including the reversal leg")
}

// TestReversalRederivedInSaturatedRegime: with the hook terms tracked and the sample
// reproduced by MidFee * multiplier, the post-fill fees are re-derived per direction
// instead of both collapsing to the worse one — which is the whole directional spread.
func TestReversalRederivedInSaturatedRegime(t *testing.T) {
	stableInCases, _ := fillableCases(t)
	require.NotEmpty(t, stableInCases)
	sim := simFromFixture(t, stableInCases[0], nil)

	e := &sim.Extra
	e.ReservationPriceWad = uint256.NewInt(1e18)
	e.MidFeeWad = uint256.NewInt(30_000_000_000_000_000)   // 3%
	e.DirSkewWad = uint256.NewInt(150_000_000_000_000_000) // 0.15
	e.InvSkewKappaWad = uint256.NewInt(2_000_000_000_000_000_000)
	e.InvSkewBandWad = uint256.NewInt(60_000_000_000_000_000)
	e.CurvatureWad = uint256.NewInt(50_000_000_000_000_000)
	e.LpFeeWad = uint256.NewInt(30_000_000_000_000_000)
	require.True(t, e.feeLawTracked())

	// Seed the sample from the law itself so the parity guard admits the re-derivation.
	x, rs, rv := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	fs, fv := feeUpperBoundWad(e, x, rs, rv, true), feeUpperBoundWad(e, x, rs, rv, false)
	require.NotNil(t, fs)
	require.NotEqual(t, fs.String(), fv.String(), "the directions must differ pre-fill")
	e.FeeStableInWad, _ = uint256.FromBig(fs)
	e.FeeVolatileInWad, _ = uint256.FromBig(fv)

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: stableInCases[0].amountIn.ToBig()},
		TokenOut:      testVol,
	})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})

	require.False(t, e.FeeStableInWad.Eq(e.FeeVolatileInWad),
		"the legs must stay distinct — collapsing them is the 90 bps the fold cost")
	wantS := feeUpperBoundWad(e, e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig(), true)
	wantV := feeUpperBoundWad(e, e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig(), false)
	require.Zero(t, e.FeeStableInWad.ToBig().Cmp(wantS))
	require.Zero(t, e.FeeVolatileInWad.ToBig().Cmp(wantV))
}

// TestReversalRespectsHookFloor: the hook floor is applied last on-chain and can exceed
// the multiplier term, so the bound has to carry it. A floor above anything the law can
// produce must raise BOTH legs to the floor rather than price under it.
func TestReversalRespectsHookFloor(t *testing.T) {
	stableInCases, _ := fillableCases(t)
	require.NotEmpty(t, stableInCases)
	sim := simFromFixture(t, stableInCases[0], nil)

	e := &sim.Extra
	e.ReservationPriceWad = uint256.NewInt(1e18)
	e.MidFeeWad = uint256.NewInt(30_000_000_000_000_000)
	e.DirSkewWad = uint256.NewInt(150_000_000_000_000_000)
	e.InvSkewKappaWad = uint256.NewInt(2_000_000_000_000_000_000)
	e.InvSkewBandWad = uint256.NewInt(60_000_000_000_000_000)
	e.CurvatureWad = uint256.NewInt(50_000_000_000_000_000)
	e.LpFeeWad = uint256.NewInt(30_000_000_000_000_000)

	x, rs, rv := e.XWad.ToBig(), sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	fs, fv := feeUpperBoundWad(e, x, rs, rv, true), feeUpperBoundWad(e, x, rs, rv, false)
	e.FeeStableInWad, _ = uint256.FromBig(fs)
	e.FeeVolatileInWad, _ = uint256.FromBig(fv)
	// A floor above anything the law can produce forces the decline.
	e.FloorStableInWad = uint256.NewInt(900_000_000_000_000_000)
	e.FloorVolatileInWad = uint256.NewInt(900_000_000_000_000_000)

	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: stableInCases[0].amountIn.ToBig()},
		TokenOut:      testVol,
	})
	require.NoError(t, err)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})

	require.True(t, e.FeeStableInWad.Eq(uint256.NewInt(900_000_000_000_000_000)),
		"a dominating floor must raise the leg to it, never price under it")
	require.True(t, e.FeeVolatileInWad.Eq(uint256.NewInt(900_000_000_000_000_000)))
}

// TestApplyLiquidityDeltaAtBoundaries exercises the reverse-coupling scalar where it is
// most likely to misbehave: a burn that nearly empties the book, a burn past the whole
// supply, and a mint that multiplies it. The scalar must keep the curve's shape (x,
// anchor, support untouched), never produce a negative or overflowing book, and leave the
// sim quoting within its own reserves.
func TestApplyLiquidityDeltaAtBoundaries(t *testing.T) {
	stableInCases, _ := fillableCases(t)
	require.NotEmpty(t, stableInCases)

	supply := big.NewInt(1_000_000)
	for _, c := range []struct {
		name    string
		delta   *big.Int
		refused bool // a supply that would go negative must be refused outright
	}{
		{"burn all but one share", new(big.Int).Neg(big.NewInt(999_999)), false},
		{"burn the entire supply", new(big.Int).Neg(new(big.Int).Set(supply)), false},
		{"burn past the supply", new(big.Int).Neg(big.NewInt(1_000_001)), true},
		{"mint 1000x the book", new(big.Int).Mul(supply, big.NewInt(1000)), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			sim := simFromFixture(t, stableInCases[0], nil)
			x0 := sim.Extra.XWad.Clone()
			anchor0 := sim.Extra.AnchorSqrtX96.Clone()
			support0 := sim.Extra.Support

			rs0, rv0, k0 := sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig(), sim.Extra.Kappa.ToBig()

			dS, dV := sim.ApplyLiquidityDelta(c.delta, supply)
			if c.refused {
				require.Nil(t, dS, "a supply that would go negative must be refused")
				require.True(t, sim.reserveStable.Eq(uint256.MustFromBig(rs0)), "a refusal must not mutate")
				return
			}
			require.NotNil(t, dS)
			require.NotNil(t, dV)

			// The property the scalar actually claims: reserves and kappa all scale by
			// supplyAfter/supplyBefore, so the curve keeps its shape.
			after := new(big.Int).Add(supply, c.delta)
			for _, leg := range []struct {
				name     string
				was, now *big.Int
			}{
				{"reserveStable", rs0, sim.reserveStable.ToBig()},
				{"reserveVolatile", rv0, sim.reserveVolatile.ToBig()},
				{"kappa", k0, sim.Extra.Kappa.ToBig()},
			} {
				want := new(big.Int).Quo(new(big.Int).Mul(leg.was, after), supply)
				require.Zero(t, leg.now.Cmp(want),
					"%s must scale pro rata: got %s want %s", leg.name, leg.now, want)
			}

			require.True(t, sim.Extra.XWad.Eq(x0), "x must not move")
			require.True(t, sim.Extra.AnchorSqrtX96.Eq(anchor0), "anchor must not move")
			require.Equal(t, support0, sim.Extra.Support, "support must not move")
			require.False(t, sim.reserveStable.Lt(uint256.NewInt(0)))
			require.GreaterOrEqual(t, sim.Info.Reserves[0].Sign(), 0)
			require.GreaterOrEqual(t, sim.Info.Reserves[1].Sign(), 0)

			// A quote on the rescaled book must stay inside it, or fail cleanly.
			res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
				TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: big.NewInt(1e18)},
				TokenOut:      testVol,
			})
			if err != nil {
				return
			}
			require.LessOrEqual(t, res.TokenAmountOut.Amount.Cmp(sim.reserveVolatile.ToBig()), 0,
				"a quote must never promise more than the rescaled book holds")
		})
	}
}

// TestFeeNeverExceedsWad: on-chain a hook floor above WAD is DISCARDED (the law returns
// the base fee), and a fee at or above WAD would underflow `gross - fee` into a wrapped
// quote. Both the bound and the swap path must refuse to go there.
func TestFeeNeverExceedsWad(t *testing.T) {
	stableInCases, _ := fillableCases(t)
	require.NotEmpty(t, stableInCases)
	sim := simFromFixture(t, stableInCases[0], nil)

	e := &sim.Extra
	e.ReservationPriceWad = uint256.NewInt(1e18)
	e.MidFeeWad = uint256.NewInt(30_000_000_000_000_000)
	e.DirSkewWad = uint256.NewInt(150_000_000_000_000_000)
	e.InvSkewKappaWad = uint256.NewInt(2_000_000_000_000_000_000)
	e.InvSkewBandWad = uint256.NewInt(60_000_000_000_000_000)
	e.CurvatureWad = uint256.NewInt(50_000_000_000_000_000)
	e.LpFeeWad = uint256.NewInt(30_000_000_000_000_000)
	// A hook whose saturated floor probe exceeds WAD.
	huge := uint256.MustFromDecimal("5000000000000000000")
	e.FloorStableInWad, e.FloorVolatileInWad = huge, huge

	for _, stableIn := range []bool{true, false} {
		b := feeUpperBoundWad(e, e.XWad.ToBig(), sim.reserveStable.ToBig(),
			sim.reserveVolatile.ToBig(), stableIn)
		require.NotNil(t, b)
		require.LessOrEqual(t, b.Cmp(bigWadFee), 0,
			"stableIn=%v: bound %s exceeds WAD", stableIn, b)
	}

	// And the swap path refuses a fee at or above WAD rather than wrapping the subtraction.
	e.FeeStableInWad = uint256.MustFromDecimal("2000000000000000000")
	_, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testStable, Amount: big.NewInt(1e18)},
		TokenOut:      testVol,
	})
	require.ErrorIs(t, err, ErrInvalidFee)
}
