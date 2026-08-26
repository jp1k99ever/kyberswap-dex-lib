package everlongrebalancer

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	everlongcvamm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/cvamm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// TestBidirectionalSequentialFork executes two fills back to back on an anvil fork —
// in BOTH orders — and validates the coupled simulators' SECOND quote against the
// actual settled execution:
//
//	order A: CVAMM swap, then a rebalancer deleverage fill quoted on the folded state
//	         (leverage deliberately fails closed until the external reference gate is
//	         re-attested by a refresh after a price-moving base swap);
//	order B: rebalancer leverage fill, then a CVAMM swap quoted on the pushed base.
//
// The trading wallet is impersonated (it holds balances and approvals on both venues),
// and both the first fill and coupled second fill must settle wei-exactly. No tolerance
// is accepted: a transition that cannot be replayed exactly is not quotable.
func TestBidirectionalSequentialFork(t *testing.T) {
	test.SkipCI(t)
	if _, err := exec.LookPath("anvil"); err != nil {
		t.Skip("anvil not installed")
	}
	ctx := context.Background()

	const wallet = "0x4A964e9658792f294AF4BF923ca1A38F6FBa0896"
	forkURL := os.Getenv("EVERLONG_FORK_RPC_URL")
	if forkURL == "" {
		forkURL = berachainRPCURL()
	}

	for _, order := range []string{"cvamm-then-rebalancer", "rebalancer-then-cvamm"} {
		t.Run(order, func(t *testing.T) {
			rpcURL, stop := startAnvil(t, forkURL)
			defer stop()

			rc, err := rpc.Dial(rpcURL)
			require.NoError(t, err)
			defer rc.Close()
			geth := ethclient.NewClient(rc)
			require.NoError(t, rc.CallContext(ctx, nil, "anvil_impersonateAccount", wallet))
			require.NoError(t, rc.CallContext(ctx, nil, "anvil_setBalance", wallet, "0x1000000000000000000"))

			client := ethrpc.New(rpcURL).
				SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
			cfg := berachainTestConfig()

			pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
			require.NoError(t, err)
			var se StaticExtra
			require.NoError(t, json.Unmarshal([]byte(pools[0].StaticExtra), &se))

			tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
			require.NoError(t, err)

			cvammCfg := &everlongcvamm.Config{DexID: "everlong-cvamm", ChainID: valueobject.ChainIDBerachain,
				ALMs: []everlongcvamm.ALMConfig{{Address: se.UnderlyingCvamm}}}
			cvammPools, _, err := everlongcvamm.NewPoolsListUpdater(cvammCfg, client).GetNewPools(ctx, nil)
			require.NoError(t, err)
			cvammTracked, err := everlongcvamm.NewPoolTracker(cvammCfg, client).GetNewPoolState(ctx, cvammPools[0], pool.GetNewPoolStateParams{})
			require.NoError(t, err)
			base, err := everlongcvamm.NewPoolSimulator(cvammTracked)
			require.NoError(t, err)
			coupled, err := NewPoolSimulatorWithBases(tracked, map[string]pool.IPoolSimulator{se.UnderlyingCvamm: base})
			require.NoError(t, err)

			stable, volatile := coupled.Info.Tokens[0], coupled.Info.Tokens[1]
			almAddr := common.HexToAddress(se.UnderlyingCvamm)
			swapperAddr := common.HexToAddress(se.Swapper)
			walletAddr := common.HexToAddress(wallet)

			header, err := geth.HeaderByNumber(ctx, nil)
			require.NoError(t, err)
			deadline := new(big.Int).SetUint64(header.Time + 600)

			cvammIn := new(big.Int).Mul(big.NewInt(15), big.NewInt(1e18)) // 15 NECT stable-in
			levWBTC := big.NewInt(12_000)                                 // 12k sats leverage principal
			dlvNECT := new(big.Int).Mul(big.NewInt(20), big.NewInt(1e18)) // 20 NECT deleverage budget

			// venue call builders (same shapes the live bot settles with)
			cvammSwap := func(amountIn *big.Int) []byte {
				return packUints("0xbd34aca8", big.NewInt(1), amountIn, big.NewInt(0),
					big.NewInt(0), walletAddr.Big(), deadline)
			}
			leverage := func(si SwapInfo, maxVolatileIn *big.Int) []byte {
				// the adapter's convention: flash cap = the previewed stable leg, volatile
				// cap = the runtime amountIn
				return packUints("0x9b662ebc", si.CollVaultShares,
					si.FlashStableCap, maxVolatileIn, big.NewInt(0), walletAddr.Big())
			}
			deleverage := func(si SwapInfo, maxNetStableIn *big.Int) []byte {
				return packUints("0x5cfcefff", si.GrossStableIn,
					maxNetStableIn, big.NewInt(0), walletAddr.Big())
			}

			runCvamm := func() (*big.Int, *big.Int) { // (simOut, actualOut)
				q, err := base.CalcAmountOut(pool.CalcAmountOutParams{
					TokenAmountIn: pool.TokenAmount{Token: stable, Amount: cvammIn}, TokenOut: volatile})
				require.NoError(t, err)
				rcpt := execTx(t, rc, geth, wallet, almAddr, cvammSwap(cvammIn))
				actual := transferTo(t, rcpt.Logs, common.HexToAddress(volatile), walletAddr)
				base.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: q.SwapInfo})
				return q.TokenAmountOut.Amount, actual
			}
			runLeverage := func() (*big.Int, *big.Int) {
				q, err := coupled.CalcAmountOut(pool.CalcAmountOutParams{
					TokenAmountIn: pool.TokenAmount{Token: volatile, Amount: levWBTC}, TokenOut: stable})
				if err != nil {
					t.Skipf("leverage not quotable at fork state: %v", err)
				}
				si := q.SwapInfo.(SwapInfo)
				rcpt := execTx(t, rc, geth, wallet, swapperAddr, leverage(si, levWBTC))
				actual := transferTo(t, rcpt.Logs, common.HexToAddress(stable), walletAddr)
				coupled.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: q.SwapInfo})
				return q.TokenAmountOut.Amount, actual
			}
			runDeleverage := func() (*big.Int, *big.Int) {
				q, err := coupled.CalcAmountOut(pool.CalcAmountOutParams{
					TokenAmountIn: pool.TokenAmount{Token: stable, Amount: dlvNECT}, TokenOut: volatile})
				require.NoError(t, err)
				si := q.SwapInfo.(SwapInfo)
				rcpt := execTx(t, rc, geth, wallet, swapperAddr, deleverage(si, dlvNECT))
				actual := transferTo(t, rcpt.Logs, common.HexToAddress(volatile), walletAddr)
				coupled.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: q.SwapInfo})
				return q.TokenAmountOut.Amount, actual
			}

			var firstSim, firstActual, secondSim, secondActual *big.Int
			if order == "cvamm-then-rebalancer" {
				firstSim, firstActual = runCvamm()
				secondSim, secondActual = runDeleverage()
			} else {
				firstSim, firstActual = runLeverage()
				secondSim, secondActual = runCvamm()
			}

			require.Zero(t, firstSim.Cmp(firstActual),
				"first fill must be wei-exact (sim %s vs settled %s)", firstSim, firstActual)
			require.Zero(t, secondSim.Cmp(secondActual),
				"coupled second fill must be wei-exact (sim %s vs settled %s)", secondSim, secondActual)
			t.Logf("%s: both fills wei-exact; second quote/out %s", order, secondSim)
		})
	}
}

