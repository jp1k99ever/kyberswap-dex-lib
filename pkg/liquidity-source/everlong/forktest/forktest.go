// Package forktest is the shared harness for proving the Everlong simulators against
// executor-path fills on an anvil fork: it starts anvil, deploys the ks-dex-adapter-lib
// adapter from its forge artifact, funds the adapter the way the KyberSwap executor does
// (input tokens transferred in before the call) and runs execute<Dex>, returning exactly
// what the executor would see.
package forktest

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"
)

// Deployer is anvil's first unlocked account (funded, no impersonation needed).
const Deployer = "0xf39Fd6e51aad88F6F4ce6aB8827279cffFb92266"

// Multicall3 on Berachain.
const Multicall3 = "0xcA11bde05977b3631167028862bE2a173976CA11"

// adapterABI covers the three adapters: same (bytes,uint256,address,address,address)
// -> (uint256 amountUnused, uint256 amountOut) shape, different names.
const adapterABIJSON = `[
 {"type":"function","name":"executeEverlongCvamm","stateMutability":"payable","inputs":[{"name":"data","type":"bytes"},{"name":"amountIn","type":"uint256"},{"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},{"name":"recipient","type":"address"}],"outputs":[{"name":"amountUnused","type":"uint256"},{"name":"amountOut","type":"uint256"}]},
 {"type":"function","name":"executeEverlongRebalancer","stateMutability":"payable","inputs":[{"name":"data","type":"bytes"},{"name":"amountIn","type":"uint256"},{"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},{"name":"recipient","type":"address"}],"outputs":[{"name":"amountUnused","type":"uint256"},{"name":"amountOut","type":"uint256"}]},
 {"type":"function","name":"executeEverlongPsm","stateMutability":"payable","inputs":[{"name":"data","type":"bytes"},{"name":"amountIn","type":"uint256"},{"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},{"name":"recipient","type":"address"}],"outputs":[{"name":"amountUnused","type":"uint256"},{"name":"amountOut","type":"uint256"}]},
 {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"who","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
 {"type":"function","name":"transfer","stateMutability":"nonpayable","inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]}
]`

var adapterABI = func() abi.ABI {
	a, err := abi.JSON(strings.NewReader(adapterABIJSON))
	if err != nil {
		panic(err)
	}
	return a
}()

// Fork is one anvil fork with its clients.
type Fork struct {
	URL  string
	RPC  *rpc.Client
	Geth *ethclient.Client
	stop func()
}

// Start forks forkURL on a fresh anvil; skips the test when anvil is missing.
func Start(t *testing.T, forkURL string) *Fork {
	t.Helper()
	if _, err := exec.LookPath("anvil"); err != nil {
		t.Skip("anvil not installed")
	}
	url, stop := startAnvil(t, forkURL)
	rc, err := rpc.Dial(url)
	require.NoError(t, err)
	f := &Fork{URL: url, RPC: rc, Geth: ethclient.NewClient(rc), stop: stop}
	t.Cleanup(f.Close)
	return f
}

func (f *Fork) Close() {
	f.RPC.Close()
	f.stop()
}

// Impersonate unlocks and funds an address on the fork.
func (f *Fork) Impersonate(t *testing.T, who string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.RPC.CallContext(ctx, nil, "anvil_impersonateAccount", who))
	require.NoError(t, f.RPC.CallContext(ctx, nil, "anvil_setBalance", who, "0x1000000000000000000"))
}

// ExecTx sends a transaction from an unlocked account and requires it to succeed.
func (f *Fork) ExecTx(t *testing.T, from string, to common.Address, data []byte) *types.Receipt {
	t.Helper()
	return f.send(t, from, &to, data)
}

