package everlongpsm

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/KyberNetwork/ethrpc"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/goccy/go-json"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	poollist "github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool/list"
)

type PoolsListUpdater struct {
	config       *Config
	ethrpcClient *ethrpc.Client

	hasInitialized bool
}

var _ = poollist.RegisterFactoryCE(DexType, NewPoolsListUpdater)

func NewPoolsListUpdater(cfg *Config, ethrpcClient *ethrpc.Client) *PoolsListUpdater {
	return &PoolsListUpdater{
		config:       cfg,
		ethrpcClient: ethrpcClient,
	}
}

// GetNewPools lists one pool per configured stable the PSM reports as whitelisted
// (stables(addr) != 0, which also yields the frozen decimals offset). The debt token is
// resolved from the PSM so only the PSM address and stable candidates are configuration.
func (u *PoolsListUpdater) GetNewPools(ctx context.Context, _ []byte) ([]entity.Pool, []byte, error) {
	if u.hasInitialized || len(u.config.Stables) == 0 {
		return nil, nil, nil
	}

	var (
		debtToken  gethcommon.Address
		wadOffsets = make([]uint64, len(u.config.Stables))
	)
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    psmABI,
		Target: u.config.PSM,
		Method: psmMethodDebtToken,
	}, []any{&debtToken})
	for i, stable := range u.config.Stables {
		req.AddCall(&ethrpc.Call{
			ABI:    psmABI,
			Target: u.config.PSM,
			Method: psmMethodStables,
			Params: []any{gethcommon.HexToAddress(stable)},
		}, []any{&wadOffsets[i]})
	}
	resp, err := req.Aggregate()
	if err != nil {
		return nil, nil, err
	}
	var blockNumber uint64
	if resp.BlockNumber != nil {
		blockNumber = resp.BlockNumber.Uint64()
	}

	debt := hexutil.Encode(debtToken[:])
	pools := make([]entity.Pool, 0, len(u.config.Stables))
	for i, stable := range u.config.Stables {
		if wadOffsets[i] == 0 { // not whitelisted (yet) — skip, re-listing picks it up
			continue
		}
		staticExtra, err := json.Marshal(StaticExtra{
			PSM:        strings.ToLower(u.config.PSM),
			WadOffset:  new(big.Int).SetUint64(wadOffsets[i]),
			GasDeposit: u.config.GasDeposit,
			GasRedeem:  u.config.GasRedeem,
		})
		if err != nil {
			return nil, nil, err
		}
		pools = append(pools, entity.Pool{
			// one pool per (psm, stable) pair
			Address:   strings.ToLower(u.config.PSM) + "-" + strings.ToLower(stable),
			Exchange:  u.config.DexID,
			Type:      DexType,
			Timestamp: time.Now().Unix(),
			Tokens: []*entity.PoolToken{
				{Address: debt, Swappable: true},
				{Address: strings.ToLower(stable), Swappable: true},
			},
			Reserves:    entity.PoolReserves{"0", "0"},
			StaticExtra: string(staticExtra),
			Extra:       "{}",
			BlockNumber: blockNumber,
		})
	}

	u.hasInitialized = true
	return pools, nil, nil
}
