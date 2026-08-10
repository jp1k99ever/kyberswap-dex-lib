package everlongcollvault

import (
	"context"
	"math/big"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestReplayLiveExecutions replays every fill settled on the Berachain rebalancer so far
// (LeverageIncreased / LeverageDecreased events) against the local port: all state words
// are read at the fill's PARENT block and the local quote must reproduce the settled
// (out, newColl, newDebt) to the wei. Because leverageQuoteChecked/deleverageQuoteChecked
// now include the full fill-acceptance predicate (_assertFillSafe: post-fill reservation
// recompute, anchor/baseX non-decrease, price band, physical CR, ICR floor), a spurious
// local REJECTION of a fill the venue actually settled fails just as loudly as a wrong
// amount — this is the regression gate for the acceptance port.
//
// The fills are immutable chain facts; only the RPC is environmental.
func TestReplayLiveExecutions(t *testing.T) {
	test.SkipCI(t)

	fills := []struct {
		txHash     string
		isLeverage bool
	}{
		{"0xc5d981e781d6b3b68a9c33fbc6f1c5d053fe62fc3884c43df87c2321571b5f1a", true},  // block 24737837
		{"0x684ddc1d13f5a64b2d1daf138ed43dc13d0cdf428d92fde507ccf4d2276ea80e", false}, // block 24737839
		{"0xb2bc229b4718afcbd9fd0ff4ba1b3700f51079530b637fe1cb0bea5024d6b3c5", true},  // block 24737843
		{"0x441c39ffecd11f0eb0f3b3ac9fb7c8c2659b18cd357fa3cacd1b2b09f1439219", false}, // block 24737845
		{"0x76fec2fd042ac015bbb241a4e00717ef00884ca7be492c963684986111d3f237", true},  // block 24737846
	}

	rpcURL := berachainRPCURL()
	cfg := berachainTestConfig()
	client := berachainRPCClient()

	lister := NewPoolsListUpdater(cfg, client)
	pools, _, err := lister.GetNewPools(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)
	var staticExtra StaticExtra
	require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &staticExtra))

	geth, err := ethclient.Dial(rpcURL)
	require.NoError(t, err)
	defer geth.Close()

	rebalancer := common.HexToAddress(cfg.Rebalancer)
	for _, fill := range fills {
		t.Run(fill.txHash[:10], func(t *testing.T) {
			receipt, err := geth.TransactionReceipt(context.Background(), common.HexToHash(fill.txHash))
			require.NoError(t, err)

			// The event's six non-indexed words: (in, out, newColl, newDebt, spreadPpm, icrAfter).
			var words []*big.Int
			for _, lg := range receipt.Logs {
				if lg.Address != rebalancer || len(lg.Data) != 6*32 {
					continue
				}
				for i := 0; i < 6; i++ {
					words = append(words, new(big.Int).SetBytes(lg.Data[i*32:(i+1)*32]))
				}
				break
			}
			require.Len(t, words, 6, "rebalancer fill event not found in receipt")
			amountIn, amountOut := words[0], words[1]
			wantColl, wantDebt, wantSpread, wantIcr := words[2], words[3], words[4], words[5]

			// Full tracker snapshot pinned to the fill's parent block.
			parent := new(big.Int).Sub(receipt.BlockNumber, big.NewInt(1))
			rd := newRPCState()
			req := client.NewRequest().SetContext(context.Background()).SetBlockNumber(parent)
			addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &staticExtra, rd)
			_, err = req.TryAggregate()
			require.NoError(t, err)
			p, err := buildPoolState(pools[0], &staticExtra, rd, parent)
			require.NoError(t, err)
			var state Extra
			require.NoError(t, json.Unmarshal([]byte(p.Extra), &state))
			state.CvDecimalsOffset = staticExtra.CvDecimalsOffset

			// The contract quotes on the fill-block spread; the parent-block snapshot can
			// lag a levFeeHook move, so pin the settled value before replaying.
			if state.SpreadPpm.Cmp(wantSpread) != 0 {
				t.Logf("spread moved between parent and fill block: %s -> %s (pinning settled)",
					state.SpreadPpm, wantSpread)
				state.SpreadPpm = wantSpread
			}

			cp := &staticExtra.CurveParams
			var out, newColl, newDebt *big.Int
			if fill.isLeverage {
				out, newColl, newDebt = cp.leverageQuoteChecked(&state, amountIn)
			} else {
				out, newColl, newDebt = cp.deleverageQuoteChecked(&state, amountIn)
			}
			require.NotZero(t, out.Sign(),
				"local port rejected a fill the venue settled (block %s)", receipt.BlockNumber)
			require.Zero(t, out.Cmp(amountOut), "out: local %s vs settled %s", out, amountOut)
			require.Zero(t, newColl.Cmp(wantColl), "newColl: local %s vs settled %s", newColl, wantColl)
			require.Zero(t, newDebt.Cmp(wantDebt), "newDebt: local %s vs settled %s", newDebt, wantDebt)

			// ICR soft check: the settled icrAfter used the fill block's fetchPrice; ours
			// is the parent block's. Equal price -> equal ICR.
			if state.IcrPriceWad != nil && state.IcrPriceWad.Sign() > 0 {
				if icr := computeCR(newColl, newDebt, state.IcrPriceWad); icr != nil && icr.Cmp(wantIcr) != 0 {
					t.Logf("icrAfter differs (oracle moved within the block): local %s vs settled %s", icr, wantIcr)
				}
			}
			t.Logf("block %s %v: in=%s out=%s newColl=%s newDebt=%s replayed wei-exact",
				receipt.BlockNumber, map[bool]string{true: "leverage", false: "deleverage"}[fill.isLeverage],
				amountIn, out, newColl, newDebt)
		})
	}

	// Adapter-parity pin: the ks-dex-adapter-lib EverlongCollVaultAdapter re-derives the
	// share count on-chain by the same largest-shares-that-fit rule sharesForVolatileIn
	// uses. Its fork test at block 24736812 (data-hint-free bisection over the settled
	// fill's volatile budget) lands on 325591518529 shares — the rounding-plateau right
	// edge covering the settled 325575695741. The local inversion must agree exactly.
	t.Run("adapter-inversion-parity", func(t *testing.T) {
		parent := big.NewInt(24_736_812)
		rd := newRPCState()
		req := client.NewRequest().SetContext(context.Background()).SetBlockNumber(parent)
		addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, &staticExtra, rd)
		_, err := req.TryAggregate()
		require.NoError(t, err)
		p, err := buildPoolState(pools[0], &staticExtra, rd, parent)
		require.NoError(t, err)
		var state Extra
		require.NoError(t, json.Unmarshal([]byte(p.Extra), &state))
		state.CvDecimalsOffset = staticExtra.CvDecimalsOffset

		settledShares := big.NewInt(325_575_695_741)
		_, volatileBudget, ok := state.previewTokenAmounts(settledShares, true)
		require.True(t, ok)
		cp := &staticExtra.CurveParams
		maxShares := cp.maxLeverageShares(&state)
		shares := state.sharesForVolatileIn(volatileBudget, maxShares)
		require.Zero(t, shares.Cmp(big.NewInt(325_591_518_529)),
			"local inversion %s must match the adapter's on-chain bisection", shares)
	})
}