func (f *Fork) send(t *testing.T, from string, to *common.Address, data []byte) *types.Receipt {
	t.Helper()
	msg := map[string]any{"from": from, "data": "0x" + common.Bytes2Hex(data), "gas": "0x7a1200"}
	if to != nil {
		msg["to"] = to.Hex()
	}
	var txHash common.Hash
	require.NoError(t, f.RPC.CallContext(context.Background(), &txHash, "eth_sendTransaction", msg))
	for i := 0; i < 300; i++ {
		rcpt, err := f.Geth.TransactionReceipt(context.Background(), txHash)
		if err == nil {
			require.Equal(t, uint64(1), rcpt.Status, "fork tx reverted: %s", txHash)
			return rcpt
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("fork tx %s never mined", txHash)
	return nil
}

// DeployAdapter deploys the named adapter from the ks-dex-adapter-lib forge artifact.
// The artifact directory comes from EVERLONG_ADAPTER_OUT, defaulting to the sibling
// checkout's `out/`; a missing artifact skips the test (run `forge build` there).
func (f *Fork) DeployAdapter(t *testing.T, name string) common.Address {
	t.Helper()
	dir := os.Getenv("EVERLONG_ADAPTER_OUT")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", "..", "..", "ks-dex-adapter-lib", "out")
	}
	path := filepath.Join(dir, name+".sol", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("adapter artifact %s not found (forge build in ks-dex-adapter-lib): %v", path, err)
	}
	var art struct {
		Bytecode struct {
			Object string `json:"object"`
		} `json:"bytecode"`
	}
	require.NoError(t, json.Unmarshal(raw, &art))
	require.NotEmpty(t, art.Bytecode.Object)
	rcpt := f.send(t, Deployer, nil, common.FromHex(art.Bytecode.Object))
	require.NotEqual(t, common.Address{}, rcpt.ContractAddress)
	return rcpt.ContractAddress
}

// Fund moves `amount` of token from an impersonated holder to `to` — the executor's
// "input tokens are already transferred to the adapter" precondition.
func (f *Fork) Fund(t *testing.T, token, holder, to common.Address, amount *big.Int) {
	t.Helper()
	f.Impersonate(t, holder.Hex())
	data, err := adapterABI.Pack("transfer", to, amount)
	require.NoError(t, err)
	f.ExecTx(t, holder.Hex(), token, data)
}

// Balance reads token.balanceOf(who) at the fork head.
func (f *Fork) Balance(t *testing.T, token, who common.Address) *big.Int {
	t.Helper()
	data, err := adapterABI.Pack("balanceOf", who)
	require.NoError(t, err)
	out, err := f.Geth.CallContract(context.Background(), ethereum.CallMsg{To: &token, Data: data}, nil)
	require.NoError(t, err)
	return new(big.Int).SetBytes(out)
}

// Fill is what the executor observes from one execute<Dex> call.
type Fill struct {
	AmountUnused *big.Int
	AmountOut    *big.Int
	GasUsed      uint64
}

// Execute runs execute<Dex> on a freshly funded adapter: funds `amountIn` of tokenIn
// into the adapter, reads the return values with eth_call, then settles the same call
// as a transaction and checks the recipient's balance moved by exactly amountOut.
func (f *Fork) Execute(t *testing.T, adapter common.Address, method string, data []byte,
	amountIn *big.Int, tokenIn, tokenOut, holder, recipient common.Address) Fill {
	t.Helper()
	inputBefore := f.Balance(t, tokenIn, adapter)
	f.Fund(t, tokenIn, holder, adapter, amountIn)

	calldata, err := adapterABI.Pack(method, data, amountIn, tokenIn, tokenOut, recipient)
	require.NoError(t, err)
	from := common.HexToAddress(Deployer)
	ret, err := f.Geth.CallContract(context.Background(),
		ethereum.CallMsg{From: from, To: &adapter, Data: calldata, Gas: 8_000_000}, nil)
	require.NoError(t, err, "adapter call reverted")
	vals, err := adapterABI.Unpack(method, ret)
	require.NoError(t, err)
	fill := Fill{AmountUnused: vals[0].(*big.Int), AmountOut: vals[1].(*big.Int)}

	before := f.Balance(t, tokenOut, recipient)
	rcpt := f.ExecTx(t, Deployer, adapter, calldata)
	fill.GasUsed = rcpt.GasUsed
	after := f.Balance(t, tokenOut, recipient)
	require.Zero(t, new(big.Int).Sub(after, before).Cmp(fill.AmountOut), "recipient delta vs returned amountOut")
	wantInputAfter := new(big.Int).Add(inputBefore, fill.AmountUnused)
	require.Zero(t, f.Balance(t, tokenIn, adapter).Cmp(wantInputAfter),
		"adapter input delta vs returned amountUnused")
	return fill
}

// TryExecute funds the adapter and eth_calls execute<Dex> without settling it: the
// error (if any) is the venue's answer to a fill the simulator refused.
func (f *Fork) TryExecute(t *testing.T, adapter common.Address, method string, data []byte,
	amountIn *big.Int, tokenIn, tokenOut, holder, recipient common.Address) error {
	t.Helper()
	// A refusal probe must not leave its funded input on a long-lived adapter. Snapshot
	// the fork so the exact same caller can be used for caller-bound policy without
	// polluting the later sequential fills.
	var snapshotID string
	require.NoError(t, f.RPC.CallContext(context.Background(), &snapshotID, "evm_snapshot"))
	defer func() {
		var reverted bool
		require.NoError(t, f.RPC.CallContext(context.Background(), &reverted, "evm_revert", snapshotID))
		require.True(t, reverted)
	}()
	f.Fund(t, tokenIn, holder, adapter, amountIn)
	calldata, err := adapterABI.Pack(method, data, amountIn, tokenIn, tokenOut, recipient)
	require.NoError(t, err)
	from := common.HexToAddress(Deployer)
	_, err = f.Geth.CallContract(context.Background(),
		ethereum.CallMsg{From: from, To: &adapter, Data: calldata, Gas: 8_000_000}, nil)
	return err
}

// PackUints ABI-encodes a selector with uint256 words.
func PackUints(selector string, args ...*big.Int) []byte {
	out := common.FromHex(selector)
	for _, a := range args {
		out = append(out, common.LeftPadBytes(a.Bytes(), 32)...)
	}
	return out
}

// Words ABI-encodes plain 32-byte words (addresses and uints) — the `data` layout every
// Everlong adapter decodes with CalldataDecoder.
func Words(vals ...any) []byte {
	var out []byte
	for _, v := range vals {
		switch x := v.(type) {
		case common.Address:
			out = append(out, common.LeftPadBytes(x.Bytes(), 32)...)
		case string:
			out = append(out, common.LeftPadBytes(common.HexToAddress(x).Bytes(), 32)...)
		case *big.Int:
			if x == nil {
				x = new(big.Int)
			}
			out = append(out, common.LeftPadBytes(x.Bytes(), 32)...)
		default:
			panic(fmt.Sprintf("unsupported word %T", v))
		}
	}
	return out
}

// TransferTo finds the ERC-20 transfer of `token` to `to` in a receipt.
func TransferTo(t *testing.T, logs []*types.Log, token, to common.Address) *big.Int {
	t.Helper()
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
	t.Helper()
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

// Berachain fixtures shared by the parity tests.
const (
	NECT  = "0x1cE0a25D13CE4d52071aE7e02Cf1F6606F4C79d3"
	WBTC  = "0x0555E30da8f98308EdB960aa94C0Db47230d2B9c"
	HONEY = "0xFCBD14DC51f0A4d49d5E53C2E0950e0bC26d0Dce"
	PSM   = "0x0999417c0f9ded4356B099bcC83A16437B841323"
	// Whale holds WBTC and HONEY; NECT is minted 1:1 through the PSM from its HONEY.
	Whale = "0x24147243f9c08d835C218Cda1e135f8dFD0517D0"
)

// MintNECT deposits HONEY from the whale into the PSM so the whale holds `amount` NECT
// (plus the PSM's fee), leaving the PSM's mint room reduced accordingly — call it
// BEFORE tracking when a test quotes the PSM.
func (f *Fork) MintNECT(t *testing.T, amount *big.Int) {
	t.Helper()
	f.Impersonate(t, Whale)
	psm := common.HexToAddress(PSM)
	honey := common.HexToAddress(HONEY)
	// gross = amount / (1 - fee) with a little slack; the PSM mints net of its fee
	gross := new(big.Int).Mul(amount, big.NewInt(101))
	gross.Div(gross, big.NewInt(100))
	f.ExecTx(t, Whale, honey, PackUints("0x095ea7b3", psm.Big(), gross))
	// deposit(address stable, uint256 stableAmount, address receiver, uint16 maxFeePercentage)
	f.ExecTx(t, Whale, psm, PackUints("0xe8eda9df", honey.Big(), gross, common.HexToAddress(Whale).Big(), big.NewInt(65535)))
	require.True(t, f.Balance(t, common.HexToAddress(NECT), common.HexToAddress(Whale)).Cmp(amount) >= 0)
}

// ForkURL is the upstream the fork is taken from.
func ForkURL(fallback string) string {
	if u := os.Getenv("EVERLONG_FORK_RPC_URL"); u != "" {
		return u
	}
	return fallback
}

// Remaining is RemainingTokenAmountIn as a number (nil -> 0).
func Remaining(amount *big.Int) *big.Int {
	if amount == nil {
		return new(big.Int)
	}
	return amount
}
