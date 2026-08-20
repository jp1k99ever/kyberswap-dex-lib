package everlongcvamm

import (
	"bytes"
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/goccy/go-json"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// Live pipeline test: lister -> tracker -> simulator against a real deployment, with a
// wei-exact parity gate against the venue itself. Env-gated so the suite stays hermetic:
//
//	EVERLONG_CVAMM_RPC_URL   RPC endpoint (required)
//	EVERLONG_CVAMM_ALM       CvammALM address (required)
//	EVERLONG_CVAMM_MULTICALL Multicall3 override (optional)
//
// THE PARITY ORACLE IS THE SWAP ITSELF. There is deliberately no CvammQuoter here:
// `eth_call` on CvammALM.swap, with state overrides granting the caller the input
// balance and allowance, returns the venue's own (amountInUsed, amountOut) without
// deploying anything and without moving chain state. That is strictly stronger than any
// view function — it is the settled fill — and it means the gate works against any RPC
// on day one. The same technique backs the checked-in vectors asserted offline by
// TestExecutionParity.
func TestLivePipeline(t *testing.T) {
	test.SkipCI(t)
	rpcURL := os.Getenv("EVERLONG_CVAMM_RPC_URL")
	almAddress := os.Getenv("EVERLONG_CVAMM_ALM")
	if rpcURL == "" || almAddress == "" {
		t.Skip("EVERLONG_CVAMM_RPC_URL / EVERLONG_CVAMM_ALM not set")
	}

	multicall := os.Getenv("EVERLONG_CVAMM_MULTICALL")
	if multicall == "" {
		multicall = "0xcA11bde05977b3631167028862bE2a173976CA11"
	}
	multicallAddr := common.HexToAddress(multicall)
	client := ethrpc.New(rpcURL).SetMulticallContract(multicallAddr)
	cfg := &Config{DexID: DexType, ALMs: []ALMConfig{{Address: almAddress}}}
	ctx := context.Background()

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)
	require.Len(t, pools[0].Tokens, 2)

	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)

	var extra Extra
	require.NoError(t, json.Unmarshal([]byte(tracked.Extra), &extra))
	require.NotNil(t, extra.XWad)
	require.NotNil(t, extra.Kappa)
	t.Logf("block=%d x=%s kappa=%s fees=(%s/%s) paused=%v reserves=%v",
		tracked.BlockNumber, extra.XWad.Dec(), extra.Kappa.Dec(),
		extra.FeeStableInWad.Dec(), extra.FeeVolatileInWad.Dec(), extra.Paused, tracked.Reserves)

	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	blockNumber := new(big.Int).SetUint64(tracked.BlockNumber)
	parityChecks := 0
	for _, stableIn := range []bool{true, false} {
		tokenIn, tokenOut := sim.Info.Tokens[0], sim.Info.Tokens[1]
		if !stableIn {
			tokenIn, tokenOut = tokenOut, tokenIn
		}

		// Size the sweep off the venue's measured input-side capacity: an over-large
		// probe fills to the band edge and reports the rest, so what it consumed IS the
		// capacity in the input token's own units.
		probe := new(big.Int).Lsh(big.NewInt(1), 180)
		probeRes, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: probe},
			TokenOut:      tokenOut,
		})
		require.NoError(t, err, "capacity probe stableIn=%v", stableIn)
		capIn := new(big.Int).Sub(probe, probeRes.RemainingTokenAmountIn.Amount)
		require.True(t, capIn.Sign() > 0, "no capacity stableIn=%v", stableIn)
		t.Logf("stableIn=%v input-side capacity=%s", stableIn, capIn)

		overrides, err := fundingOverrides(ctx, client, blockNumber, tokenIn, multicallAddr,
			common.HexToAddress(almAddress))
		require.NoError(t, err, "could not build funding overrides for %s", tokenIn)

		sizes := []*big.Int{
			new(big.Int).Add(new(big.Int).Quo(capIn, big.NewInt(1_000_000)), big.NewInt(1)),
			new(big.Int).Add(new(big.Int).Quo(capIn, big.NewInt(1000)), big.NewInt(1)),
			new(big.Int).Add(new(big.Int).Quo(capIn, big.NewInt(3)), big.NewInt(1)),
			capIn,
			new(big.Int).Mul(capIn, big.NewInt(2)), // over capacity: partial fill
		}
		for _, amountIn := range sizes {
			used, out, swapErr := callSwap(ctx, client, blockNumber, overrides, almAddress,
				multicallAddr, stableIn, amountIn)

			res, calcErr := sim.CalcAmountOut(pool.CalcAmountOutParams{
				TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn},
				TokenOut:      tokenOut,
			})
			if swapErr != nil {
				require.Error(t, calcErr,
					"stableIn=%v in=%s: the venue reverts, so the simulator must refuse", stableIn, amountIn)
				continue
			}
			require.NoError(t, calcErr, "stableIn=%v in=%s", stableIn, amountIn)
			require.Equal(t, out.String(), res.TokenAmountOut.Amount.String(),
				"stableIn=%v in=%s amountOut", stableIn, amountIn)
			gotUsed := new(big.Int).Sub(amountIn, res.RemainingTokenAmountIn.Amount)
			require.Equal(t, used.String(), gotUsed.String(),
				"stableIn=%v in=%s amountInUsed", stableIn, amountIn)
			parityChecks++
			t.Logf("parity stableIn=%v in=%s -> used=%s out=%s", stableIn, amountIn, used, out)
		}
	}
	require.GreaterOrEqual(t, parityChecks, 6, "both directions must have produced parity checks")
}

