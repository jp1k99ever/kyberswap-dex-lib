package everlongcvamm

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/goccy/go-json"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	poollist "github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool/list"
)

type PoolsListUpdater struct {
	config       *Config
	ethrpcClient *ethrpc.Client
}

// Metadata is the listing cursor: the set of ALMs already listed, so venues added to
// the config later are picked up without relisting the rest.
type Metadata struct {
	Listed          map[string]bool   `json:"listed"`
	Implementations map[string]string `json:"implementations,omitempty"`
}

var _ = poollist.RegisterFactoryCE(DexType, NewPoolsListUpdater)

func NewPoolsListUpdater(cfg *Config, ethrpcClient *ethrpc.Client) *PoolsListUpdater {
	return &PoolsListUpdater{
		config:       cfg,
		ethrpcClient: ethrpcClient,
	}
}

// GetNewPools lists each configured CvammALM venue exactly once. The ALM is the venue —
// pool address = ALM address — and pins the stable to leg 0 (token0) and the volatile to
// leg 1, read from the contract rather than configured. Reserves are left to the tracker.
func (u *PoolsListUpdater) GetNewPools(ctx context.Context, metadataBytes []byte) ([]entity.Pool, []byte, error) {
	metadata := Metadata{Listed: map[string]bool{}, Implementations: map[string]string{}}
	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			return nil, metadataBytes, err
		}
		if metadata.Listed == nil {
			metadata.Listed = map[string]bool{}
		}
		if metadata.Implementations == nil {
			metadata.Implementations = map[string]string{}
		}
	}

	// Pin listing and every implementation word to one head. The cursor stores the
	// implementation identity rather than only a boolean: an upgrade relists the same
	// address with freshly attested StaticExtra instead of leaving the tracker disabled
	// forever on the stale pin.
	blockNumber, err := u.ethrpcClient.GetETHClient().BlockNumber(ctx)
	if err != nil {
		return nil, metadataBytes, err
	}
	block := new(big.Int).SetUint64(blockNumber)
	implementations := make(map[string]string, len(u.config.ALMs))
	var newALMs []ALMConfig
	for _, alm := range u.config.ALMs {
		almAddress := strings.ToLower(alm.Address)
		word, err := u.ethrpcClient.GetETHClient().StorageAt(ctx,
			common.HexToAddress(alm.Address), cvammEIP1967ImplSlot, block)
		if err != nil {
			return nil, metadataBytes, err
		}
		impl := strings.ToLower(common.BytesToAddress(word).Hex())
		if common.HexToAddress(impl) == (common.Address{}) {
			return nil, metadataBytes, ErrImplementationUnpinned
		}
		code, err := u.ethrpcClient.GetETHClient().CodeAt(ctx, common.HexToAddress(impl), block)
		if err != nil {
			return nil, metadataBytes, err
		}
		if crypto.Keccak256Hash(code) != supportedImplementationCodeHash {
			return nil, metadataBytes, ErrUnsupportedImplementation
		}
		implementations[almAddress] = impl
		if !metadata.Listed[almAddress] ||
			!strings.EqualFold(metadata.Implementations[almAddress], impl) {
			newALMs = append(newALMs, alm)
		}
	}
	if len(newALMs) == 0 {
		return nil, metadataBytes, nil
	}

	tokens0 := make([]common.Address, len(newALMs))
	tokens1 := make([]common.Address, len(newALMs))
	feeHooks := make([]common.Address, len(newALMs))
	req := u.ethrpcClient.NewRequest().SetContext(ctx).SetBlockNumber(block)
	for i, alm := range newALMs {
		req.AddCall(&ethrpc.Call{
			ABI:    almABI,
			Target: alm.Address,
			Method: almMethodToken0,
		}, []any{&tokens0[i]})
		req.AddCall(&ethrpc.Call{
			ABI:    almABI,
			Target: alm.Address,
			Method: almMethodToken1,
		}, []any{&tokens1[i]})
		req.AddCall(&ethrpc.Call{
			ABI:    almABI,
			Target: alm.Address,
			Method: almMethodFeeHook,
		}, []any{&feeHooks[i]})
	}
	_, err = req.Aggregate()
	if err != nil {
		return nil, metadataBytes, err
	}

	pools := make([]entity.Pool, 0, len(newALMs))
	for i, alm := range newALMs {
		if tokens0[i] == (common.Address{}) || tokens1[i] == (common.Address{}) {
			continue
		}
		staticExtra, err := json.Marshal(StaticExtra{
			FeeHook:                hookOrEmpty(feeHooks[i]),
			Implementation:         implementations[strings.ToLower(alm.Address)],
			ImplementationCodeHash: supportedImplementationCodeHash.Hex(),
			Adapter:                strings.ToLower(alm.Adapter),
			GasStableIn:            alm.GasStableIn,
			GasVolatileIn:          alm.GasVolatileIn,
		})
		if err != nil {
			return nil, metadataBytes, err
		}

		almAddress := strings.ToLower(alm.Address)
		pools = append(pools, entity.Pool{
			Address:   almAddress,
			Exchange:  u.config.DexID,
			Type:      DexType,
			Timestamp: time.Now().Unix(),
			Tokens: []*entity.PoolToken{
				{Address: hexutil.Encode(tokens0[i][:]), Swappable: true},
				{Address: hexutil.Encode(tokens1[i][:]), Swappable: true},
			},
			Reserves:    entity.PoolReserves{"0", "0"},
			StaticExtra: string(staticExtra),
			Extra:       "{}",
			BlockNumber: blockNumber,
		})
		metadata.Listed[almAddress] = true
		metadata.Implementations[almAddress] = implementations[almAddress]
	}

	newMetadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, metadataBytes, err
	}
	return pools, newMetadataBytes, nil
}

// hookOrEmpty renders the fee hook, or "" when the ALM has none. address(0) is a valid
// configuration — CvammFeeLib then prices off the base fee alone — and it has no getters
// to read, so it must not be stored as an address the tracker would try to call.
func hookOrEmpty(hook common.Address) string {
	if hook == (common.Address{}) {
		return ""
	}
	return hexutil.Encode(hook[:])
}
