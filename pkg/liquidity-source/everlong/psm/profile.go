package everlongpsm

import (
	"context"
	"math/big"
	"strings"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/goccy/go-json"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

const (
	productionProfileVersion uint8 = 2
	productionListedStables        = 1
	// Direct, non-proxy PermissionlessPSM runtime deployed on Berachain at
	// 0x0999417c0f9ded4356B099bcC83A16437B841323. Pinning the venue semantics is
	// load-bearing: matching hook pointers alone cannot make an arbitrary contract's
	// preview/accounting behavior safe to simulate.
	productionPSMCodeHash = "0xbd95a90edd774ecbd8822a6258e2643379d1e84b4f23400237281460081b0564"

	// The only reviewed fee model is the deployed, non-proxy PsmFlatFeeHook. Its
	// runtime is book-independent: feeBpFor for one caller cannot move after a fill,
	// so UpdateBalance may carry the sampled rate across sequential swaps in the same
	// transaction. The scalar rates and caller policy remain mutable and are sampled;
	// the address and runtime semantics are not inferred.
	productionFeeHookAddress  = "0x43cBb9e00E91FcffeF1B510f29Bd45355F846af5"
	productionFeeHookCodeHash = "0x2890a7c2d3a22efec23d7d87311358f9059daf2bfaa1473522730d7ed38233d2"
	// Final direct-call EverlongPsmAdapter runtime from the reviewed adapter commit.
	// If production instead delegatecalls the adapter, the PSM sees the executor as
	// msg.sender; that topology is intentionally unsupported until its runtime is
	// reviewed and a new profile version is shipped.
	productionFeeCallerCodeHash = "0xbb3b9a67f1812ab434d456acdd02c65fc12621e4ed7dd4f88c20d5b3dc32ac6c"
)

// psmListingProfile binds every operator-controlled input which can change execution
// identity or gas. A pool removed from config, moved to another chain/exchange, or
// rotated to another caller/stable must be rejected by the tracker before it performs
// any RPC.
type psmListingProfile struct {
	Version    uint8               `json:"version"`
	DexID      string              `json:"dexId"`
	ChainID    valueobject.ChainID `json:"chainId"`
	PSM        string              `json:"psm"`
	Stable     string              `json:"stable"`
	FeeCaller  string              `json:"feeCaller"`
	GasDeposit int64               `json:"gasDeposit"`
	GasRedeem  int64               `json:"gasRedeem"`
}

// psmStaticProfile binds every immutable word a decoded simulator trusts. ConfigHash
// alone cannot prevent a cache cross-wire from changing both an entity token/address
// and its matching StaticExtra; ProfileHash makes that current-format mutation fail
// closed on the first quote.
type psmStaticProfile struct {
	Version           uint8               `json:"version"`
	DexID             string              `json:"dexId"`
	ChainID           valueobject.ChainID `json:"chainId"`
	ConfigHash        string              `json:"configHash"`
	PSM               string              `json:"psm"`
	PSMCodeHash       string              `json:"psmCodeHash"`
	DebtToken         string              `json:"debtToken"`
	Stable            string              `json:"stable"`
	MetaCore          string              `json:"metaCore"`
	FeeCaller         string              `json:"feeCaller"`
	FeeCallerCodeHash string              `json:"feeCallerCodeHash"`
	FeeHook           string              `json:"feeHook"`
	FeeHookCodeHash   string              `json:"feeHookCodeHash"`
	ListedStableCount int                 `json:"listedStableCount"`
	WadOffset         string              `json:"wadOffset"`
	GasDeposit        int64               `json:"gasDeposit"`
	GasRedeem         int64               `json:"gasRedeem"`
}

type profileSnapshot struct {
	PSM                 common.Address
	DebtToken           common.Address
	MetaCore            common.Address
	Stable              common.Address
	FeeCaller           common.Address
	FeeHook             common.Address
	CapHook             common.Address
	YieldHook           common.Address
	ListedStable        common.Address
	ListedStablesLength *big.Int
	WadOffset           uint64
	EntryFeeBp          *big.Int
	ExitFeeBp           *big.Int
	PSMCodeHash         common.Hash
	FeeHookCodeHash     common.Hash
	FeeCallerCodeHash   common.Hash
	PSMBonded           bool
}

// configuredAddresses makes caller identity part of the pool identity. The fee hook
// has caller discounts and explicit exemptions on-chain, so the zero address or a
// representative EOA is not a safe stand-in for the contract that will execute a fill.
func configuredAddresses(cfg *Config) (psm, stable, feeCaller common.Address, err error) {
	if cfg == nil || !common.IsHexAddress(cfg.PSM) || common.HexToAddress(cfg.PSM) == (common.Address{}) ||
		len(cfg.Stables) != productionListedStables || !common.IsHexAddress(cfg.Stables[0]) ||
		common.HexToAddress(cfg.Stables[0]) == (common.Address{}) {
		return psm, stable, feeCaller, ErrUnsupportedProfile
	}
	if !common.IsHexAddress(cfg.FeeCaller) || common.HexToAddress(cfg.FeeCaller) == (common.Address{}) {
		return psm, stable, feeCaller, ErrFeeCallerRequired
	}
	return common.HexToAddress(cfg.PSM), common.HexToAddress(cfg.Stables[0]),
		common.HexToAddress(cfg.FeeCaller), nil
}

func normalizedProfileAddress(raw string) (string, bool) {
	if !common.IsHexAddress(raw) {
		return "", false
	}
	address := common.HexToAddress(raw)
	if address == (common.Address{}) {
		return "", false
	}
	return hexutil.Encode(address[:]), true
}

func psmConfigHash(cfg *Config) (string, error) {
	psm, stable, feeCaller, err := configuredAddresses(cfg)
	if err != nil {
		return "", err
	}
	if cfg.DexID == "" || cfg.ChainID == 0 || cfg.GasDeposit < 0 || cfg.GasRedeem < 0 {
		return "", ErrUnsupportedProfile
	}
	payload, err := json.Marshal(psmListingProfile{
		Version: productionProfileVersion, DexID: cfg.DexID, ChainID: cfg.ChainID,
		PSM: hexutil.Encode(psm[:]), Stable: hexutil.Encode(stable[:]),
		FeeCaller: hexutil.Encode(feeCaller[:]), GasDeposit: cfg.GasDeposit,
		GasRedeem: cfg.GasRedeem,
	})
	if err != nil {
		return "", err
	}
	return crypto.Keccak256Hash(payload).Hex(), nil
}

func staticConfigHash(s *StaticExtra) (string, error) {
	if s == nil {
		return "", ErrUnsupportedProfile
	}
	return psmConfigHash(&Config{
		DexID: s.DexID, ChainID: s.ChainID, PSM: s.PSM,
		Stables: []string{s.Stable}, FeeCaller: s.FeeCaller,
		GasDeposit: s.GasDeposit, GasRedeem: s.GasRedeem,
	})
}

func staticProfileHash(s *StaticExtra) (string, error) {
	if s == nil || s.WadOffset == nil || s.WadOffset.Sign() <= 0 || s.DexID == "" ||
		s.ChainID == 0 || s.GasDeposit < 0 || s.GasRedeem < 0 {
		return "", ErrUnsupportedProfile
	}
	psm, ok0 := normalizedProfileAddress(s.PSM)
	debtToken, ok1 := normalizedProfileAddress(s.DebtToken)
	stable, ok2 := normalizedProfileAddress(s.Stable)
	metaCore, ok3 := normalizedProfileAddress(s.MetaCore)
	feeCaller, ok4 := normalizedProfileAddress(s.FeeCaller)
	feeHook, ok5 := normalizedProfileAddress(s.FeeHook)
	if !ok0 || !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || debtToken == stable {
		return "", ErrUnsupportedProfile
	}
	payload, err := json.Marshal(psmStaticProfile{
		Version: s.ProfileVersion, DexID: s.DexID, ChainID: s.ChainID,
		ConfigHash: strings.ToLower(s.ConfigHash), PSM: psm,
		PSMCodeHash: strings.ToLower(s.PSMCodeHash), DebtToken: debtToken,
		Stable: stable, MetaCore: metaCore, FeeCaller: feeCaller,
		FeeCallerCodeHash: strings.ToLower(s.FeeCallerCodeHash), FeeHook: feeHook,
		FeeHookCodeHash:   strings.ToLower(s.FeeHookCodeHash),
		ListedStableCount: s.ListedStableCount, WadOffset: s.WadOffset.String(),
		GasDeposit: s.GasDeposit, GasRedeem: s.GasRedeem,
	})
	if err != nil {
		return "", err
	}
	return crypto.Keccak256Hash(payload).Hex(), nil
}

func (s *profileSnapshot) validate() error {
	if s == nil || s.PSM == (common.Address{}) || s.DebtToken == (common.Address{}) ||
		s.MetaCore == (common.Address{}) || s.Stable == (common.Address{}) ||
		s.FeeCaller == (common.Address{}) || s.WadOffset == 0 ||
		s.ListedStablesLength == nil || !s.ListedStablesLength.IsInt64() ||
		s.ListedStablesLength.Int64() != productionListedStables || s.ListedStable != s.Stable ||
		!strings.EqualFold(s.PSMCodeHash.Hex(), productionPSMCodeHash) ||
		!strings.EqualFold(s.FeeHook.Hex(), productionFeeHookAddress) ||
		!strings.EqualFold(s.FeeHookCodeHash.Hex(), productionFeeHookCodeHash) ||
		!strings.EqualFold(s.FeeCallerCodeHash.Hex(), productionFeeCallerCodeHash) ||
		!s.PSMBonded || s.CapHook != (common.Address{}) || s.YieldHook != (common.Address{}) {
		return ErrUnsupportedProfile
	}
	for _, fee := range []*big.Int{s.EntryFeeBp, s.ExitFeeBp} {
		if fee == nil || fee.Sign() < 0 || fee.Cmp(bigBp) >= 0 {
			return ErrUnsupportedProfile
		}
	}
	return nil
}

// probeProfileCode batches the out-of-ABI identity checks at the Multicall block. The
// PSM and fee-hook hashes pin reviewed semantics. FeeCaller is intentionally
// address-keyed and the exact direct adapter runtime proves that the configured address
// is the reviewed execution path, not merely some unrelated contract that happens to
// have code.
func probeProfileCode(ctx context.Context, client *ethrpc.Client, psm, feeHook,
	feeCaller common.Address, block *big.Int) (common.Hash, common.Hash, common.Hash, error) {
	if client == nil || block == nil {
		return common.Hash{}, common.Hash{}, common.Hash{}, ErrInvalidSnapshot
	}
	var psmCode, hookCode, callerCode hexutil.Bytes
	blockArg := hexutil.EncodeBig(block)
	batch := []rpc.BatchElem{
		{Method: "eth_getCode", Args: []any{psm, blockArg}, Result: &psmCode},
		{Method: "eth_getCode", Args: []any{feeHook, blockArg}, Result: &hookCode},
		{Method: "eth_getCode", Args: []any{feeCaller, blockArg}, Result: &callerCode},
	}
	if err := client.GetETHClient().Client().BatchCallContext(ctx, batch); err != nil {
		return common.Hash{}, common.Hash{}, common.Hash{}, err
	}
	for _, elem := range batch {
		if elem.Error != nil {
			return common.Hash{}, common.Hash{}, common.Hash{}, elem.Error
		}
	}
	return crypto.Keccak256Hash(psmCode), crypto.Keccak256Hash(hookCode),
		crypto.Keccak256Hash(callerCode), nil
}

func probePSMBond(ctx context.Context, client *ethrpc.Client, psm, debtToken common.Address,
	block *big.Int) (bool, error) {
	if client == nil || block == nil {
		return false, ErrInvalidSnapshot
	}
	callData, err := debtTokenABI.Pack(debtTokenMethodPSMBonds, psm)
	if err != nil {
		return false, err
	}
	var result hexutil.Bytes
	err = client.GetETHClient().Client().CallContext(ctx, &result, "eth_call", map[string]any{
		"to": debtToken, "data": hexutil.Encode(callData),
	}, hexutil.EncodeBig(block))
	if err != nil {
		return false, err
	}
	return decodePSMBond(result)
}

func decodePSMBond(result []byte) (bool, error) {
	values, err := debtTokenABI.Unpack(debtTokenMethodPSMBonds, result)
	if err != nil || len(values) != 1 {
		return false, ErrInvalidSnapshot
	}
	bonded, ok := values[0].(bool)
	if !ok {
		return false, ErrInvalidSnapshot
	}
	return bonded, nil
}

func staticExtraFromProfile(s *profileSnapshot, cfg *Config, configHash string) (StaticExtra, error) {
	if s == nil || cfg == nil {
		return StaticExtra{}, ErrUnsupportedProfile
	}
	static := StaticExtra{
		ProfileVersion:    productionProfileVersion,
		PSM:               hexutil.Encode(s.PSM[:]),
		PSMCodeHash:       productionPSMCodeHash,
		DebtToken:         hexutil.Encode(s.DebtToken[:]),
		Stable:            hexutil.Encode(s.Stable[:]),
		MetaCore:          hexutil.Encode(s.MetaCore[:]),
		FeeCaller:         hexutil.Encode(s.FeeCaller[:]),
		FeeCallerCodeHash: productionFeeCallerCodeHash,
		FeeHook:           hexutil.Encode(s.FeeHook[:]),
		FeeHookCodeHash:   productionFeeHookCodeHash,
		ListedStableCount: productionListedStables,
		WadOffset:         new(big.Int).SetUint64(s.WadOffset),
		GasDeposit:        cfg.GasDeposit,
		GasRedeem:         cfg.GasRedeem,
		DexID:             cfg.DexID,
		ChainID:           cfg.ChainID,
		ConfigHash:        configHash,
	}
	profileHash, err := staticProfileHash(&static)
	if err != nil {
		return StaticExtra{}, err
	}
	static.ProfileHash = profileHash
	if err := static.validateProductionProfile(); err != nil {
		return StaticExtra{}, err
	}
	return static, nil
}

func (s *StaticExtra) validateProductionProfile() error {
	if s == nil || s.ProfileVersion != productionProfileVersion ||
		s.ListedStableCount != productionListedStables || s.WadOffset == nil || s.WadOffset.Sign() <= 0 ||
		s.DexID == "" || s.ChainID == 0 || s.GasDeposit < 0 || s.GasRedeem < 0 ||
		!validNonzeroAddress(s.PSM) || !validNonzeroAddress(s.DebtToken) ||
		!validNonzeroAddress(s.Stable) || !validNonzeroAddress(s.MetaCore) ||
		!validNonzeroAddress(s.FeeCaller) ||
		!strings.EqualFold(s.PSMCodeHash, productionPSMCodeHash) ||
		!strings.EqualFold(s.FeeCallerCodeHash, productionFeeCallerCodeHash) ||
		!strings.EqualFold(s.FeeHook, productionFeeHookAddress) ||
		!strings.EqualFold(s.FeeHookCodeHash, productionFeeHookCodeHash) {
		return ErrUnsupportedProfile
	}
	wantConfigHash, err := staticConfigHash(s)
	if err != nil || !strings.EqualFold(wantConfigHash, s.ConfigHash) {
		return ErrUnsupportedProfile
	}
	wantProfileHash, err := staticProfileHash(s)
	if err != nil || !strings.EqualFold(wantProfileHash, s.ProfileHash) {
		return ErrUnsupportedProfile
	}
	return nil
}

func (e *Extra) validateProductionSnapshot() error {
	if e == nil || !e.PSMBonded || e.EntryFeeBp == nil || e.ExitFeeBp == nil || e.AvailableMint == nil ||
		e.DebtTokenMinted == nil || e.AvailableReserve == nil {
		return ErrInvalidSnapshot
	}
	for _, fee := range []*big.Int{e.EntryFeeBp, e.ExitFeeBp} {
		if fee.Sign() < 0 || fee.Cmp(bigBp) >= 0 {
			return ErrInvalidSnapshot
		}
	}
	for _, value := range []*big.Int{e.AvailableMint, e.DebtTokenMinted, e.AvailableReserve} {
		if value.Sign() < 0 {
			return ErrInvalidSnapshot
		}
	}
	return nil
}

func validNonzeroAddress(value string) bool {
	return common.IsHexAddress(value) && common.HexToAddress(value) != (common.Address{})
}

func canonicalPoolAddress(psm, stable string) (string, bool) {
	psmAddress, ok0 := normalizedProfileAddress(psm)
	stableAddress, ok1 := normalizedProfileAddress(stable)
	return psmAddress + "-" + stableAddress, ok0 && ok1
}

func validStaticPoolProfile(s *StaticExtra, address, exchange, poolType string,
	blockNumber uint64, tokens []string) bool {
	if s == nil || s.validateProductionProfile() != nil || poolType != DexType ||
		exchange != s.DexID || blockNumber == 0 || len(tokens) != 2 {
		return false
	}
	wantAddress, ok := canonicalPoolAddress(s.PSM, s.Stable)
	if !ok || !strings.EqualFold(wantAddress, address) {
		return false
	}
	debtToken, ok0 := normalizedProfileAddress(tokens[0])
	stable, ok1 := normalizedProfileAddress(tokens[1])
	wantDebt, ok2 := normalizedProfileAddress(s.DebtToken)
	wantStable, ok3 := normalizedProfileAddress(s.Stable)
	return ok0 && ok1 && ok2 && ok3 && strings.EqualFold(debtToken, wantDebt) &&
		strings.EqualFold(stable, wantStable)
}

func validateEntityProfile(p entity.Pool, s *StaticExtra) error {
	tokens := make([]string, len(p.Tokens))
	for i, token := range p.Tokens {
		if token == nil {
			return ErrUnsupportedProfile
		}
		tokens[i] = token.Address
	}
	if !validStaticPoolProfile(s, p.Address, p.Exchange, p.Type, p.BlockNumber, tokens) {
		return ErrUnsupportedProfile
	}
	return nil
}

// validateTrackerProfile retires removed or rotated configuration before the tracker
// creates an RPC request. Static self-consistency is insufficient here: only current
// pool-service configuration has authority to keep a cached venue active.
func validateTrackerProfile(p entity.Pool, s *StaticExtra, cfg *Config) error {
	if validateEntityProfile(p, s) != nil || cfg == nil || cfg.DexID != s.DexID || cfg.ChainID != s.ChainID {
		return ErrProfileChanged
	}
	wantConfigHash, err := psmConfigHash(cfg)
	if err != nil || !strings.EqualFold(wantConfigHash, s.ConfigHash) {
		return ErrProfileChanged
	}
	return nil
}

func (s *PoolSimulator) validPoolProfile() bool {
	if s == nil {
		return false
	}
	return validStaticPoolProfile(&s.StaticExtra, s.Info.Address, s.Info.Exchange,
		s.Info.Type, s.Info.BlockNumber, s.Info.Tokens)
}
