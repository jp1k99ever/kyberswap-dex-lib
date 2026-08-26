package everlongpsm

import (
	"context"
	"math/big"
	"strconv"
	"strings"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	productionProfileVersion uint8 = 1
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

func staticExtraFromProfile(s *profileSnapshot, gasDeposit, gasRedeem int64) StaticExtra {
	return StaticExtra{
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
		GasDeposit:        gasDeposit,
		GasRedeem:         gasRedeem,
	}
}

func (s *StaticExtra) validateProductionProfile() error {
	if s == nil || s.ProfileVersion != productionProfileVersion ||
		s.ListedStableCount != productionListedStables || s.WadOffset == nil || s.WadOffset.Sign() <= 0 ||
		!validNonzeroAddress(s.PSM) || !validNonzeroAddress(s.DebtToken) ||
		!validNonzeroAddress(s.Stable) || !validNonzeroAddress(s.MetaCore) ||
		!validNonzeroAddress(s.FeeCaller) ||
		!strings.EqualFold(s.PSMCodeHash, productionPSMCodeHash) ||
		!strings.EqualFold(s.FeeCallerCodeHash, productionFeeCallerCodeHash) ||
		!strings.EqualFold(s.FeeHook, productionFeeHookAddress) ||
		!strings.EqualFold(s.FeeHookCodeHash, productionFeeHookCodeHash) {
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

// profileFingerprint is the listing cursor. Mutable scalar rates are deliberately not
// included: the tracker samples them every block. Identity, caller, decimal scale and
// gas configuration require a replacement pool so persisted StaticExtra cannot lag.
func profileFingerprint(s StaticExtra, dexID string, chainID uint64) string {
	parts := []string{
		strings.ToLower(dexID), strconv.FormatUint(chainID, 10),
		strconv.FormatUint(uint64(s.ProfileVersion), 10), strings.ToLower(s.PSM),
		strings.ToLower(s.PSMCodeHash),
		strings.ToLower(s.DebtToken), strings.ToLower(s.Stable), strings.ToLower(s.MetaCore),
		strings.ToLower(s.FeeCaller), strings.ToLower(s.FeeCallerCodeHash),
		strings.ToLower(s.FeeHook), strings.ToLower(s.FeeHookCodeHash),
		strconv.Itoa(s.ListedStableCount), s.WadOffset.String(),
		strconv.FormatInt(s.GasDeposit, 10), strconv.FormatInt(s.GasRedeem, 10),
	}
	return crypto.Keccak256Hash([]byte(strings.Join(parts, "|"))).Hex()
}
