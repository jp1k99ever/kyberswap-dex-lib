package everlongrebalancer

import (
	"context"
	"math/big"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestLazyClosureToleratesOptionalReverts exercises the same per-call unpack closures
// pool-service invokes after a JSON-RPC batch. The two deployment-dependent calls are
// deliberately left without a result: getReservesAtReference closes leverage sizing
// without killing deleverage state. The pinned legacy runtime's absent leverageCurve is
// not planned at all; its curve comes from listing-time attestation.
func TestLazyClosureToleratesOptionalReverts(t *testing.T) {
	p := newTestPoolEntity(t)
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(p.StaticExtra), &se))
	se.UnderlyingDepositAllowlist = "0x00000000000000000000000000000000000000a1"
	raw, err := json.Marshal(se)
	require.NoError(t, err)
	p.StaticExtra = string(raw)

	tracker := NewPoolTracker(&Config{}, ethrpc.New("http://127.0.0.1:1"))
	lazy, apply, err := tracker.LazyNewPoolState(context.Background(), p, pool.GetNewPoolStateParams{})
	require.NoError(t, err)

	f := loadFixture(t)
	state := vaultStateFromFixture(t, f)
	for i, unpack := range lazy.GetUnpacks() {
		call := lazy.GetEthRpcCall(i)
		require.NotEqual(t, rebalancerMethodLeverageCurve, call.Method,
			"the allowlisted legacy implementation does not expose leverageCurve")
		if call.Method == almMethodGetReservesAtReference {
			continue // a reverted BatchElem never invokes its unpack closure
		}
		var values []any
		switch call.Method {
		case rebalancerMethodExchangeState:
			values = []any{state.Collateral, state.Debt, state.PriceWad, state.SpreadPpm}
		case almMethodGetTotalAmounts:
			values = []any{state.AlmStableReserve, state.AlmVolatileReserve}
		case erc20MethodTotalSupply:
			if common.HexToAddress(call.Target) == common.HexToAddress(se.ALM) {
				values = []any{state.AlmSupply}
			} else {
				values = []any{state.CvTotalSupply}
			}
		case cvMethodTotalAssets:
			values = []any{state.CvTotalAssets}
		case cvMethodGetWithdrawFee:
			values = []any{state.WithdrawFeeBp}
		case rebalancerMethodPhysicalCrFloor:
			values = []any{se.CurveParams.PhysicalCrFloorWad}
		case almMethodRvpsWad:
			values = []any{big.NewInt(1)}
		case cvammMethodReservationPriceWad:
			values = []any{big.NewInt(1)}
		case cvammMethodIdleStable, cvammMethodIdleVolatile, almMethodPaused:
			values = []any{new(big.Int)}
		case rebalancerMethodSettlementSwapper:
			values = []any{common.HexToAddress(se.Swapper)}
		case cvammMethodDepositAllowlist:
			values = []any{common.HexToAddress(se.UnderlyingDepositAllowlist)}
		case allowlistMethodIsDepositAllowed:
			values = []any{big.NewInt(1)}
		case rebalancerMethodManagedVault:
			values = []any{common.Address{}}
		default:
			t.Fatalf("unhandled planned call %d: %s", i, call.Method)
		}
		result, packErr := call.ABI.Methods[call.Method].Outputs.Pack(values...)
		require.NoError(t, packErr, "pack %s", call.Method)
		require.NoError(t, unpack(result), "unpack %s", call.Method)
	}

	_, err = apply(nil)
	require.ErrorIs(t, err, ErrInvalidSnapshotWord,
		"a batch worker must never apply an unpinned snapshot")
	got, err := apply(big.NewInt(123))
	require.NoError(t, err)
	var extra Extra
	require.NoError(t, json.Unmarshal([]byte(got.Extra), &extra))
	require.NotNil(t, extra.LiveCurve)
	require.Equal(t, se.CurveParams.HJoin.String(), extra.LiveCurve.HJoin.String(),
		"missing leverageCurve must retain the attested frozen curve")
	require.Zero(t, extra.RefStableReserve.Sign())
	require.Zero(t, extra.RefAssetReserve.Sign())
	require.Zero(t, se.CurveParams.maxLeverageShares(&extra).Sign(),
		"missing reference reserves must close leverage sizing")
	require.Equal(t, state.Debt.String(), extra.Debt.String(),
		"the rest of the snapshot must still be applied")
}

// TestLiveLazyBatchCallContext validates the exact low-level dispatch shape used by a
// batch worker: one eth_call BatchElem per planned call, successful elements unpacked
// independently, and applyResult run at that same block. Optional results are omitted on
// purpose even if the current reference getter happens to answer.
func TestLiveLazyBatchCallContext(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	cfg := berachainTestConfig()
	tracker := NewPoolTracker(cfg, berachainRPCClient())
	pools, _, err := NewPoolsListUpdater(cfg, berachainRPCClient()).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)
	lazy, apply, err := tracker.LazyNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)

	rpcClient, err := gethrpc.DialContext(ctx, berachainRPCURL())
	require.NoError(t, err)
	defer rpcClient.Close()
	var block hexutil.Uint64
	require.NoError(t, rpcClient.CallContext(ctx, &block, "eth_blockNumber"))
	blockTag := hexutil.EncodeUint64(uint64(block))

	results := make([]hexutil.Bytes, len(lazy.GetCallMsgs()))
	elems := make([]gethrpc.BatchElem, len(results))
	for i, msg := range lazy.GetCallMsgs() {
		elems[i] = gethrpc.BatchElem{
			Method: "eth_call",
			Args: []any{map[string]any{
				"to":   msg.To.Hex(),
				"data": hexutil.Bytes(msg.Data),
			}, blockTag},
			Result: &results[i],
		}
	}
	require.NoError(t, rpcClient.BatchCallContext(ctx, elems))
	for i, unpack := range lazy.GetUnpacks() {
		method := lazy.GetEthRpcCall(i).Method
		require.NotEqual(t, rebalancerMethodLeverageCurve, method)
		if method == almMethodGetReservesAtReference {
			continue
		}
		require.NoError(t, elems[i].Error, "batch call %d %s", i, method)
		require.NoError(t, unpack(results[i]), "unpack %d %s", i, method)
	}

	got, err := apply(new(big.Int).SetUint64(uint64(block)))
	require.NoError(t, err)
	var extra Extra
	require.NoError(t, json.Unmarshal([]byte(got.Extra), &extra))
	require.Zero(t, extra.RefStableReserve.Sign())
	require.Zero(t, extra.RefAssetReserve.Sign())
	require.Empty(t, extra.DeleverageBlockedBy,
		"omitting the leverage-only reference result must leave live deleverage available")
}
