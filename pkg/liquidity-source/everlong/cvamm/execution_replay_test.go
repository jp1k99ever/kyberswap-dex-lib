package everlongcvamm

import (
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/test"
)

// TestReplayLiveSwaps replays every swap settled on the Berachain CvammALM so far
// against the local port: the venue words are read at the swap's PARENT block, the
// simulator quotes the settled amountInUsed, and the output must match the settled
// net amountOut to the wei (the Swap event's positive leg is amountInUsed, the negative
// leg the net post-fee payout — see CvammSwapLib._emitFill).
//
// The txs are immutable chain facts; only the RPC is environmental
// (EVERLONG_CVAMM_RPC_URL overrides the public default).
func TestReplayLiveSwaps(t *testing.T) {
	test.SkipCI(t)

	txHashes := []string{
		// deployment-day session (blocks 24710246..24719612)
		"0x143c77be4357b374e76ad70b7363accb9ac2654487a05624fe6887ca3fa191ac",
		"0x6a8293f6eb14bbdee6ed5809e3cd672315aacda62efd9ed37b688c0ddaae4a27",
		"0x3b73544ead385d06fdc3e6b61b8ba29fbbc3849f956b0e5f9e65f21fa778a743",
		"0x2c1bfd6da4bf994536e0b8db66d1875fea450e6926b9ea388ed8731d45b12e7f",
		"0x62936296f29e016612e4671125577bf8d85f1ce50d218cff49d929ef6f7328a2",
		"0x4f9ebb607005a43a7076df63e1c8a75f9868478a8a459b373e89163e7fdcfde4",
		"0xc3183a996e9ca94d9ecf1468f00c7c1f64e3b9d4e5d18757098cf0a0a8b3b16b",
		"0x038625494955914475d493987aed6eb6c0d2205aaca1aec319874b4eae76c83f",
		"0xb8c179066e9328ed8673ac191ca4a4ed875b6ea7d94f8f4b852a85fe602eac15",
		"0x799600c8e290d5fc800a69a04361bcffc5f77584a1b9b2f679eedf71d5898021",
		"0x388514c6d8d9814ba47af60ab02b505a4d6e8b3d4c35f33209378d3c2d3b31f8",
		// later session (blocks 24731620..24731626)
		"0xd7c3bf5ac3f8199fcfca391ad9cf768cf1295288969e5bfb49ebd536035fbf1f",
		"0x696ec5799e421ec0b7634983bc89bf9644472441ad26d632939f9463ea6cac7f",
		"0xe7eff1c560d0e7adf066bc56a31ffd0a2305947335787162591796f9aecf10ba",
		"0xd519962018cefb0da45e9b7e34d7f420f55b58c6085af882e15b24d4a77faca5",
		// post-FFAD-upgrade sessions (V2 impl on the same proxy: dynamic directional fees
		// via the fee hook, keeper repeg): first V2 fill, then a consecutive-block burst
		// including states where the directional fees flipped between blocks — the fee is
		// read at the parent block, so replay parity here proves the read-fees-per-block
		// model holds under the hook (blocks 24842443..24852484)
		"0xba262821e368c77fa5b3ae72e55b34b002dbcb5cd3a6a3fb500e010a2ce970ae",
		"0x1e7598b86bddc85bf5a3860156e68ea3646e7a583c82215638cf1a8fde36c19e",
		"0x4cc99354814179f54f2ccd974fa4c7ae09005a64503fca2cd103877a5f293279",
		"0xed2045b937647b2a8e1836b30d3b0908652dbc156289086e61f90a3cc5ced313",
		"0xa508114796045b1813e913e6e3c8390baa66c91aaa9cfad32e45f8e576f37e19",
		"0x4e2937632261bc9d7ef6ec9e45a967140bd99fe2a4df521e8e3a4a0e0d889cc2",
		"0x8178d2b5424a0bc0a78d8eec8feb105159f9fc4d71e692ea1bcffb17537cf5ca",
		"0x12c0d42e916f376d6c7bb71931ecc85bfb02ad65d61e25949c0f0c788d9c91a5",
		"0xcd53afbdf9d3cc620699806379b1ebda9299a8ce5947e186bd76e892795bac3a",
		"0x65070ab291ac99f6532fada81feb606c0e5ba19b4e067395da7aaa3a8e5a2e72",
		"0xbb887e4b91d979e0e2c67e138d18161c47480512ce1a256f17a95dd552c7d9d8",
	}

	rpcURL := os.Getenv("EVERLONG_CVAMM_RPC_URL")
	if rpcURL == "" {
		rpcURL = "https://rpc.berachain.com"
	}
	const almAddress = "0xF5124F5605ce1e91A7429B837b7daC8f9E5378dd"
	client := ethrpc.New(rpcURL).
		SetMulticallContract(common.HexToAddress("0xcA11bde05977b3631167028862bE2a173976CA11"))
	cfg := &Config{DexID: DexType, ALMs: []ALMConfig{{Address: almAddress}}}
	ctx := context.Background()

	pools, _, err := NewPoolsListUpdater(cfg, client).GetNewPools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, pools, 1)

	geth, err := ethclient.Dial(rpcURL)
	require.NoError(t, err)
	defer geth.Close()

	alm := common.HexToAddress(almAddress)
	swapTopic := common.HexToHash("0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67")
	for _, txHash := range txHashes {
		t.Run(txHash[:10], func(t *testing.T) {
			receipt, err := geth.TransactionReceipt(ctx, common.HexToHash(txHash))
			require.NoError(t, err)

			var amount0, amount1 *big.Int
			for _, lg := range receipt.Logs {
				if lg.Address != alm || len(lg.Topics) == 0 || lg.Topics[0] != swapTopic {
					continue
				}
				amount0 = new(big.Int).SetBytes(lg.Data[0:32])
				amount1 = new(big.Int).SetBytes(lg.Data[32:64])
				two255 := new(big.Int).Lsh(big.NewInt(1), 255)
				for _, a := range []*big.Int{amount0, amount1} {
					if a.Cmp(two255) >= 0 { // int256 two's complement
						a.Sub(a, new(big.Int).Lsh(big.NewInt(1), 256))
					}
				}
				break
			}
			require.NotNil(t, amount0, "ALM Swap event not found in receipt")

			stableIn := amount0.Sign() > 0
			amountInUsed, amountOut := amount0, new(big.Int).Neg(amount1)
			if !stableIn {
				amountInUsed, amountOut = amount1, new(big.Int).Neg(amount0)
			}

			// Venue words pinned to the swap's parent block.
			parent := new(big.Int).Sub(receipt.BlockNumber, big.NewInt(1))
			rd := newRPCState()
			req := client.NewRequest().SetContext(ctx).SetBlockNumber(parent)
			addRPCCalls(func(c *ethrpc.Call, o []any) { req.AddCall(c, o) }, pools[0].Address, rd)
			_, err = req.Aggregate()
			require.NoError(t, err)
			p, err := buildPoolState(pools[0], rd, parent)
			require.NoError(t, err)

			sim, err := NewPoolSimulator(p)
			require.NoError(t, err)

			tokenIn, tokenOut := sim.Info.Tokens[0], sim.Info.Tokens[1]
			if !stableIn {
				tokenIn, tokenOut = tokenOut, tokenIn
			}
			res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
				TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountInUsed},
				TokenOut:      tokenOut,
			})
			require.NoError(t, err, "local port rejected a swap the venue settled (block %s)", receipt.BlockNumber)
			require.Zero(t, res.TokenAmountOut.Amount.Cmp(amountOut),
				"out: local %s vs settled %s", res.TokenAmountOut.Amount, amountOut)
			require.Zero(t, res.RemainingTokenAmountIn.Amount.Sign(),
				"settled amountInUsed must be fully consumed, remaining %s", res.RemainingTokenAmountIn.Amount)

			var extra Extra
			require.NoError(t, json.Unmarshal([]byte(p.Extra), &extra))
			t.Logf("block %s stableIn=%v: in=%s out=%s replayed wei-exact",
				receipt.BlockNumber, stableIn, amountInUsed, amountOut)
		})
	}
}
