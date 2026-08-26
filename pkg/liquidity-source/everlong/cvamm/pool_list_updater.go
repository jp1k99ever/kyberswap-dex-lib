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

// Metadata is the listing cursor: venues added later and venues whose implementation or
// configured execution profile changed are emitted without relisting unaffected ALMs.
type Metadata struct {
	Listed          map[string]bool   `json:"listed"`
	Implementations map[string]string `json:"implementations,omitempty"`
	Profiles        map[string]string `json:"profiles,omitempty"`
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
	metadata := Metadata{Listed: map[string]bool{}, Implementations: map[string]string{},
		Profiles: map[string]string{}}
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
		if metadata.Profiles == nil {
			metadata.Profiles = map[string]string{}
		}
	}
	if u.config == nil || u.config.DexID == "" || u.config.ChainID == 0 {
		return nil, metadataBytes, ErrInvalidProfile
	}

	// Build the current configured set before touching RPC. Besides validating every
	// execution profile, this lets the cursor forget removed venues. Otherwise removing
	// and later re-adding the same ALM would leave Listed=true forever and suppress the
	// replacement entity the tracker now requires.
	configured := make(map[string]bool, len(u.config.ALMs))
	profiles := make(map[string]string, len(u.config.ALMs))
	for _, alm := range u.config.ALMs {
		almAddress, ok := normalizeAddress(alm.Address, false)
		if !ok || configured[almAddress] {
			return nil, metadataBytes, ErrInvalidProfile
		}
		profile, err := cvammConfigHash(u.config.DexID, u.config.ChainID, alm)
		if err != nil {
			return nil, metadataBytes, err
		}
		configured[almAddress], profiles[almAddress] = true, profile
	}
	cursorChanged := metadata.prune(configured)
	if len(configured) == 0 {
		if !cursorChanged {
			return nil, metadataBytes, nil
		}
		newMetadataBytes, err := json.Marshal(metadata)
		return nil, newMetadataBytes, err
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
		almAddress, _ := normalizeAddress(alm.Address, false)
		profile := profiles[almAddress]
		word, err := u.ethrpcClient.GetETHClient().StorageAt(ctx,
			common.HexToAddress(alm.Address), cvammEIP1967ImplSlot, block)
		if err != nil {
			return nil, metadataBytes, err
		}
		implAddress := common.BytesToAddress(word)
		if implAddress == (common.Address{}) {
			return nil, metadataBytes, ErrImplementationUnpinned
		}
		impl := hexutil.Encode(implAddress[:])
		code, err := u.ethrpcClient.GetETHClient().CodeAt(ctx, implAddress, block)
		if err != nil {
			return nil, metadataBytes, err
		}
		if crypto.Keccak256Hash(code) != supportedImplementationCodeHash {
			return nil, metadataBytes, ErrUnsupportedImplementation
		}
		implementations[almAddress] = impl
		if !metadata.matches(almAddress, impl, profile) {
			newALMs = append(newALMs, alm)
		}
	}
	if len(newALMs) == 0 {
		if !cursorChanged {
			return nil, metadataBytes, nil
		}
		newMetadataBytes, err := json.Marshal(metadata)
		return nil, newMetadataBytes, err
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
		almAddress, _ := normalizeAddress(alm.Address, false)
		token0 := hexutil.Encode(tokens0[i][:])
		token1 := hexutil.Encode(tokens1[i][:])
		adapter, _ := normalizeAddress(alm.Adapter, true)
		se := StaticExtra{
			ProfileVersion:         cvammProfileVersion,
			DexID:                  u.config.DexID,
			ChainID:                u.config.ChainID,
			ALM:                    almAddress,
			Token0:                 token0,
			Token1:                 token1,
			ConfigHash:             profiles[almAddress],
			FeeHook:                hookOrEmpty(feeHooks[i]),
			Implementation:         implementations[almAddress],
			ImplementationCodeHash: supportedImplementationCodeHash.Hex(),
			Adapter:                adapter,
			GasStableIn:            alm.GasStableIn,
			GasVolatileIn:          alm.GasVolatileIn,
		}
		se.ProfileHash, err = staticProfileHash(&se)
		if err != nil {
			return nil, metadataBytes, err
		}
		staticExtra, err := json.Marshal(se)
		if err != nil {
			return nil, metadataBytes, err
		}

		pools = append(pools, entity.Pool{
			Address:   almAddress,
			Exchange:  u.config.DexID,
			Type:      DexType,
			Timestamp: time.Now().Unix(),
			Tokens: []*entity.PoolToken{
				{Address: token0, Swappable: true},
				{Address: token1, Swappable: true},
			},
			Reserves:    entity.PoolReserves{"0", "0"},
			StaticExtra: string(staticExtra),
			Extra:       "{}",
			BlockNumber: blockNumber,
		})
		metadata.Listed[almAddress] = true
		metadata.Implementations[almAddress] = implementations[almAddress]
		metadata.Profiles[almAddress] = profiles[almAddress]
	}

	newMetadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, metadataBytes, err
	}
	return pools, newMetadataBytes, nil
}

func (m Metadata) matches(address, implementation, profile string) bool {
	return m.Listed[address] && strings.EqualFold(m.Implementations[address], implementation) &&
		strings.EqualFold(m.Profiles[address], profile)
}

func (m *Metadata) prune(configured map[string]bool) bool {
	if m == nil {
		return false
	}
	changed := false
	for address := range m.Listed {
		if !configured[address] {
			delete(m.Listed, address)
			changed = true
		}
	}
	for address := range m.Implementations {
		if !configured[address] {
			delete(m.Implementations, address)
			changed = true
		}
	}
	for address := range m.Profiles {
		if !configured[address] {
			delete(m.Profiles, address)
			changed = true
		}
	}
	return changed
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
