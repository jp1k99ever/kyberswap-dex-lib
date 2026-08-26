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
}

// Metadata is the listing cursor: the stables already listed. A single latch cannot
// express this — a stable whitelisted later must be picked up without re-emitting the
// ones already listed, and a latch does one or the other.
type Metadata struct {
	Listed map[string]bool `json:"listed"`
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
func (u *PoolsListUpdater) GetNewPools(ctx context.Context, metadataBytes []byte) ([]entity.Pool, []byte, error) {
	metadata := Metadata{Listed: map[string]bool{}}
	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			return nil, metadataBytes, err
		}
		if metadata.Listed == nil {
			metadata.Listed = map[string]bool{}
		}
	}

	var candidates []string
	for _, stable := range u.config.Stables {
		if !metadata.Listed[strings.ToLower(stable)] {
			candidates = append(candidates, stable)
		}
	}
	if len(candidates) == 0 {
		return nil, metadataBytes, nil
	}

	var (
		debtToken  gethcommon.Address
		metaCore   gethcommon.Address
		wadOffsets = make([]uint64, len(candidates))
	)
	req := u.ethrpcClient.NewRequest().SetContext(ctx)
	req.AddCall(&ethrpc.Call{
		ABI:    psmABI,
		Target: u.config.PSM,
		Method: psmMethodDebtToken,
	}, []any{&debtToken})
	req.AddCall(&ethrpc.Call{
		ABI:    psmABI,
		Target: u.config.PSM,
		Method: psmMethodMetaCore,
	}, []any{&metaCore})
	for i, stable := range candidates {
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

	if debtToken == (gethcommon.Address{}) {
		return nil, metadataBytes, ErrInvalidSnapshot // a PSM with no debt token lists nothing
	}
	debt := hexutil.Encode(debtToken[:])
	pools := make([]entity.Pool, 0, len(candidates))
	for i, stable := range candidates {
		if wadOffsets[i] == 0 { // not whitelisted (yet) — skip, re-listing picks it up
			continue
		}
		staticExtra, err := json.Marshal(StaticExtra{
			PSM:        strings.ToLower(u.config.PSM),
			MetaCore:   hexutil.Encode(metaCore[:]),
			WadOffset:  new(big.Int).SetUint64(wadOffsets[i]),
			GasDeposit: u.config.GasDeposit,
			GasRedeem:  u.config.GasRedeem,
		})
		if err != nil {
			return nil, metadataBytes, err
		}
		metadata.Listed[strings.ToLower(stable)] = true
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

	newMetadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, metadataBytes, err
	}
	return pools, newMetadataBytes, nil
}
