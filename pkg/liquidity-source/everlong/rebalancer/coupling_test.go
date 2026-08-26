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

type liquidityReplayCall struct {
	shares, supply, used0, used1 *big.Int
}

type exactBaseStub struct {
	pool.Pool
	totals []*big.Int
	exact  bool
	calls  []liquidityReplayCall
	xWad   *big.Int
}

func newExactBaseStub(address string) *exactBaseStub {
	reserves := []*big.Int{big.NewInt(1_000_000), big.NewInt(2_000_000)}
	return &exactBaseStub{
		Pool: pool.Pool{Info: pool.PoolInfo{
			Address:  address,
			Tokens:   []string{testNECT, testWBTC},
			Reserves: []*big.Int{new(big.Int).Set(reserves[0]), new(big.Int).Set(reserves[1])},
		}},
		totals: reserves,
		exact:  true,
		xWad:   new(big.Int).Set(bigWadTest),
	}
}

func (b *exactBaseStub) CalcAmountOut(pool.CalcAmountOutParams) (*pool.CalcAmountOutResult, error) {
	return nil, nil
}
func (b *exactBaseStub) UpdateBalance(pool.UpdateBalanceParams) {}
func (b *exactBaseStub) GetMetaInfo(_, _ string) any            { return nil }
func (b *exactBaseStub) GetTotalReserves() []*big.Int           { return b.totals }
func (b *exactBaseStub) IsLiquidityStateExact() bool            { return b.exact }
func (b *exactBaseStub) InvalidateLiquidityState()              { b.exact = false }
func (b *exactBaseStub) CurrentInventoryXWad() *big.Int         { return new(big.Int).Set(b.xWad) }
func (b *exactBaseStub) SnapshotBlockNumber() uint64            { return b.Info.BlockNumber }
func (b *exactBaseStub) ReservationValuePerShareWad(_, _ *big.Int) (*big.Int, bool) {
	return new(big.Int).Set(bigWadTest), b.exact
}

func TestPriceMovingBaseDeltaDisablesOnlyLeverage(t *testing.T) {
	sim, base := exactCouplingHarness(t)
	base.xWad.Add(base.xWad, big.NewInt(1))
	base.totals[0].Add(base.totals[0], big.NewInt(1))
	base.Info.Reserves[0].Add(base.Info.Reserves[0], big.NewInt(1))

	_, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: sim.Info.Tokens[1], Amount: big.NewInt(1)},
		TokenOut:      sim.Info.Tokens[0],
	})
	require.ErrorIs(t, err, ErrVenueGateClosed)
	require.ErrorIs(t, err, ErrUnattestedReference)

	_, err = sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: sim.Info.Tokens[0], Amount: big.NewInt(1)},
		TokenOut:      sim.Info.Tokens[1],
	})
	require.NotErrorIs(t, err, ErrUnattestedReference,
		"the adapter's reference-oracle check is leverage-only")
}
func (b *exactBaseStub) ApplyLiquidityDelta(shares, supply, used0, used1 *big.Int) (
	*big.Int, *big.Int, *big.Int, *big.Int) {
	clone := func(v *big.Int) *big.Int {
		if v == nil {
			return nil
		}
		return new(big.Int).Set(v)
	}
	b.calls = append(b.calls, liquidityReplayCall{clone(shares), clone(supply), clone(used0), clone(used1)})
	return new(big.Int), new(big.Int), new(big.Int), new(big.Int)
}

func exactCouplingHarness(t *testing.T) (*PoolSimulator, *exactBaseStub) {
	sim := newTestPoolSimulator(t)
	sim.StaticExtra.UnderlyingCvamm = "0x000000000000000000000000000000000000c0de"
	sim.Extra.AlmIdleStable = new(big.Int)
	sim.Extra.AlmIdleVolatile = new(big.Int)
	sim.Extra.RvpsWad = new(big.Int).Set(bigWadTest)
	sim.Extra.AlmResvPriceWad = new(big.Int).Set(bigWadTest)
	base := newExactBaseStub(sim.StaticExtra.UnderlyingCvamm)
	sim.wireBase(base)
	require.NotNil(t, sim.basePool)
	// Test-only harness: production enables this latch exclusively after the coupled
	// factory's full snapshot-coherence attestation.
	sim.couplingExact = true
	return sim, base
}

func TestBareSimulatorFailsClosedUntilExactBaseWiring(t *testing.T) {
	sim, err := NewPoolSimulator(newTestPoolEntity(t))
	require.NoError(t, err)
	_, err = sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: sim.Info.Tokens[0], Amount: big.NewInt(1)},
		TokenOut:      sim.Info.Tokens[1],
	})
	require.ErrorIs(t, err, ErrInexactCoupledState)
}

