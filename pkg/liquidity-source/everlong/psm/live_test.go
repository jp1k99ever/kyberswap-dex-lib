package everlongpsm

import (
	"context"
	"errors"
	"math/big"
	"os"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

const (
	livePSM   = "0x0999417c0f9ded4356B099bcC83A16437B841323"
	liveHoney = "0xFCBD14DC51f0A4d49d5E53C2E0950e0bC26d0Dce"
	liveNECT  = "0x1cE0a25D13CE4d52071aE7e02Cf1F6606F4C79d3"
	// Read-only policy probes only. Neither address is a production adapter config: the
	// adapter parity test deploys the actual execution contract before listing and binds
	// FeeCaller to it. The ordinary contract currently pays 5/5; the savings vault is an
	// actual PSM caller with an explicit exemption and pays 0/0.
	liveOrdinaryCallerProbe = "0x27775EC38E2b394738B73C0D25f63e20063DF054"
	liveEmptyCodeCaller     = "0x1111111111111111111111111111111111111111"
	// This deployed contract is explicitly feeExempt on PsmFlatFeeHook. It proves why
	// caller identity must be pinned rather than inferred from a few ordinary callers.
	liveExemptFeeCaller = "0x3eb566c0776d250522fdb3fcc8c010a31627f655"
)

// TestLiveListTrackQuote runs lister -> tracker -> simulator against the deployed
// Berachain PermissionlessPSM. The deployment's capacity words are governance-settable,
// so quotes must either succeed or fail with a documented sentinel — a decode error or
// panic is the only failure mode this test rejects.
func TestLiveListTrackQuote(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{
		DexID:     DexType,
		ChainID:   valueobject.ChainIDBerachain,
		PSM:       livePSM,
		Stables:   []string{liveHoney},
		FeeCaller: liveProductionAdapter(t),
	}

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1, "HONEY is whitelisted on the deployed PSM")

	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)

	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)
	debt, stable := sim.Info.Tokens[0], sim.Info.Tokens[1]

	// The configured non-exempt contract currently pays the flat 5/5 bp production rate.
	e := sim.Extra
	require.NotNil(t, e.EntryFeeBp, "entry rate must decode through feeBpFor")
	require.NotNil(t, e.ExitFeeBp, "exit rate must decode through feeBpFor")
	require.Equal(t, "5", e.EntryFeeBp.String())
	require.Equal(t, "5", e.ExitFeeBp.String())
	require.True(t, e.EntryFeeBp.Cmp(bigBp) < 0 && e.ExitFeeBp.Cmp(bigBp) < 0,
		"rates are bounded below 100%%: entry %s exit %s", e.EntryFeeBp, e.ExitFeeBp)
	require.NotNil(t, e.AvailableMint)
	require.NotNil(t, e.AvailableReserve)
	t.Logf("live psm: entry %s bp, exit %s bp, availableMint %s, availableReserve %s",
		e.EntryFeeBp, e.ExitFeeBp, e.AvailableMint, e.AvailableReserve)

	sentinels := []error{ErrPaused, ErrCapExhausted, ErrNothingToRedeem, ErrZeroAmountOut, ErrFeeUnavailable}
	quote := func(tokenIn, tokenOut string, amountIn *big.Int) {
		res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
			TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn},
			TokenOut:      tokenOut,
		})
		if err == nil {
			require.True(t, res.TokenAmountOut.Amount.Sign() > 0)
			return
		}
		for _, s := range sentinels {
			if errors.Is(err, s) {
				return
			}
		}
		t.Fatalf("quote %s->%s failed outside the documented sentinels: %v", tokenIn, tokenOut, err)
	}
	quote(stable, debt, big.NewInt(1e18)) // deposit: 1 HONEY -> NECT
	quote(debt, stable, big.NewInt(1e18)) // redeem: 1 NECT -> HONEY
}

func berachainPsmRPCURL() string {
	if url := os.Getenv("EVERLONG_PSM_RPC_URL"); url != "" {
		return url
	}
	return "https://rpc.berachain.com"
}

func liveProductionAdapter(t *testing.T) string {
	t.Helper()
	address := os.Getenv("EVERLONG_PSM_FEE_CALLER")
	if address == "" {
		t.Skip("EVERLONG_PSM_FEE_CALLER not set: no production EverlongPsmAdapter deployment is recorded")
	}
	require.True(t, common.IsHexAddress(address))
	return address
}

