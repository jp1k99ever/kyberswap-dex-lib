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
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

func TestStateOverridesFailClosed(t *testing.T) {
	original := entity.Pool{Address: "0x0000000000000000000000000000000000000001"}
	got, err := (&PoolTracker{}).GetNewPoolStateWithOverrides(context.Background(), original,
		pool.GetNewPoolStateWithOverridesParams{Overrides: map[common.Address]gethclient.OverrideAccount{
			common.HexToAddress(original.Address): {},
		}})
	require.ErrorIs(t, err, ErrStateOverridesUnsupported)
	require.Equal(t, original, got)
}

func TestEmptyStateOverridesNormalizeToExactPath(t *testing.T) {
	for _, overrides := range []map[common.Address]gethclient.OverrideAccount{
		nil,
		{},
	} {
		normalized, err := normalizeStateOverrides(overrides)
		require.NoError(t, err)
		require.Nil(t, normalized,
			"the exact raw rv/code path is selected by nil, so empty maps must normalize to nil")
	}
}

func TestTrackerRejectsStaleConfiguredProfileBeforeRPC(t *testing.T) {
	sIn, _ := fillableCases(t)
	p := poolEntityFromFixture(t, sIn[0], nil)
	client := ethrpc.New("http://127.0.0.1:1")

	valid := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		ALMs: []ALMConfig{{Address: testALM}}}
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(p.StaticExtra), &se))
	require.NoError(t, validateTrackerProfile(p, &se, valid))

	for _, tc := range []struct {
		name string
		cfg  *Config
	}{
		{"nil config", nil},
		{"removed ALM", &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain}},
		{"changed dex", &Config{DexID: "detached", ChainID: valueobject.ChainIDBerachain,
			ALMs: valid.ALMs}},
		{"changed chain", &Config{DexID: DexType, ChainID: 1, ALMs: valid.ALMs}},
		{"changed adapter", &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain, ALMs: []ALMConfig{{
			Address: testALM, Adapter: "0x0000000000000000000000000000000000000003",
		}}}},
		{"changed gas", &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain, ALMs: []ALMConfig{{
			Address: testALM, GasStableIn: 1,
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPoolTracker(tc.cfg, client).GetNewPoolStateAtBlock(
				context.Background(), p, big.NewInt(1))
			require.ErrorIs(t, err, ErrInvalidProfile,
				"profile drift must fail before the deliberately unreachable RPC endpoint")
		})
	}
}

// An empty allocated override set is not an alternate state. This exercises the public
// path against the live fixture because the regression lived in the handoff to the raw
// implementation/rv/code probes: when the empty map was passed through as non-nil, the
// first quote remained exact but the exact inputs needed after UpdateBalance vanished.
func TestEmptyStateOverridesPreserveExactFeeState(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client, cfg := liveClient()
	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)

	tracked, err := NewPoolTracker(cfg, client).GetNewPoolStateWithOverrides(ctx, pools[0],
		pool.GetNewPoolStateWithOverridesParams{Overrides: map[common.Address]gethclient.OverrideAccount{}})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)
	require.True(t, sim.Extra.feeLawExactTracked(),
		"an empty override set must retain the same exact fee inputs as the ordinary tracker path")
}

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
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		ALMs: []ALMConfig{{Address: almAddress}}}
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

// TestFeeLawParityAgainstChain checks that the implementation-pinned rv storage word and
// public hook terms reproduce both directional samples exactly.
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
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		ALMs: []ALMConfig{{Address: almAddress}}}

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	e := &sim.Extra
	require.True(t, e.feeLawExactTracked(), "the complete exact fee state must be tracked")

	x := e.XWad.ToBig()
	rs, rv := sim.reserveStable.ToBig(), sim.reserveVolatile.ToBig()
	stableIn, volatileIn, ok := exactFeesAt(e, x, rs, rv)
	require.True(t, ok)
	require.Equal(t, e.FeeStableInWad.Dec(), stableIn.String())
	require.Equal(t, e.FeeVolatileInWad.Dec(), volatileIn.String())
	t.Logf("rv=%s reconstructs exact fees %s / %s", e.RealizedVarianceWad, stableIn, volatileIn)
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
			pausedDecoded: true, feeHookDecoded: true, resvPrice: new(big.Int).Set(bigWadFee),
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
		{"reservationPrice", func(r *rpcState) { r.resvPrice = nil }},
		{"paused decode", func(r *rpcState) { r.pausedDecoded = false }},
		{"feeHook decode", func(r *rpcState) { r.feeHookDecoded = false }},
	} {
		rd := full()
		c.drop(rd)
		_, err := buildPoolState(entity.Pool{}, rd, big.NewInt(1))
		require.Error(t, err, "an absent %s must fail the snapshot, not quote off a zero", c.name)
	}
}

// TestLazyPathPlansTheFeeLaw: the batched path cannot run a second round, so the fee-law
// terms have to be planned in the SAME round as everything else. Otherwise the batched
// path cannot attest the exact post-move fee and must disable a revisit.
func TestLazyPathPlansTheFeeLaw(t *testing.T) {
	collect := func(se *StaticExtra) map[string]bool {
		got := map[string]bool{}
		addRPCCalls(func(c *ethrpc.Call, _ []any) { got[c.Method] = true }, "0xalm", se, newRPCState())
		return got
	}

	withHook := collect(&StaticExtra{FeeHook: "0x00000000000000000000000000000000000000aa"})
	for _, m := range []string{
		hookMethodMidFee, hookMethodDirSkew, hookMethodInvSkewKappa,
		hookMethodInvSkewBand, hookMethodCurvature, hookMethodLpFee, almMethodFfadState,
	} {
		require.True(t, withHook[m], "%s must be planned in the single round", m)
	}
	require.False(t, withHook[hookMethodHotFeeFloor],
		"the optional floor is probed directly at the returned block with msg.sender=ALM")
	require.True(t, withHook[almMethodFeeHook], "the live hook is re-read to detect a swap")

	// No pinned hook: the terms are simply absent and the simulator keeps the fold.
	require.False(t, collect(&StaticExtra{})[hookMethodMidFee])
}

// TestZeroFeeHookIsBaseFeeMode: address(0) is a valid hook setting — CvammFeeLib then
// prices off the base fee alone. It has no getters, so listing must not store it as an
// address and the tracker must not plan hook reads against it; doing so failed the whole
// aggregate and left the venue untrackable.
func TestZeroFeeHookIsBaseFeeMode(t *testing.T) {
	require.Empty(t, hookOrEmpty(common.Address{}), "a zero hook must not be stored as an address")
	require.Equal(t, "0x00000000000000000000000000000000000000aa",
		hookOrEmpty(common.HexToAddress("0x00000000000000000000000000000000000000aa")))

	planned := func(se *StaticExtra) map[string]bool {
		got := map[string]bool{}
		addRPCCalls(func(c *ethrpc.Call, _ []any) { got[c.Method] = true }, "0xalm", se, newRPCState())
		return got
	}
	for _, hook := range []string{"", "0x0000000000000000000000000000000000000000"} {
		got := planned(&StaticExtra{FeeHook: hook})
		require.False(t, got[hookMethodMidFee], "no hook getter may be planned for %q", hook)
		require.True(t, got[almMethodPaused], "the venue itself is still read")
	}
}