var (
	swapABI, _ = abi.JSON(bytes.NewReader([]byte(`[{
		"inputs": [
			{"internalType": "bool", "name": "stableIn", "type": "bool"},
			{"internalType": "uint256", "name": "amountIn", "type": "uint256"},
			{"internalType": "uint256", "name": "minAmountOut", "type": "uint256"},
			{"internalType": "uint160", "name": "sqrtPriceLimitX96", "type": "uint160"},
			{"internalType": "address", "name": "to", "type": "address"},
			{"internalType": "uint256", "name": "deadline", "type": "uint256"}
		],
		"name": "swap",
		"outputs": [
			{"internalType": "uint256", "name": "amountInUsed", "type": "uint256"},
			{"internalType": "uint256", "name": "amountOut", "type": "uint256"}
		],
		"stateMutability": "nonpayable",
		"type": "function"
	}]`)))
	erc20ABI, _ = abi.JSON(bytes.NewReader([]byte(`[
		{"inputs":[{"name":"a","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
		{"inputs":[{"name":"o","type":"address"},{"name":"s","type":"address"}],"name":"allowance","outputs":[{"name":"","type":"uint256"}],"stateMutability":"view","type":"function"}
	]`)))
)

// callSwap runs CvammALM.swap through eth_call under the given state overrides. The
// call executes for real inside the node — reserves move, the fee is sampled, the
// coordinate is written — but nothing is mined, so this is a settled fill's answer with
// no side effects. msg.sender is the multicall contract, which is why the overrides
// fund that address.
func callSwap(ctx context.Context, client *ethrpc.Client, blockNumber *big.Int,
	overrides map[common.Address]gethclient.OverrideAccount, alm string, caller common.Address,
	stableIn bool, amountIn *big.Int) (used, out *big.Int, err error) {
	var result struct {
		AmountInUsed *big.Int
		AmountOut    *big.Int
	}
	req := client.NewRequest().SetContext(ctx).SetBlockNumber(blockNumber).SetOverrides(overrides)
	req.AddCall(&ethrpc.Call{
		ABI:    swapABI,
		Target: alm,
		Method: "swap",
		Params: []any{stableIn, amountIn, big.NewInt(0), big.NewInt(0), caller,
			new(big.Int).Lsh(big.NewInt(1), 64)},
	}, []any{&result})
	if _, err = req.Aggregate(); err != nil {
		return nil, nil, err
	}
	if result.AmountInUsed == nil || result.AmountOut == nil {
		return nil, nil, ErrSwapExhausted
	}
	return result.AmountInUsed, result.AmountOut, nil
}

