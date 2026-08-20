package everlongrebalancer

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	everlongcvamm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/cvamm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestBasePoolCoupling wires the live CollateralRebalancer pool to the everlong-cvamm pool it
// actually trades on (the swapper's ALM adapter wraps the CvammALM) and checks the meta
// coupling end to end:
//   - the listing resolves the underlying CvammALM from the adapter's alm() getter;
//   - with no base movement the coupled quote equals the uncoupled quote;
//   - after a CVAMM fill is applied to the base sim, the rebalancer quote moves, and it
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
	require.NotEmpty(t, se.UnderlyingCvamm, "adapter alm() must resolve the wrapped CvammALM")

	cvammCfg := &everlongcvamm.Config{DexID: "everlong-cvamm", ALMs: []everlongcvamm.ALMConfig{{Address: se.UnderlyingCvamm}}}
	cvammPools, _, err := everlongcvamm.NewPoolsListUpdater(cvammCfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, cvammPools, 1)

	// track both at head
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	cvammTracked, err := everlongcvamm.NewPoolTracker(cvammCfg, client).GetNewPoolState(ctx, cvammPools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)

	// live-curve overlay: the CR floor answers on the deployed impl and must land in Extra
	var trackedExtra Extra
	require.NoError(t, json.Unmarshal([]byte(tracked.Extra), &trackedExtra))
	require.NotNil(t, trackedExtra.LiveCurve, "PHYSICAL_CR_FLOOR_WAD() read must produce the live overlay")
	require.Equal(t, "1820000000000000000", trackedExtra.LiveCurve.PhysicalCrFloorWad.String())

	base, err := everlongcvamm.NewPoolSimulator(cvammTracked)
	require.NoError(t, err)

	_, errNoBase := NewPoolSimulatorWithBases(tracked, map[string]pool.IPoolSimulator{})
	require.ErrorIs(t, errNoBase, ErrMissingBasePool, "absent base must fail closed")

	coupled, err := NewPoolSimulatorWithBases(tracked, map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
	require.NoError(t, err)
	require.NotNil(t, coupled.basePool, "base must wire by the resolved address")
	uncoupled, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	stable, volatile := coupled.Info.Tokens[0], coupled.Info.Tokens[1]
	quoteIn := pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: stable, Amount: new(big.Int).Mul(big.NewInt(500), bigWadTest)}, // 500 NECT deleverage probe: large enough that a 10 NECT base fill moves the 8-dec output by whole sats at today's book
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
	// TOTAL-inventory deltas (accounted + idled fee): the domain the vault's
	// getTotalAmounts-based words move by. The output fee stays inside the ALM, so the
	// accounted delta would overstate the outflow by exactly the fee.
	beforeTot := base.GetTotalReserves()
	beforeAcc := base.GetReserves()
	base.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: swap.SwapInfo})
	afterTot := base.GetTotalReserves()
	dS := new(big.Int).Sub(afterTot[0], beforeTot[0])
	dV := new(big.Int).Sub(afterTot[1], beforeTot[1])
	require.True(t, dS.Sign() != 0 || dV.Sign() != 0)
	afterAcc := base.GetReserves()
	dAccV := new(big.Int).Sub(afterAcc[1], beforeAcc[1])
	require.Equal(t, swap.Fee.Amount.String(), new(big.Int).Sub(dV, dAccV).String(),
		"totals must move by -net while accounted moves by -gross (fee idles in the ALM)")
	// idle-only fee legs: value them at the reservation price to shift rvps
	fS := new(big.Int).Sub(dS, new(big.Int).Sub(afterAcc[0], beforeAcc[0]))
	fV := new(big.Int).Sub(dV, dAccV)

	shifted, err := NewPoolSimulator(tracked)
	require.NoError(t, err)
	foldBaseDeltas(&shifted.Extra, dS, dV, fS, fV)

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
		_, _, _, _, moved := coupled.baseDeltas()
		require.False(t, moved, "baseline must advance so base movement is not recounted")
	}
}