// TestFeeIsCallerBound proves the live hook is not caller-invariant: an ordinary
// contract pays 5/5 bp while an explicitly exempt contract pays 0/0. The tracker must
// sample the exact execution address and must not substitute address(0) or an EOA.
func TestFeeIsCallerBound(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()

	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	var bonded bool
	_, err := client.NewRequest().SetContext(ctx).AddCall(&ethrpc.Call{
		ABI: debtTokenABI, Target: liveNECT, Method: debtTokenMethodPSMBonds,
		Params: []any{common.HexToAddress(livePSM)},
	}, []any{&bonded}).Aggregate()
	require.NoError(t, err)
	require.True(t, bonded, "revoking DebtToken.PSMBonds closes both swap directions")

	var rates []string
	for _, caller := range []string{liveOrdinaryCallerProbe, liveExemptFeeCaller} {
		entry, exit := new(big.Int), new(big.Int)
		_, err = client.NewRequest().SetContext(ctx).
			AddCall(&ethrpc.Call{ABI: psmABI, Target: livePSM, Method: psmMethodFeeBpFor,
				Params: []any{common.HexToAddress(caller), common.HexToAddress(liveHoney), true}}, []any{&entry}).
			AddCall(&ethrpc.Call{ABI: psmABI, Target: livePSM, Method: psmMethodFeeBpFor,
				Params: []any{common.HexToAddress(caller), common.HexToAddress(liveHoney), false}}, []any{&exit}).
			Aggregate()
		require.NoError(t, err)
		rates = append(rates, entry.String()+"/"+exit.String())
	}
	require.Equal(t, []string{"5/5", "0/0"}, rates)
}

// TestListerRejectsEmptyCodeFeeCaller guards the operator boundary: FeeCaller is an
// execution contract, not an EOA or placeholder. Its code is checked at the exact block
// used for the topology/rate snapshot.
func TestListerRejectsEmptyCodeFeeCaller(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		PSM: livePSM, Stables: []string{liveHoney}, FeeCaller: liveEmptyCodeCaller}

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.ErrorIs(t, err, ErrUnsupportedProfile)
	require.Empty(t, pools)
}

// TestTrackerStampsBlockNumber: the lister and tracker snapshots must carry the exact
// block shared by Multicall state and out-of-band code attestation. A zero-block cache
// is not a request for repair: it is detached state and must fail before RPC.
func TestTrackerStampsBlockNumber(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		PSM: livePSM, Stables: []string{liveHoney}, FeeCaller: liveProductionAdapter(t)}

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.NotZero(t, pools[0].BlockNumber, "the lister must stamp the block it read at")

	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	require.NotZero(t, tracked.BlockNumber, "the tracker must stamp the block it read at")

	detached := pools[0]
	detached.BlockNumber = 0
	_, err = NewPoolTracker(cfg, client).GetNewPoolState(ctx, detached, pool.GetNewPoolStateParams{})
	require.ErrorIs(t, err, ErrProfileChanged)
}

// TestListerProfileCursor re-attests the live profile on every poll, emits nothing when
// its static fingerprint is unchanged, and upgrades the old map-only cursor once.
func TestListerProfileCursor(t *testing.T) {
	test.SkipCI(t)
	ctx := context.Background()
	client := ethrpc.New(berachainPsmRPCURL()).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		PSM: livePSM, Stables: []string{liveHoney}, FeeCaller: liveProductionAdapter(t)}

	u := NewPoolsListUpdater(cfg, client)
	first, meta, err := u.GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, first, 1, "only the whitelisted stable lists")

	second, meta2, err := u.GetNewPools(ctx, meta)
	require.NoError(t, err)
	require.Empty(t, second, "an already-listed pool must not be emitted again")

	var m Metadata
	require.NoError(t, json.Unmarshal(meta2, &m))
	require.NotEmpty(t, m.Profile)

	legacy := []byte(`{"listed":{"0xfcbd14dc51f0a4d49d5e53c2e0950e0bc26d0dce":true}}`)
	relisted, _, err := u.GetNewPools(ctx, legacy)
	require.NoError(t, err)
	require.Len(t, relisted, 1, "old metadata must relist to acquire profile attestation")
}
