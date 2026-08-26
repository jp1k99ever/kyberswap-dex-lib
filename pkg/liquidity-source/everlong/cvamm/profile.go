package everlongcvamm

import (
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/goccy/go-json"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// cvammListingProfile is the configured identity behind a listed pool. It deliberately
// excludes the fee hook: feeHook is governance-rotatable and the tracker re-pins it at a
// block. Changes to any configured execution/gas identity relist the address instead of
// leaving stale StaticExtra behind.
type cvammListingProfile struct {
	Version       uint64              `json:"version"`
	DexID         string              `json:"dexId"`
	ChainID       valueobject.ChainID `json:"chainId"`
	ALM           string              `json:"alm"`
	Adapter       string              `json:"adapter"`
	GasStableIn   int64               `json:"gasStableIn"`
	GasVolatileIn int64               `json:"gasVolatileIn"`
}

// cvammStaticProfile binds every immutable field a decoded simulator trusts. The
// config-only cursor above cannot include tokens or implementation because those are
// discovered on-chain after the relist decision; keeping a second full digest prevents
// a cache cross-wire from changing both the advertised entity and matching StaticExtra.
type cvammStaticProfile struct {
	Version                uint64              `json:"version"`
	DexID                  string              `json:"dexId"`
	ChainID                valueobject.ChainID `json:"chainId"`
	ALM                    string              `json:"alm"`
	Token0                 string              `json:"token0"`
	Token1                 string              `json:"token1"`
	Implementation         string              `json:"implementation"`
	ImplementationCodeHash string              `json:"implementationCodeHash"`
	Adapter                string              `json:"adapter"`
	GasStableIn            int64               `json:"gasStableIn"`
	GasVolatileIn          int64               `json:"gasVolatileIn"`
}

func normalizeAddress(raw string, optional bool) (string, bool) {
	if raw == "" && optional {
		return "", true
	}
	if !common.IsHexAddress(raw) {
		return "", false
	}
	address := common.HexToAddress(raw)
	if address == (common.Address{}) {
		if optional {
			return "", true
		}
		return "", false
	}
	return hexutil.Encode(address[:]), true
}

func cvammConfigHash(dexID string, chainID valueobject.ChainID, alm ALMConfig) (string, error) {
	if dexID == "" || chainID == 0 || alm.GasStableIn < 0 || alm.GasVolatileIn < 0 {
		return "", ErrInvalidProfile
	}
	almAddress, ok := normalizeAddress(alm.Address, false)
	if !ok {
		return "", ErrInvalidProfile
	}
	adapter, ok := normalizeAddress(alm.Adapter, true)
	if !ok {
		return "", ErrInvalidProfile
	}
	b, err := json.Marshal(cvammListingProfile{
		Version:       cvammProfileVersion,
		DexID:         dexID,
		ChainID:       chainID,
		ALM:           almAddress,
		Adapter:       adapter,
		GasStableIn:   alm.GasStableIn,
		GasVolatileIn: alm.GasVolatileIn,
	})
	if err != nil {
		return "", err
	}
	return crypto.Keccak256Hash(b).Hex(), nil
}

func staticConfigHash(se *StaticExtra) (string, error) {
	if se == nil {
		return "", ErrInvalidProfile
	}
	return cvammConfigHash(se.DexID, se.ChainID, ALMConfig{
		Address:       se.ALM,
		Adapter:       se.Adapter,
		GasStableIn:   se.GasStableIn,
		GasVolatileIn: se.GasVolatileIn,
	})
}

func staticProfileHash(se *StaticExtra) (string, error) {
	if se == nil {
		return "", ErrInvalidProfile
	}
	alm, ok0 := normalizeAddress(se.ALM, false)
	token0, ok1 := normalizeAddress(se.Token0, false)
	token1, ok2 := normalizeAddress(se.Token1, false)
	implementation, ok3 := normalizeAddress(se.Implementation, false)
	adapter, ok4 := normalizeAddress(se.Adapter, true)
	if !ok0 || !ok1 || !ok2 || !ok3 || !ok4 || token0 == token1 {
		return "", ErrInvalidProfile
	}
	b, err := json.Marshal(cvammStaticProfile{
		Version:                se.ProfileVersion,
		DexID:                  se.DexID,
		ChainID:                se.ChainID,
		ALM:                    alm,
		Token0:                 token0,
		Token1:                 token1,
		Implementation:         implementation,
		ImplementationCodeHash: strings.ToLower(se.ImplementationCodeHash),
		Adapter:                adapter,
		GasStableIn:            se.GasStableIn,
		GasVolatileIn:          se.GasVolatileIn,
	})
	if err != nil {
		return "", err
	}
	return crypto.Keccak256Hash(b).Hex(), nil
}

func validStaticProfile(se *StaticExtra, address, exchange, poolType string, blockNumber uint64,
	tokens []string) bool {
	if se == nil || se.ProfileVersion != cvammProfileVersion || poolType != DexType ||
		se.DexID == "" || se.ChainID == 0 || exchange != se.DexID || blockNumber == 0 || len(tokens) != 2 ||
		!strings.EqualFold(se.ImplementationCodeHash, supportedImplementationCodeHash.Hex()) {
		return false
	}
	alm, ok := normalizeAddress(se.ALM, false)
	if !ok || !strings.EqualFold(alm, address) {
		return false
	}
	implementation, ok := normalizeAddress(se.Implementation, false)
	if !ok || implementation == "" {
		return false
	}
	token0, ok0 := normalizeAddress(se.Token0, false)
	token1, ok1 := normalizeAddress(se.Token1, false)
	listed0, ok2 := normalizeAddress(tokens[0], false)
	listed1, ok3 := normalizeAddress(tokens[1], false)
	if !ok0 || !ok1 || !ok2 || !ok3 || token0 == token1 ||
		!strings.EqualFold(token0, listed0) || !strings.EqualFold(token1, listed1) {
		return false
	}
	wantConfigHash, err := staticConfigHash(se)
	if err != nil || !strings.EqualFold(wantConfigHash, se.ConfigHash) {
		return false
	}
	wantProfileHash, err := staticProfileHash(se)
	return err == nil && strings.EqualFold(wantProfileHash, se.ProfileHash)
}

func validateEntityProfile(p entity.Pool, se *StaticExtra) error {
	tokens := make([]string, len(p.Tokens))
	for i, token := range p.Tokens {
		if token == nil {
			return ErrInvalidProfile
		}
		tokens[i] = token.Address
	}
	if !validStaticProfile(se, p.Address, p.Exchange, p.Type, p.BlockNumber, tokens) {
		return ErrInvalidProfile
	}
	return nil
}

// validateTrackerProfile binds a persisted entity to the tracker configuration that is
// about to refresh it. StaticExtra is self-describing so the router can validate a
// decoded simulator without carrying source config, but self-consistency alone cannot
// retire an ALM removed from config or reject an old adapter/gas profile during a
// rotation. The tracker has that authority and must fail the stale entity before RPC.
func validateTrackerProfile(p entity.Pool, se *StaticExtra, cfg *Config) error {
	if err := validateEntityProfile(p, se); err != nil {
		return err
	}
	if cfg == nil || cfg.DexID != se.DexID || cfg.ChainID != se.ChainID {
		return ErrInvalidProfile
	}

	matched := false
	for _, alm := range cfg.ALMs {
		address, ok := normalizeAddress(alm.Address, false)
		if !ok || !strings.EqualFold(address, se.ALM) {
			continue
		}
		// Duplicate entries are ambiguous if their execution profiles differ. Matching
		// the persisted fingerprint against every occurrence makes either shape fail
		// closed instead of selecting one by order.
		wantHash, err := cvammConfigHash(cfg.DexID, cfg.ChainID, alm)
		if err != nil || !strings.EqualFold(wantHash, se.ConfigHash) {
			return ErrInvalidProfile
		}
		matched = true
	}
	if !matched {
		return ErrInvalidProfile
	}
	return nil
}