func packUints(selector string, args ...*big.Int) []byte {
	out := common.FromHex(selector)
	for _, a := range args {
		out = append(out, common.LeftPadBytes(a.Bytes(), 32)...)
	}
	return out
}

func execTx(t *testing.T, rc *rpc.Client, geth *ethclient.Client, from string,
	to common.Address, data []byte) *types.Receipt {
	var txHash common.Hash
	require.NoError(t, rc.CallContext(context.Background(), &txHash, "eth_sendTransaction", map[string]any{
		"from": from, "to": to.Hex(), "data": "0x" + common.Bytes2Hex(data), "gas": "0x7a1200",
	}))
	for i := 0; i < 300; i++ {
		rcpt, err := geth.TransactionReceipt(context.Background(), txHash)
		if err == nil {
			require.Equal(t, uint64(1), rcpt.Status, "fork tx reverted: %s", txHash)
			return rcpt
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("fork tx %s never mined", txHash)
	return nil
}

func transferTo(t *testing.T, logs []*types.Log, token, to common.Address) *big.Int {
	topic := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	for _, lg := range logs {
		if lg.Address == token && len(lg.Topics) == 3 && lg.Topics[0] == topic &&
			common.BytesToAddress(lg.Topics[2].Bytes()) == to {
			return new(big.Int).SetBytes(lg.Data)
		}
	}
	t.Fatalf("no %s transfer to %s in receipt", token, to)
	return nil
}

var anvilSeq atomic.Int32

func startAnvil(t *testing.T, forkURL string) (string, func()) {
	port := 8600 + os.Getpid()%200 + int(anvilSeq.Add(1))
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	cmd := exec.Command("anvil", "--fork-url", forkURL, "--port", fmt.Sprint(port), "--silent")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	stop := func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }
	for i := 0; i < 100; i++ {
		if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			_ = conn.Close()
			time.Sleep(500 * time.Millisecond) // fork warm-up
			return url, stop
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	t.Fatal("anvil never came up")
	return "", nil
}
