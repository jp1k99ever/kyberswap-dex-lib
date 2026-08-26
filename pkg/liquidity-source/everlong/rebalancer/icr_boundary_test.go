package everlongrebalancer

import (
	"context"
	"math/big"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestIcrFloorBoundary forces the RebalanceBelowFloor leg of _assertFillSafe to BIND and
// verifies the simulator rejects at exactly the same share boundary the venue does.
//
// The CDP oracle is independent of the venue's endogenous reservation value, so the
// price feed's CODE is replaced (eth_call state override, no storage archaeology) with a
// mock returning a chosen price that parks the pre-fill ICR just above the MCR*1.2
// floor: small leverage fills still pass, larger ones drag the post-fill ICR below the
// floor and revert on-chain. The on-chain boundary is bisected with eth_call on the
// settlement swapper under the same override; the simulator, refreshed through
// GetNewPoolStateWithOverrides with the identical override, must report the same
// maxLeverageShares to the share.
func TestIcrFloorBoundary(t *testing.T) {
	test.SkipCI(t)

	const wbtcBalanceSlot, wbtcAllowanceSlot = 5, 6 // discovered for this token

	cfg := berachainTestConfig()
	client := berachainRPCClient()
	ctx := context.Background()

	lister := NewPoolsListUpdater(cfg, client)
	pools, _, err := lister.GetNewPools(ctx, nil)
	require.NoError(t, err)
	var staticExtra StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &staticExtra))

	rpcClient, err := rpc.Dial(berachainRPCURL())
	require.NoError(t, err)
	defer rpcClient.Close()
	gc := gethclient.New(rpcClient)

	// The live feed also serves the ALM's reference oracle, so replacing its code
	// would break getReservesAtReference (physical-CR leg) and mask the gate under
	// test. Instead the mock lives at a FRESH address and the PM's `priceFeed` storage
	// var (slot 1, probed) is repointed at it — fetchPrice then uses the mock while the
	// reference leg keeps the real feed.
	pmAddr := common.HexToAddress(staticExtra.PositionManager)
	mockFeed := common.HexToAddress("0x00000000000000000000000000000000000FeEed")
	pmPriceFeedSlot := common.BigToHash(big.NewInt(1))

	// Pin to a block where the pending direction was verifiably LEVERAGE (the parent of
	// a settled leverage fill) — at a deleverage-pending state the quote level rejects
	// every leverage size regardless of the oracle and no ICR boundary exists.
	blockNumber := big.NewInt(24_736_812)
	snapshot := func(ov map[common.Address]gethclient.OverrideAccount) Extra {
		rd := newRPCState()
		req := client.NewRequest().SetContext(ctx).SetBlockNumber(blockNumber)
		if ov != nil {
			req.SetOverrides(ov)
		}
		addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &staticExtra, rd, nil)
		_, err := req.TryAggregate()
		require.NoError(t, err)
		// the gates that cannot ride a multicall (the overrides here touch only the price
		// feed and balances, so they do not change either answer)
		NewPoolTracker(cfg, client).probeOutsideMulticall(ctx, &staticExtra, rd, blockNumber)
		p, err := buildPoolState(pools[0], &staticExtra, rd, blockNumber)
		require.NoError(t, err)
		var e Extra
		require.NoError(t, json.Unmarshal([]byte(p.Extra), &e))
		e.CvDecimalsOffset = staticExtra.CvDecimalsOffset
		return e
	}
	baseExtra := snapshot(nil)
	require.NotNil(t, baseExtra.McrWad)

	// Park the pre-fill ICR ~0.05% above the floor: P' = floorCr * debt / coll * 1.0005.
	floorCr := mulDiv(baseExtra.McrWad, big.NewInt(12), big.NewInt(10))
	price := mulDiv(floorCr, baseExtra.Debt, baseExtra.Collateral)
	price = mulDiv(price, big.NewInt(10005), big.NewInt(10000))

	// Mock feed: PUSH32 price; MSTORE; RETURN 32 bytes — answers any selector.
	mock := append([]byte{0x7f}, common.LeftPadBytes(price.Bytes(), 32)...)
	mock = append(mock, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3)

	caller := common.HexToAddress("0x00000000000000000000000000000000000c0FEE")
	wbtc := common.HexToAddress(cfg.Volatile)
	swapper := common.HexToAddress(staticExtra.Swapper)
	huge := common.MaxHash

	balSlot := crypto.Keccak256Hash(common.LeftPadBytes(caller.Bytes(), 32),
		common.BigToHash(big.NewInt(wbtcBalanceSlot)).Bytes())
	allowInner := crypto.Keccak256Hash(common.LeftPadBytes(caller.Bytes(), 32),
		common.BigToHash(big.NewInt(wbtcAllowanceSlot)).Bytes())
	allowSlot := crypto.Keccak256Hash(common.LeftPadBytes(swapper.Bytes(), 32), allowInner.Bytes())

	overrides := map[common.Address]gethclient.OverrideAccount{
		mockFeed: {Code: mock},
		pmAddr:   {StateDiff: map[common.Hash]common.Hash{pmPriceFeedSlot: common.BytesToHash(mockFeed.Bytes())}},
		wbtc:     {StateDiff: map[common.Hash]common.Hash{balSlot: huge, allowSlot: huge}},
	}

	// Sanity: the overridden feed answers with our price.
	pmABIPrice := new(big.Int)
	{
		data, err := positionManagerABI.Pack(pmMethodFetchPrice)
		require.NoError(t, err)
		pm := common.HexToAddress(staticExtra.PositionManager)
		out, err := gc.CallContract(ctx, ethereum.CallMsg{To: &pm, Data: data}, blockNumber, &overrides)
		require.NoError(t, err)
		pmABIPrice.SetBytes(out)
		require.Zero(t, pmABIPrice.Cmp(price), "feed code override must take effect")
	}

	// On-chain boundary: largest sharesIn whose swapVolatileForStable eth_call succeeds.
	fills := func(shares *big.Int) bool {
		preview, err := swapperABI.Pack("previewTokenAmounts", shares, true)
		require.NoError(t, err)
		out, err := gc.CallContract(ctx, ethereum.CallMsg{To: &swapper, Data: preview}, blockNumber, &overrides)
		if err != nil {
			return false
		}
		res, err := swapperABI.Unpack("previewTokenAmounts", out)
		require.NoError(t, err)
		stableReq, volReq := res[0].(*big.Int), res[1].(*big.Int)
		data, err := swapperABI.Pack("swapVolatileForStable",
			shares, stableReq, volReq, big.NewInt(0), caller)
		require.NoError(t, err)
		_, err = gc.CallContract(ctx,
			ethereum.CallMsg{From: caller, To: &swapper, Data: data}, blockNumber, &overrides)
		if err != nil && testing.Verbose() {
			t.Logf("probe %s: %v", shares, err)
		}
		return err == nil
	}
	// Sub-share-scale fills die in the swapper's materialization (previewMint -> 0 ALM
	// shares), which is not the gate under test — find any filling size by halving from
	// collateral scale, then bisect the UPPER edge from there.
	one := big.NewInt(1)
	lo := new(big.Int).Set(baseExtra.Collateral)
	found := false
	for i := 0; i < 40 && lo.Sign() > 0; i++ {
		if fills(lo) {
			found = true
			break
		}
		lo.Rsh(lo, 1)
	}
	require.True(t, found, "no filling size found under the parked ICR")
	hi := new(big.Int).Set(baseExtra.Collateral) // generous hi
	for lo.Cmp(hi) < 0 {
		var mid big.Int
		mid.Sub(hi, lo)
		mid.Add(&mid, one)
		mid.Rsh(&mid, 1)
		mid.Add(lo, &mid)
		if fills(&mid) {
			lo.Set(&mid)
		} else {
			hi.Sub(&mid, one)
		}
	}
	onchainBoundary := lo
	t.Logf("on-chain boundary under parked ICR: %s shares (price %s)", onchainBoundary, price)

	// Simulator side: identical overrides through the tracker plan, same block.
	extra := snapshot(overrides)
	require.Zero(t, extra.IcrPriceWad.Cmp(price), "tracker must read the overridden price")

	cp := &staticExtra.CurveParams
	localBoundary := cp.maxLeverageShares(&extra)
	require.Zero(t, localBoundary.Cmp(onchainBoundary),
		"simulator boundary %s must equal the on-chain boundary %s under the parked ICR",
		localBoundary, onchainBoundary)

	// The boundary must be the ICR leg binding, not the physical-CR floor: with the ICR
	// check disabled locally the boundary must move outward.
	noIcr := extra
	noIcr.IcrPriceWad = nil
	require.True(t, cp.maxLeverageShares(&noIcr).Cmp(localBoundary) > 0,
		"disabling the ICR leg must widen the boundary — otherwise another gate bound first")
}