// TestReverseCouplingAgainstChain replays the settled leverage fill at block 24,863,882
// (the review's counter-example) through the COUPLED sims and compares the pushed base
// CVAMM state against the chain at the fill block: reserves must shift by the fill's ALM
// legs and kappa by the pro-rata supply scale, with x, anchor and band untouched.
func TestReverseCouplingAgainstChain(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	const fillTx = "0x9c404f52d4c6b70d13a31600d72fa6babdd8b05aeaf69bffbc45ad4724c1fb96"

	cfg := berachainTestConfig()
	client := berachainRPCClient()
	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &se))
	require.NotEmpty(t, se.UnderlyingCvamm)

	geth, err := ethclient.Dial(berachainRPCURL())
	require.NoError(t, err)
	defer geth.Close()
	receipt, err := geth.TransactionReceipt(ctx, common.HexToHash(fillTx))
	require.NoError(t, err)
	parent := new(big.Int).Sub(receipt.BlockNumber, big.NewInt(1))

	// The physical WBTC the caller paid: the volatile Transfer into the swapper.
	transferTopic := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	volatileToken := common.HexToAddress(cfg.Volatile)
	swapper := common.HexToAddress(se.Swapper)
	var amountIn *big.Int
	for _, lg := range receipt.Logs {
		if lg.Address == volatileToken && len(lg.Topics) == 3 && lg.Topics[0] == transferTopic &&
			common.BytesToAddress(lg.Topics[2].Bytes()) == swapper {
			amountIn = new(big.Int).SetBytes(lg.Data)
			break
		}
	}
	require.NotNil(t, amountIn, "caller volatile transfer not found")

	// Rebalancer snapshot at the parent block.
	rd := newRPCState()
	req := client.NewRequest().SetContext(ctx).SetBlockNumber(parent)
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &se, rd)
	_, err = req.TryAggregate()
	require.NoError(t, err)
	trackedEntity, err := buildPoolState(pools[0], &se, rd, parent)
	require.NoError(t, err)

	// CVAMM snapshot at the parent block and at the fill block (the truth to match).
	cvammCfg := &everlongcvamm.Config{DexID: "everlong-cvamm", ALMs: []everlongcvamm.ALMConfig{{Address: se.UnderlyingCvamm}}}
	cvammPools, _, err := everlongcvamm.NewPoolsListUpdater(cvammCfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	baseAt := func(block *big.Int) *everlongcvamm.PoolSimulator {
		p, err := everlongcvamm.NewPoolTracker(cvammCfg, client).GetNewPoolStateAtBlock(ctx, cvammPools[0], block)
		require.NoError(t, err)
		sim, err := everlongcvamm.NewPoolSimulator(p)
		require.NoError(t, err)
		return sim
	}
	base := baseAt(parent)
	truth := baseAt(receipt.BlockNumber)

	// fail-closed: an absent base must refuse construction, not fall back uncoupled
	_, errNoBase := NewPoolSimulatorWithBases(trackedEntity, map[string]pool.IPoolSimulator{})
	require.ErrorIs(t, errNoBase, ErrMissingBasePool)

	coupled, err := NewPoolSimulatorWithBases(trackedEntity, map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
	require.NoError(t, err)
	require.NotNil(t, coupled.basePool)

	// Replay the settled leverage fill (volatile in) and push it through UpdateBalance.
	res, err := coupled.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: coupled.Info.Tokens[1], Amount: amountIn},
		TokenOut:      coupled.Info.Tokens[0],
	})
	require.NoError(t, err)
	coupled.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})

	gotRes, wantRes := base.GetReserves(), truth.GetReserves()
	// The pro-rata scaling itself is exact — the chain scales kappa by the same
	// (supply+shares)/supply and splits the deposit so the priced book takes its share —
	// and x is unmoved to the wei. What drifts is the SCALE FACTOR: the predicted ALM mint
	// runs ~0.2% above the settled one, and since the mint is a fraction of supply that
	// dilutes to ~2 bps, applied identically to both reserves and kappa. Bounded at 5 here,
	// and it reaches only a chained quote on the pushed base — see ApplyLiquidityDelta.
	require.Zero(t, base.Extra.XWad.Cmp(truth.Extra.XWad), "x must be unmoved by a pro-rata leg")
	relDiffBps := func(got, want *big.Int) int64 {
		d := new(big.Int).Abs(new(big.Int).Sub(got, want))
		d.Mul(d, big.NewInt(10_000))
		return new(big.Int).Div(d, want).Int64()
	}
	require.LessOrEqual(t, relDiffBps(gotRes[0], wantRes[0]), int64(5), "stable reserve drift: got %s want %s", gotRes[0], wantRes[0])
	require.LessOrEqual(t, relDiffBps(gotRes[1], wantRes[1]), int64(5), "volatile reserve drift: got %s want %s", gotRes[1], wantRes[1])
	// What the drift is WORTH. The reserves and kappa carry it, but a quote reads them
	// only through price impact, which is second order — so a book 2 bps deep in the wrong
	// place does not move a quote by 2 bps. Measured against the chain across the size
	// range, including an input equal to the whole stable leg (where the solvency clamp
	// binds and the drift scales it directly), the error stays inside a bp. It is on the
	// optimistic side, so it spends slippage budget rather than being free; this bounds how
	// much. A regression that turned the structural sliver into a real mispricing would
	// show up here rather than in the reserve figures, which is why it is asserted.
	for _, frac := range []int64{1000, 100, 10, 4, 2, 1} {
		amt := new(big.Int).Div(wantRes[0], big.NewInt(frac))
		if amt.Sign() == 0 {
			continue
		}
		quoteOut := func(s *everlongcvamm.PoolSimulator) (out, used *big.Int) {
			r, err := s.CalcAmountOut(pool.CalcAmountOutParams{
				TokenAmountIn: pool.TokenAmount{Token: s.Info.Tokens[0], Amount: amt},
				TokenOut:      s.Info.Tokens[1]})
			if err != nil {
				return nil, nil
			}
			return r.TokenAmountOut.Amount, new(big.Int).Sub(amt, r.RemainingTokenAmountIn.Amount)
		}
		pushedOut, pushedUsed := quoteOut(base)
		chainOut, chainUsed := quoteOut(truth)
		if pushedOut == nil || chainOut == nil {
			continue
		}
		errPpm := new(big.Int).Div(
			new(big.Int).Mul(new(big.Int).Sub(pushedOut, chainOut), big.NewInt(1_000_000)), chainOut)
		t.Logf("quote at 1/%d of the book: pushed vs chain %s ppm (%s vs %s, consumed %s vs %s)",
			frac, errPpm, pushedOut, chainOut, pushedUsed, chainUsed)
		require.LessOrEqual(t, errPpm.Int64(), int64(200),
			"a chained quote at 1/%d of the book overstates the chain by %s ppm", frac, errPpm)
	}

	require.LessOrEqual(t, relDiffBps(base.Extra.Kappa.ToBig(), truth.Extra.Kappa.ToBig()), int64(2),
		"kappa drift: got %s want %s", base.Extra.Kappa, truth.Extra.Kappa)
	t.Logf("pushed base vs chain @%s: x exact, reserves within %d/%d bps, kappa within %d bps",
		receipt.BlockNumber, relDiffBps(gotRes[0], wantRes[0]), relDiffBps(gotRes[1], wantRes[1]),
		relDiffBps(base.Extra.Kappa.ToBig(), truth.Extra.Kappa.ToBig()))
}

