package everlongrebalancer

import (
	"math/big"
	"math/rand"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Property tests for the sizing layer: the point fixtures pin values, these pin the
// relations every sizing routine (and the on-chain adapter's bisections) rely on, over
// randomized states derived from the pinned one — collateral, debt, book and idle
// scaled independently, at every region the curve accepts.
//
// Seeded (EVERLONG_PROP_SEED overrides) so a failure reproduces.

func propRand(t *testing.T) *rand.Rand {
	seed := int64(20260820)
	if s := os.Getenv("EVERLONG_PROP_SEED"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		require.NoError(t, err)
		seed = v
	}
	t.Logf("EVERLONG_PROP_SEED=%d", seed)
	return rand.New(rand.NewSource(seed))
}

// scaled returns v * f where f in [lo, hi] (per-mille precision).
func scaled(r *rand.Rand, v *big.Int, lo, hi float64) *big.Int {
	f := lo + r.Float64()*(hi-lo)
	num := big.NewInt(int64(f * 1000))
	return new(big.Int).Div(new(big.Int).Mul(v, num), big.NewInt(1000))
}

// randomState perturbs the fixture: position and book move independently, idle is a
// random slice of the totals, the CollVault may carry a donation (assets > supply).
func randomState(t *testing.T, r *rand.Rand) *VaultState {
	s := vaultStateFromFixture(t, loadFixture(t))
	// the position moves as a whole (its CR is what the curve prices), with a little
	// independent jitter; the book and its idle slice move independently
	pos := 0.8 + r.Float64()*0.5
	s.Collateral = scaled(r, scaled(r, s.Collateral, pos, pos), 0.97, 1.03)
	s.Debt = scaled(r, scaled(r, s.Debt, pos, pos), 0.97, 1.03)
	book := 0.7 + r.Float64()*0.8
	s.AlmStableReserve = scaled(r, scaled(r, s.AlmStableReserve, book, book), 0.9, 1.1)
	s.AlmVolatileReserve = scaled(r, scaled(r, s.AlmVolatileReserve, book, book), 0.9, 1.1)
	s.RefStableReserve = scaled(r, s.RefStableReserve, book, book)
	s.RefAssetReserve = scaled(r, s.RefAssetReserve, book, book)
	s.AlmIdleStable = scaled(r, s.AlmStableReserve, 0, 0.2)
	s.AlmIdleVolatile = scaled(r, s.AlmVolatileReserve, 0, 0.2)
	s.CvTotalSupply = new(big.Int).Set(s.Collateral)
	s.CvTotalAssets = new(big.Int).Set(s.Collateral)
	if r.Intn(4) == 0 {
		s.CvTotalAssets = scaled(r, s.CvTotalAssets, 1, 1.05) // donated vault
	}
	return s
}

func sampleUpTo(r *rand.Rand, hi *big.Int, n int) []*big.Int {
	out := make([]*big.Int, 0, n)
	for i := 0; i < n; i++ {
		x := new(big.Int).Rand(r, hi)
		x.Add(x, big.NewInt(1))
		out = append(out, x)
	}
	return out
}

func TestPropDeleverageSizing(t *testing.T) {
	r := propRand(t)
	cp := berachainCurveParams()
	var quotable, adjacentCompared int
	for trial := 0; trial < 60; trial++ {
		s := randomState(t, r)
		maxGross := cp.maxDeleverageIn(s)
		if maxGross.Sign() == 0 {
			continue
		}
		quotable++
		// The inversion below is a bisection, so its safety premise is the exact physical
		// net — including separate accounted/idle floors — being nondecreasing at every
		// one-wei gross boundary. Probe adjacent values around random points, curve/CDP
		// edges and later around every selected inversion result.
		checkAdjacent := func(g *big.Int) {
			if g.Sign() <= 0 || g.Cmp(maxGross) >= 0 {
				return
			}
			next := new(big.Int).Add(g, big.NewInt(1))
			stable, ok := cp.physicalDeleverageStableAt(s, g)
			stableNext, nextOK := cp.physicalDeleverageStableAt(s, next)
			if !ok || !nextOK {
				return
			}
			net := new(big.Int).Sub(g, stable)
			netNext := new(big.Int).Sub(next, stableNext)
			require.GreaterOrEqual(t, netNext.Cmp(net), 0,
				"trial %d: physical net decreased at adjacent grosses %s/%s (%s -> %s)",
				trial, g, next, net, netNext)
			adjacentCompared++
		}
		boundaries := []*big.Int{big.NewInt(1), new(big.Int).Sub(maxGross, big.NewInt(1))}
		boundaries = append(boundaries, sampleUpTo(r, maxGross, 48)...)
		for _, center := range boundaries {
			for delta := int64(-2); delta <= 2; delta++ {
				checkAdjacent(new(big.Int).Add(center, big.NewInt(delta)))
			}
		}
		// the max quotes; one more is either refused by the curve or past the CDP's
		// minimum-net-debt ceiling (which the raw quote does not know about)
		out, _, _ := cp.deleverageQuoteChecked(s, maxGross)
		require.Positive(t, out.Sign(), "trial %d: maxDeleverageIn must quote", trial)
		if maxGross.Cmp(s.debtRepayCeiling()) < 0 {
			out, _, _ = cp.deleverageQuoteChecked(s, new(big.Int).Add(maxGross, big.NewInt(1)))
			require.Zero(t, out.Sign(), "trial %d: maxDeleverageIn+1 must not quote", trial)
		}

		// net(gross) = gross - released stable is nondecreasing and stays under gross
		grosses := sampleUpTo(r, maxGross, 24)
		sortBig(grosses)
		prev := new(big.Int).Neg(big.NewInt(1))
		for _, g := range grosses {
			stableOut, ok := cp.physicalDeleverageStableAt(s, g)
			if !ok {
				continue // sub-wei lots the curve rejects
			}
			require.Less(t, stableOut.Cmp(g), 0, "trial %d: released stable must stay under the debt retired", trial)
			net := new(big.Int).Sub(g, stableOut)
			require.GreaterOrEqual(t, net.Cmp(prev), 0, "trial %d: net must be nondecreasing in gross (%s then %s)", trial, prev, net)
			prev = net
		}

		// grossForNetStableIn(budget) is the largest quoting gross whose net fits
		for _, budget := range sampleUpTo(r, maxGross, 8) {
			g := cp.grossForNetStableIn(s, budget, maxGross)
			if g.Sign() == 0 {
				continue
			}
			stableOut, ok := cp.physicalDeleverageStableAt(s, g)
			require.True(t, ok)
			require.LessOrEqual(t, new(big.Int).Sub(g, stableOut).Cmp(budget), 0, "trial %d: chosen gross overspends", trial)
			next := new(big.Int).Add(g, big.NewInt(1))
			for delta := int64(-3); delta <= 2; delta++ {
				checkAdjacent(new(big.Int).Add(g, big.NewInt(delta)))
			}
			if next.Cmp(maxGross) <= 0 {
				if so, ok := cp.physicalDeleverageStableAt(s, next); ok {
					require.Greater(t, new(big.Int).Sub(next, so).Cmp(budget), 0, "trial %d: a larger gross still fits the budget", trial)
				}
			}
		}

		// the physical redeem legs sit within one wei under the preview
		for _, g := range grosses[:4] {
			shares, _, _ := cp.deleverageQuoteChecked(s, g)
			if shares.Sign() == 0 {
				continue
			}
			prevS, prevV, ok := s.previewTokenAmounts(shares, false)
			require.True(t, ok)
			alm, ok := s.redeemAlmShares(s.netRedeemShares(shares))
			if !ok {
				continue // donated vault: preview and transfer disagree, the quote refuses
			}
			actS, actV := s.redeemLegs(alm)
			for _, d := range []*big.Int{new(big.Int).Sub(prevS, actS), new(big.Int).Sub(prevV, actV)} {
				require.True(t, d.Sign() >= 0 && d.Cmp(big.NewInt(1)) <= 0, "trial %d: physical leg vs preview off by %s", trial, d)
			}
		}
	}
	require.Greater(t, quotable, 20, "too few quotable states to mean anything")
	require.Greater(t, adjacentCompared, 1_000,
		"too few valid one-wei physical-net boundaries to support bisection")
}

func TestPropLeverageSizing(t *testing.T) {
	r := propRand(t)
	cp := berachainCurveParams()
	var quotable int
	for trial := 0; trial < 60; trial++ {
		s := randomState(t, r)
		maxShares := cp.maxLeverageShares(s)
		if maxShares.Sign() == 0 {
			continue
		}
		quotable++
		// previewTokenAmounts' volatile leg is nondecreasing in shares
		shares := sampleUpTo(r, maxShares, 24)
		sortBig(shares)
		prev := new(big.Int)
		for _, sh := range shares {
			_, vol, ok := s.previewTokenAmounts(sh, true)
			require.True(t, ok)
			require.GreaterOrEqual(t, vol.Cmp(prev), 0, "trial %d: volatile leg must be nondecreasing", trial)
			prev = vol
		}
		// sharesForVolatileIn is the largest count whose leg fits, capped
		for _, amountIn := range sampleUpTo(r, prev, 8) {
			got := s.sharesForVolatileIn(amountIn, maxShares)
			if got.Sign() == 0 {
				continue
			}
			_, vol, _ := s.previewTokenAmounts(got, true)
			require.LessOrEqual(t, vol.Cmp(amountIn), 0, "trial %d: chosen shares need more volatile than funded", trial)
			if got.Cmp(maxShares) < 0 {
				_, volNext, _ := s.previewTokenAmounts(new(big.Int).Add(got, big.NewInt(1)), true)
				require.Greater(t, volNext.Cmp(amountIn), 0, "trial %d: one more share would still fit", trial)
			}
			// the physical legs never exceed the caps the executor passes
			stableLeg, _, _ := s.previewTokenAmounts(got, true)
			physS, physV, _, _, _, ok := s.leverageLegsActual(s.cvConvertToAssets(got, true), stableLeg, amountIn)
			require.True(t, ok, "trial %d: the mint must cover the CollVault's requirement", trial)
			require.LessOrEqual(t, physS.Cmp(stableLeg), 0)
			require.LessOrEqual(t, physV.Cmp(amountIn), 0)
			require.True(t, physS.Sign() >= 0 && physV.Sign() >= 0)
		}
	}
	require.Greater(t, quotable, 20, "too few quotable states to mean anything")
}

func sortBig(xs []*big.Int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j].Cmp(xs[j-1]) < 0; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