// fundingOverrides grants `holder` an effectively unlimited balance of `token` and an
// unlimited allowance for `spender`, by writing the two mapping slots directly.
//
// The slots are DISCOVERED rather than assumed: layouts differ per token even within
// one deployment (the pair here uses balance slot 0 / allowance slot 1 for the stable
// and 5 / 6 for the volatile), so a hard-coded guess would silently fund nothing and
// turn every parity check into a revert that looks like a venue refusal.
func fundingOverrides(ctx context.Context, client *ethrpc.Client, blockNumber *big.Int,
	token string, holder, spender common.Address) (map[common.Address]gethclient.OverrideAccount, error) {
	tokenAddr := common.HexToAddress(token)
	sentinel := common.BigToHash(big.NewInt(0x1234))
	huge := common.BigToHash(new(big.Int).Lsh(big.NewInt(1), 200))

	balanceSlot, err := findSlot(ctx, client, blockNumber, tokenAddr, sentinel, func(slot int64) common.Hash {
		return mappingKey(common.BytesToHash(holder.Bytes()), big.NewInt(slot))
	}, "balanceOf", holder)
	if err != nil {
		return nil, err
	}
	allowanceSlot, err := findSlot(ctx, client, blockNumber, tokenAddr, sentinel, func(slot int64) common.Hash {
		inner := mappingKey(common.BytesToHash(holder.Bytes()), big.NewInt(slot))
		return mappingKey(common.BytesToHash(spender.Bytes()), new(big.Int).SetBytes(inner.Bytes()))
	}, "allowance", holder, spender)
	if err != nil {
		return nil, err
	}

	inner := mappingKey(common.BytesToHash(holder.Bytes()), big.NewInt(allowanceSlot))
	return map[common.Address]gethclient.OverrideAccount{
		tokenAddr: {StateDiff: map[common.Hash]common.Hash{
			mappingKey(common.BytesToHash(holder.Bytes()), big.NewInt(balanceSlot)):               huge,
			mappingKey(common.BytesToHash(spender.Bytes()), new(big.Int).SetBytes(inner.Bytes())): huge,
		}},
	}, nil
}

// findSlot probes mapping slots by writing a sentinel and reading the getter back.
func findSlot(ctx context.Context, client *ethrpc.Client, blockNumber *big.Int, token common.Address,
	sentinel common.Hash, key func(int64) common.Hash, method string, args ...any) (int64, error) {
	params := make([]any, len(args))
	copy(params, args)
	for slot := int64(0); slot < 32; slot++ {
		var got *big.Int
		req := client.NewRequest().SetContext(ctx).SetBlockNumber(blockNumber).
			SetOverrides(map[common.Address]gethclient.OverrideAccount{
				token: {StateDiff: map[common.Hash]common.Hash{key(slot): sentinel}},
			})
		req.AddCall(&ethrpc.Call{ABI: erc20ABI, Target: token.Hex(), Method: method, Params: params},
			[]any{&got})
		if _, err := req.Aggregate(); err != nil {
			continue
		}
		if got != nil && got.Cmp(sentinel.Big()) == 0 {
			return slot, nil
		}
	}
	return 0, ErrInvalidToken
}

// mappingKey is solidity's keccak256(abi.encode(key, slot)) for a mapping at `slot`.
func mappingKey(key common.Hash, slot *big.Int) common.Hash {
	return crypto.Keccak256Hash(key.Bytes(), common.BigToHash(slot).Bytes())
}

// TestFeeLawParityAgainstChain checks the invariant the re-derivation actually rests on:
// it is admitted ONLY when it reproduces poolFeeDirectional. In the saturated regime the
// ported expression must match the chain exactly; outside it the guard must decline, so
// the conservative fold stands rather than a formula that no longer applies.
func TestFeeLawParityAgainstChain(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	rpcURL := os.Getenv("EVERLONG_CVAMM_RPC_URL")
	almAddress := os.Getenv("EVERLONG_CVAMM_ALM")
	if rpcURL == "" {
		rpcURL = "https://rpc.berachain.com"
	}
	if almAddress == "" {
		almAddress = "0xF5124F5605ce1e91A7429B837b7daC8f9E5378dd"
	}
	client := ethrpc.New(rpcURL).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{DexID: DexType, ALMs: []ALMConfig{{Address: almAddress}}}

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	e := &sim.Extra
	require.True(t, e.feeLawTracked(), "the hook terms must be tracked")

	x := e.XWad.ToBig()
	rs, rv := sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	exact := true
	for _, c := range []struct {
		stableIn bool
		sampled  *uint256.Int
	}{{true, e.FeeStableInWad}, {false, e.FeeVolatileInWad}} {
		bound := feeUpperBoundWad(e, x, rs, rv, c.stableIn)
		require.NotNil(t, bound)
		// The bound must hold against the chain — that is the whole safety claim.
		require.GreaterOrEqual(t, bound.Cmp(c.sampled.ToBig()), 0,
			"stableIn=%v: bound %s is BELOW chain %s", c.stableIn, bound, c.sampled)
		if bound.Cmp(c.sampled.ToBig()) != 0 {
			exact = false
		}
	}

	_, _, ok := reSampleFees(e, x, rs, rv, x, rs, rv)
	require.True(t, ok, "a holding bound must be admitted")
	if exact {
		t.Logf("regime 4 (saturated): priced exactly — stable-in %s, volatile-in %s",
			e.FeeStableInWad, e.FeeVolatileInWad)
	} else {
		t.Logf("regime 5 (base off the cap): bound holds above chain %s / %s — safe, still directional",
			e.FeeStableInWad, e.FeeVolatileInWad)
	}
}

