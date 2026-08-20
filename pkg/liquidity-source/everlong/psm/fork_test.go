package everlongpsm

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// TestForkQuoteMatchesFill settles a real deposit, then a real redeem of what it minted,
// on an anvil fork and requires the simulator's quote to be wei-exact against each.
func TestForkQuoteMatchesFill(t *testing.T) {
	test.SkipCI(t)
	if _, err := exec.LookPath("anvil"); err != nil {
		t.Skip("anvil not installed")
	}
	ctx := context.Background()

	const (
		psmAddr = "0x0999417c0f9ded4356B099bcC83A16437B841323"
		honey   = "0xFCBD14DC51f0A4d49d5E53C2E0950e0bC26d0Dce"
		whale   = "0x24147243f9c08d835C218Cda1e135f8dFD0517D0" // holds ~1.6M HONEY
	)
	forkURL := os.Getenv("EVERLONG_FORK_RPC_URL")
	if forkURL == "" {
		forkURL = berachainPsmRPCURL()
	}
	rpcURL, stop := startPsmAnvil(t, forkURL)
	defer stop()

	rc, err := rpc.Dial(rpcURL)
	require.NoError(t, err)
	defer rc.Close()
	geth := ethclient.NewClient(rc)
	require.NoError(t, rc.CallContext(ctx, nil, "anvil_impersonateAccount", whale))
	require.NoError(t, rc.CallContext(ctx, nil, "anvil_setBalance", whale, "0x1000000000000000000"))

	client := ethrpc.New(rpcURL).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		PSM: psmAddr, Stables: []string{honey}}

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)
	tracked, err := NewPoolTracker(cfg, client).GetNewPoolState(ctx, pools[0], pool.GetNewPoolStateParams{})
	require.NoError(t, err)
	sim, err := NewPoolSimulator(tracked)
	require.NoError(t, err)

	debt, stable := sim.Info.Tokens[0], sim.Info.Tokens[1]
	psm, whaleAddr := common.HexToAddress(psmAddr), common.HexToAddress(whale)
	maxFee := big.NewInt(65535) // uint16 max — route-level minReturn guards slippage

	// deposit: 100 HONEY -> NECT
	amountIn := new(big.Int).Mul(big.NewInt(100), big.NewInt(1e18))
	q, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: stable, Amount: amountIn}, TokenOut: debt})
	require.NoError(t, err)

	psmExecTx(t, rc, geth, whale, common.HexToAddress(stable),
		packPsm("0x095ea7b3", psm.Big(), amountIn))
	rcpt := psmExecTx(t, rc, geth, whale, psm,
		packPsm("0xe8eda9df", common.HexToAddress(stable).Big(), amountIn, whaleAddr.Big(), maxFee))
	minted := psmTransferTo(t, rcpt.Logs, common.HexToAddress(debt), whaleAddr)

	require.Zero(t, q.TokenAmountOut.Amount.Cmp(minted),
		"deposit quote %s vs settled %s", q.TokenAmountOut.Amount, minted)
	sim.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: q.SwapInfo})

	// redeem the minted NECT back; the deposit is what funded the reserve
	q2, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: debt, Amount: minted}, TokenOut: stable})
	require.NoError(t, err)

	rcpt = psmExecTx(t, rc, geth, whale, psm,
		packPsm("0x61fc0368", common.HexToAddress(stable).Big(), minted, whaleAddr.Big(), maxFee))
	returned := psmTransferTo(t, rcpt.Logs, common.HexToAddress(stable), whaleAddr)

	require.Zero(t, q2.TokenAmountOut.Amount.Cmp(returned),
		"redeem quote %s vs settled %s", q2.TokenAmountOut.Amount, returned)
	t.Logf("deposit %s -> %s NECT; redeem back -> %s HONEY (both wei-exact)",
		amountIn, minted, returned)
}

func packPsm(selector string, args ...*big.Int) []byte {
	out := common.FromHex(selector)
	for _, a := range args {
		out = append(out, common.LeftPadBytes(a.Bytes(), 32)...)
	}
	return out
}

func psmExecTx(t *testing.T, rc *rpc.Client, geth *ethclient.Client, from string,
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

func psmTransferTo(t *testing.T, logs []*types.Log, token, to common.Address) *big.Int {
	topic := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	total := new(big.Int)
	for _, lg := range logs {
		if lg.Address == token && len(lg.Topics) == 3 && lg.Topics[0] == topic &&
			common.BytesToAddress(lg.Topics[2].Bytes()) == to {
			total.Add(total, new(big.Int).SetBytes(lg.Data))
		}
	}
	require.True(t, total.Sign() > 0, "no %s transfer to %s in receipt", token, to)
	return total
}

func startPsmAnvil(t *testing.T, forkURL string) (string, func()) {
	port := 8800 + os.Getpid()%150
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	cmd := exec.Command("anvil", "--fork-url", forkURL, "--port", fmt.Sprint(port), "--silent")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	stop := func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }
	for i := 0; i < 100; i++ {
		if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond); err == nil {
			_ = conn.Close()
			time.Sleep(500 * time.Millisecond)
			return url, stop
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	t.Fatal("anvil never came up")
	return "", nil
}
