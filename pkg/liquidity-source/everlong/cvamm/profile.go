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

func validStaticProfile(se *StaticExtra, address, exchange, poolType string, tokens []string) bool {
	if se == nil || se.ProfileVersion != cvammProfileVersion || poolType != DexType ||
		se.DexID == "" || exchange != se.DexID || len(tokens) != 2 ||
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
	wantHash, err := staticConfigHash(se)
	return err == nil && strings.EqualFold(wantHash, se.ConfigHash)
}

func validateEntityProfile(p entity.Pool, se *StaticExtra) error {
	tokens := make([]string, len(p.Tokens))
	for i, token := range p.Tokens {
		if token == nil {
			return ErrInvalidProfile
		}
		tokens[i] = token.Address
	}
	if !validStaticProfile(se, p.Address, p.Exchange, p.Type, tokens) {
		return ErrInvalidProfile
	}
	return nil
}