// TestAbsentWordFailsSnapshot: nil is the only signal a read did not land. Pre-allocating
// decode targets to zero destroyed it — a fee word that failed to decode would have read
// as a legitimate 0%, silently removing the haircut and advertising a better price than
// the venue pays. Absence must fail the snapshot instead.
func TestAbsentWordFailsSnapshot(t *testing.T) {
	require.Empty(t, *newRPCState(), "newRPCState must pre-allocate nothing")

	full := func() *rpcState {
		return &rpcState{
			sup: supportRaw{AWad: new(big.Int).Mul(big.NewInt(34), bigWadFee), XLo: big.NewInt(1),
				XHi: big.NewInt(2), YHi: big.NewInt(3)},
			xWad: new(big.Int).Div(bigWadFee, big.NewInt(2)), anchor: big.NewInt(1), kappa: big.NewInt(1),
			reserveStable: big.NewInt(1), reserveVolatile: big.NewInt(1),
			feeStableIn: big.NewInt(1), feeVolatileIn: big.NewInt(1),
		}
	}
	_, err := buildPoolState(entity.Pool{}, full(), big.NewInt(1))
	require.NoError(t, err, "a complete snapshot must build")

	for _, c := range []struct {
		name string
		drop func(*rpcState)
	}{
		{"feeStableIn", func(r *rpcState) { r.feeStableIn = nil }},
		{"feeVolatileIn", func(r *rpcState) { r.feeVolatileIn = nil }},
		{"xWad", func(r *rpcState) { r.xWad = nil }},
		{"kappa", func(r *rpcState) { r.kappa = nil }},
		{"reserveStable", func(r *rpcState) { r.reserveStable = nil }},
	} {
		rd := full()
		c.drop(rd)
		_, err := buildPoolState(entity.Pool{}, rd, big.NewInt(1))
		require.Error(t, err, "an absent %s must fail the snapshot, not quote off a zero", c.name)
	}
}

// TestLazyPathPlansTheFeeLaw: the batched path cannot run a second round, so the fee-law
// terms have to be planned in the SAME round as everything else. They were not, which
// left batched snapshots permanently on the conservative fold while direct ones priced
// the reversal — two paths quoting the same pool differently.
func TestLazyPathPlansTheFeeLaw(t *testing.T) {
	collect := func(se *StaticExtra) map[string]bool {
		got := map[string]bool{}
		addRPCCalls(func(c *ethrpc.Call, _ []any) { got[c.Method] = true }, "0xalm", se, newRPCState())
		return got
	}

	withHook := collect(&StaticExtra{FeeHook: "0xhook"})
	for _, m := range []string{
		hookMethodMidFee, hookMethodDirSkew, hookMethodInvSkewKappa,
		hookMethodInvSkewBand, hookMethodCurvature, hookMethodLpFee, hookMethodHotFeeFloor,
	} {
		require.True(t, withHook[m], "%s must be planned in the single round", m)
	}
	require.True(t, withHook[almMethodFeeHook], "the live hook is re-read to detect a swap")

	// No pinned hook: the terms are simply absent and the simulator keeps the fold.
	require.False(t, collect(&StaticExtra{})[hookMethodMidFee])
}