// TestCloneIsolatesBasePool: this pool's UpdateBalance pushes the fill into the base
// CVAMM sim, so a clone that shared the base pointer would let a speculative fill mutate
// the very state the caller kept as its backup. Established meta pools (curve stable-meta)
// clone the base for exactly this reason, and the IPoolSimulator contract asks CloneState
// to cover every field UpdateBalance touches — here that includes the base.
func TestCloneIsolatesBasePool(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	cfg := berachainTestConfig()
	client := berachainRPCClient()

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &se))

	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)

	cvammCfg := &everlongcvamm.Config{DexID: "everlong-cvamm",
		ALMs: []everlongcvamm.ALMConfig{{Address: se.UnderlyingCvamm}}}
	cvammPools, _, err := everlongcvamm.NewPoolsListUpdater(cvammCfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	cvammTracked, err := everlongcvamm.NewPoolTracker(cvammCfg, client).GetNewPoolState(ctx, cvammPools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	base, err := everlongcvamm.NewPoolSimulator(cvammTracked)
	require.NoError(t, err)

	orig, err := NewPoolSimulatorWithBases(tracked, map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
	require.NoError(t, err)

	backup := orig.CloneState().(*PoolSimulator)
	require.NotSame(t, orig.basePool, backup.basePool, "the clone must not share the base pointer")

	before := snapshotReserves(orig.basePool)

	// A leverage fill on the CLONE mints ALM shares, which pushes into its base.
	stable, volatile := orig.Info.Tokens[0], orig.Info.Tokens[1]
	q, err := backup.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: volatile, Amount: big.NewInt(12_000)},
		TokenOut:      stable,
	})
	if err != nil {
		t.Skipf("leverage not quotable at head: %v", err)
	}
	backup.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: q.SwapInfo})

	require.NotEqual(t, before, snapshotReserves(backup.basePool),
		"the fill must actually reach the clone's base, or this test proves nothing")
	require.Equal(t, before, snapshotReserves(orig.basePool),
		"a fill on the clone must not move the original's base")
}