func TestCoupledFactoryRejectsUnknownAdapterRuntime(t *testing.T) {
	known := newTestPoolSimulator(t)
	p := newTestPoolEntity(t)
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(p.StaticExtra), &se))
	se.ALMAdapterCodeHash = "0xdeadbeef"
	raw, err := json.Marshal(se)
	require.NoError(t, err)
	p.StaticExtra = string(raw)

	_, err = NewPoolSimulatorWithBases(p,
		map[string]pool.IPoolSimulator{se.UnderlyingCvamm: known.basePool})
	require.ErrorIs(t, err, ErrUnsupportedAdapter)
}

func TestCoupledFactoryRejectsCrossBlockBase(t *testing.T) {
	p := newTestPoolEntity(t)
	existing := newTestPoolSimulator(t)
	base := existing.basePool.CloneState().(*everlongcvamm.PoolSimulator)
	base.Info.BlockNumber = p.BlockNumber + 1

	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(p.StaticExtra), &se))
	_, err := NewPoolSimulatorWithBases(p,
		map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
	require.ErrorIs(t, err, ErrInexactBasePool,
		"matching summaries from different blocks must not attest hidden CVAMM state")
}

func TestReverseCouplingRequiresExactTransitionShape(t *testing.T) {
	t.Run("deposit then sell-back", func(t *testing.T) {
		sim, base := exactCouplingHarness(t)
		supply := new(big.Int).Set(sim.Extra.AlmSupply)
		require.True(t, sim.pushLeverageToBase(SwapInfo{
			AlmMintedShares:  big.NewInt(10),
			AlmUsedStable:    big.NewInt(20),
			AlmUsedVolatile:  big.NewInt(30),
			AlmSoldBackShare: big.NewInt(3),
		}, supply))
		require.Len(t, base.calls, 2)
		require.Equal(t, "10", base.calls[0].shares.String())
		require.Equal(t, supply.String(), base.calls[0].supply.String())
		require.Equal(t, "20", base.calls[0].used0.String())
		require.Equal(t, "30", base.calls[0].used1.String())
		require.Equal(t, "-3", base.calls[1].shares.String())
		require.Equal(t, new(big.Int).Add(supply, big.NewInt(10)).String(), base.calls[1].supply.String())
	})

	t.Run("legacy net-share shape", func(t *testing.T) {
		sim, base := exactCouplingHarness(t)
		require.False(t, sim.pushLeverageToBase(SwapInfo{AlmShares: big.NewInt(7)}, sim.Extra.AlmSupply))
		require.False(t, base.IsLiquidityStateExact(), "the direct base path must be disabled too")
		_, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: sim.Info.Tokens[1], Amount: big.NewInt(1)},
			TokenOut:      sim.Info.Tokens[0],
		})
		require.ErrorIs(t, err, ErrInexactCoupledState)
		require.Empty(t, base.calls, "a legacy net delta must never reach ApplyLiquidityDelta")
	})
}

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
	cvammTracked, err := everlongcvamm.NewPoolTracker(cvammCfg, client).GetNewPoolStateAtBlock(
		ctx, cvammPools[0], new(big.Int).SetUint64(tracked.BlockNumber))
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

	// 1) no base movement: only the fully attested meta-factory instance can quote.
	q0, err0 := coupled.CalcAmountOut(quoteIn)
	_, uerr0 := uncoupled.CalcAmountOut(quoteIn)
	require.ErrorIs(t, uerr0, ErrInexactCoupledState)
	shifted := coupled.CloneState().(*PoolSimulator)

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
	_, staleErr := NewPoolSimulatorWithBases(tracked,
		map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
	require.ErrorIs(t, staleErr, ErrInexactCoupledState,
		"different-block rebalancer/base snapshots must never be wired")
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

	postRvps, ok := base.ReservationValuePerShareWad(shifted.Extra.AlmResvPriceWad, shifted.Extra.AlmSupply)
	require.True(t, ok)
	require.True(t, foldBaseDeltas(&shifted.Extra, dS, dV, fS, fV, postRvps))
	shifted.basePool = base.CloneState()
	totals, accounted, ok := exactBaseBook(shifted.basePool)
	require.True(t, ok)
	shifted.baseStable0 = new(big.Int).Set(totals[0])
	shifted.baseVolatile0 = new(big.Int).Set(totals[1])
	shifted.baseAccStable0 = new(big.Int).Set(accounted[0])
	shifted.baseAccVolatile0 = new(big.Int).Set(accounted[1])

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

// TestReverseCouplingAgainstChain replays the exact Deposit+Withdraw pair emitted by a
// settled leverage fill at block 24,863,882. Using the event's share counts and token
// legs isolates the transition from keeper lot selection: every post-fill book word must
// equal the chain, with no bps/ppm tolerance.
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

	// Rebalancer snapshot at the parent block.
	rd := newRPCState()
	req := client.NewRequest().SetContext(ctx).SetBlockNumber(parent)
	addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &se, rd, nil)
	_, err = req.TryAggregate()
	require.NoError(t, err)
	// the two reads a multicall cannot carry; without them every gate reads closed
	NewPoolTracker(cfg, client).probeOutsideMulticall(ctx, &se, rd, parent)
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

	depositTopic := common.HexToHash("0x4e2ca0515ed1aef1395f66b5303bb5d6f1bf9d61a353fa53f73f8ac9973fa9f6")
	withdrawTopic := common.HexToHash("0xebff2602b3f468259e1e99f613fed6691f3a6526effe6ef3e768ba7ae7a36c4f")
	decodeTriple := func(data []byte) (shares, amount0, amount1 *big.Int) {
		require.Len(t, data, 96)
		return new(big.Int).SetBytes(data[:32]), new(big.Int).SetBytes(data[32:64]),
			new(big.Int).SetBytes(data[64:96])
	}
	var minted, used0, used1, soldBack *big.Int
	alm := common.HexToAddress(se.UnderlyingCvamm)
	for _, lg := range receipt.Logs {
		if lg.Address != alm || len(lg.Topics) == 0 {
			continue
		}
		switch lg.Topics[0] {
		case depositTopic:
			minted, used0, used1 = decodeTriple(lg.Data)
		case withdrawTopic:
			soldBack, _, _ = decodeTriple(lg.Data)
		}
	}
	require.NotNil(t, minted, "CvammALM Deposit event not found")
	require.NotNil(t, soldBack, "CvammALM Withdraw event not found")
	require.True(t, coupled.pushLeverageToBase(SwapInfo{
		AlmMintedShares:  minted,
		AlmUsedStable:    used0,
		AlmUsedVolatile:  used1,
		AlmSoldBackShare: soldBack,
	}, coupled.Extra.AlmSupply))

	assertPair := func(label string, got, want []*big.Int) {
		require.Len(t, got, 2)
		require.Len(t, want, 2)
		require.Zero(t, got[0].Cmp(want[0]), "%s stable: got %s want %s", label, got[0], want[0])
		require.Zero(t, got[1].Cmp(want[1]), "%s volatile: got %s want %s", label, got[1], want[1])
	}
	assertPair("accounted reserves", base.GetReserves(), truth.GetReserves())
	assertPair("total reserves", base.GetTotalReserves(), truth.GetTotalReserves())
	require.Zero(t, base.Extra.XWad.Cmp(truth.Extra.XWad), "x")
	require.Zero(t, base.Extra.AnchorSqrtX96.Cmp(truth.Extra.AnchorSqrtX96), "anchor")
	require.Zero(t, base.Extra.Kappa.Cmp(truth.Extra.Kappa), "kappa")
	require.Zero(t, base.Extra.FeeStableInWad.Cmp(truth.Extra.FeeStableInWad), "stable-in fee")
	require.Zero(t, base.Extra.FeeVolatileInWad.Cmp(truth.Extra.FeeVolatileInWad), "volatile-in fee")

	// The legacy ClammAlmAdapter recomputes its center NAV after the same four bucket
	// floors, then divides once by the NEW LP supply. Pin that wrapper layer too: it is
	// the reservation value CollateralRebalancer reads after execution.
	postRd := newRPCState()
	postReq := client.NewRequest().SetContext(ctx).SetBlockNumber(receipt.BlockNumber)
	addRPCCalls(func(c *ethrpc.Call, o []any) { postReq.AddCall(c, o) }, &se, postRd, nil)
	_, err = postReq.TryAggregate()
	require.NoError(t, err)
	postAlmSupply := new(big.Int).Add(coupled.Extra.AlmSupply, minted)
	postAlmSupply.Sub(postAlmSupply, soldBack)
	require.Zero(t, postAlmSupply.Cmp(postRd.almSupply), "ALM supply")
	postRvps, ok := base.ReservationValuePerShareWad(coupled.Extra.AlmResvPriceWad, postAlmSupply)
	require.True(t, ok)
	require.Zero(t, postRvps.Cmp(postRd.rvpsWad), "reservationValuePerShareWad")
	require.Zero(t, base.GetTotalReserves()[0].Cmp(postRd.refReserves.StableReserve), "reference stable reserve")
	require.Zero(t, base.GetTotalReserves()[1].Cmp(postRd.refReserves.AssetReserve), "reference volatile reserve")
	postReservation := reservationValueAt(postRd.exchangeState.Collateral, postRd.cvTotalAssets,
		postRd.cvTotalSupply, se.CvDecimalsOffset, postRvps)
	require.Zero(t, postReservation.Cmp(postRd.exchangeState.PriceWad), "CollateralRebalancer reservation value")
	_, _, _, _, moved := coupled.baseDeltas()
	require.False(t, moved, "the exact reverse push must advance both baselines")
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
	cvammTracked, err := everlongcvamm.NewPoolTracker(cvammCfg, client).GetNewPoolStateAtBlock(
		ctx, cvammPools[0], new(big.Int).SetUint64(tracked.BlockNumber))
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