func snapshotReserves(p pool.IPoolSimulator) []string {
	out := make([]string, 0, 2)
	for _, r := range p.GetReserves() {
		out = append(out, r.String())
	}
	return out
}

// TestMetaInfoCarriesExactDeleverageInputs: the executor re-derives the deleverage gross
// against the venue's own math on a partial fill, so the meta must carry the library and
// the ratio it validates against — neither is discoverable on-chain.
func TestMetaInfoCarriesExactDeleverageInputs(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	cfg := berachainTestConfig()
	client := berachainRPCClient()
	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	meta, ok := sim.GetMetaInfo("", "").(PoolMeta)
	require.True(t, ok)
	require.Equal(t, strings.ToLower(cfg.Math), meta.Math)
	require.NotNil(t, meta.LeverageRatioWad)
	require.True(t, meta.LeverageRatioWad.Sign() > 0)
}

// TestMathConfigFailsClosed: a partial deleverage re-derives its gross against the
// configured CollRebalancerMath, so a missing or wrong address does not degrade the
// venue — it reverts the fill. Listing must refuse rather than publish a routable pool
// that cannot settle.
func TestMathConfigFailsClosed(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client := berachainRPCClient()

	for _, c := range []struct {
		name string
		math string
	}{
		{"unset", ""},
		{"zero address", "0x0000000000000000000000000000000000000000"},
		{"not an address", "not-an-address"},
		{"an EOA with no code", "0x4A964e9658792f294AF4BF923ca1A38F6FBa0896"},
		{"a contract without the ABI", "0x1cE0a25D13CE4d52071aE7e02Cf1F6606F4C79d3"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := berachainTestConfig()
			cfg.Math = c.math
			_, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
			require.ErrorIs(t, err, ErrMathNotConfigured)
		})
	}

	// The real library still lists.
	pools, _, err := NewPoolsListUpdater(berachainTestConfig(), client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)
}

// TestTrackerStampsBlockNumber: the snapshot must carry the block it was read at.
// TryAggregate returns none, which silently froze this field at whatever the lister
// stamped and left any same-block pinning downstream unenforced.
func TestTrackerStampsBlockNumber(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	cfg := berachainTestConfig()
	client := berachainRPCClient()

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	p := pools[0]
	p.BlockNumber = 0 // so only the tracker can set it

	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, p, pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	require.NotZero(t, tracked.BlockNumber, "the tracker must stamp the block it read at")
}
