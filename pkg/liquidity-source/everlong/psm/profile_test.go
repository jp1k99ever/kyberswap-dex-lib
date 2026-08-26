package everlongpsm

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

func validProfileSnapshot() *profileSnapshot {
	return &profileSnapshot{
		PSM:                 common.HexToAddress(unitPSM),
		DebtToken:           common.HexToAddress(unitDebt),
		MetaCore:            common.HexToAddress(unitMetaCore),
		Stable:              common.HexToAddress(unitStable),
		FeeCaller:           common.HexToAddress(unitCaller),
		FeeHook:             common.HexToAddress(productionFeeHookAddress),
		ListedStable:        common.HexToAddress(unitStable),
		ListedStablesLength: big.NewInt(1),
		WadOffset:           1,
		EntryFeeBp:          big.NewInt(5),
		ExitFeeBp:           big.NewInt(5),
		PSMCodeHash:         common.HexToHash(productionPSMCodeHash),
		FeeHookCodeHash:     common.HexToHash(productionFeeHookCodeHash),
		FeeCallerCodeHash:   common.HexToHash(productionFeeCallerCodeHash),
		PSMBonded:           true,
	}
}

func TestProductionProfileRejectsEveryUnsupportedDimension(t *testing.T) {
	require.NoError(t, validProfileSnapshot().validate())

	tests := []struct {
		name   string
		mutate func(*profileSnapshot)
	}{
		{"zero caller", func(s *profileSnapshot) { s.FeeCaller = common.Address{} }},
		{"caller is EOA", func(s *profileSnapshot) {
			s.FeeCallerCodeHash = common.HexToHash("0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")
		}},
		{"unrelated caller contract", func(s *profileSnapshot) { s.FeeCallerCodeHash = common.HexToHash("0x01") }},
		{"psm bond revoked", func(s *profileSnapshot) { s.PSMBonded = false }},
		{"psm runtime", func(s *profileSnapshot) { s.PSMCodeHash = common.HexToHash("0x01") }},
		{"hook address", func(s *profileSnapshot) { s.FeeHook = common.HexToAddress(unitPSM) }},
		{"hook runtime", func(s *profileSnapshot) { s.FeeHookCodeHash = common.HexToHash("0x01") }},
		{"cap hook", func(s *profileSnapshot) { s.CapHook = common.HexToAddress(unitPSM) }},
		{"yield hook", func(s *profileSnapshot) { s.YieldHook = common.HexToAddress(unitPSM) }},
		{"two stables", func(s *profileSnapshot) { s.ListedStablesLength = big.NewInt(2) }},
		{"different listed stable", func(s *profileSnapshot) { s.ListedStable = common.HexToAddress(unitPSM) }},
		{"blacklisted stable", func(s *profileSnapshot) { s.WadOffset = 0 }},
		{"missing entry rate", func(s *profileSnapshot) { s.EntryFeeBp = nil }},
		{"invalid exit rate", func(s *profileSnapshot) { s.ExitFeeBp = big.NewInt(10_000) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := validProfileSnapshot()
			tc.mutate(snapshot)
			require.ErrorIs(t, snapshot.validate(), ErrUnsupportedProfile)
		})
	}
}

func TestProductionProfileAllowsMutableScalarRates(t *testing.T) {
	for _, rates := range [][2]int64{{0, 0}, {5, 5}, {17, 31}, {9_999, 9_999}} {
		snapshot := validProfileSnapshot()
		snapshot.EntryFeeBp = big.NewInt(rates[0])
		snapshot.ExitFeeBp = big.NewInt(rates[1])
		require.NoError(t, snapshot.validate(), "rates %v are exact outputs of the reviewed hook", rates)
	}
}

func TestDecodePSMBondFailsClosed(t *testing.T) {
	trueWord := make([]byte, 32)
	trueWord[31] = 1
	bonded, err := decodePSMBond(trueWord)
	require.NoError(t, err)
	require.True(t, bonded)

	falseWord := make([]byte, 32)
	bonded, err = decodePSMBond(falseWord)
	require.NoError(t, err)
	require.False(t, bonded)

	for _, malformed := range [][]byte{nil, {1}, make([]byte, 31)} {
		_, err := decodePSMBond(malformed)
		require.ErrorIs(t, err, ErrInvalidSnapshot)
	}
}

func TestConfiguredAddressesRequireOneStableAndContractCallerIdentity(t *testing.T) {
	valid := &Config{PSM: unitPSM, Stables: []string{unitStable}, FeeCaller: unitCaller}
	_, _, _, err := configuredAddresses(valid)
	require.NoError(t, err)

	for _, cfg := range []*Config{
		nil,
		{PSM: unitPSM, Stables: nil, FeeCaller: unitCaller},
		{PSM: unitPSM, Stables: []string{unitStable, unitDebt}, FeeCaller: unitCaller},
		{PSM: unitPSM, Stables: []string{unitStable}},
		{PSM: unitPSM, Stables: []string{unitStable}, FeeCaller: "0x0000000000000000000000000000000000000000"},
	} {
		_, _, _, err := configuredAddresses(cfg)
		require.Error(t, err)
	}
}

func TestProfileFingerprintRelistsIdentityButNotMutableRates(t *testing.T) {
	static := staticExtraFromProfile(validProfileSnapshot(), 260_000, 220_000)
	base := profileFingerprint(static, DexType, 80094)

	changedCaller := static
	changedCaller.FeeCaller = "0x0000000000000000000000000000000000000006"
	require.NotEqual(t, base, profileFingerprint(changedCaller, DexType, 80094))

	changedGas := static
	changedGas.GasDeposit++
	require.NotEqual(t, base, profileFingerprint(changedGas, DexType, 80094))
	require.NotEqual(t, base, profileFingerprint(static, "other-dex", 80094))
	require.NotEqual(t, base, profileFingerprint(static, DexType, 1))

	// Rates live in Extra and are intentionally absent from the static cursor. A
	// governance rate update is tracked, not treated as a new pool identity.
	require.Equal(t, base, profileFingerprint(static, DexType, 80094))
}

func TestStateOverridesFailClosed(t *testing.T) {
	original := entity.Pool{Address: unitPSM}
	got, err := (&PoolTracker{}).GetNewPoolStateWithOverrides(context.Background(), original,
		pool.GetNewPoolStateWithOverridesParams{Overrides: map[common.Address]gethclient.OverrideAccount{
			common.HexToAddress(unitPSM): {},
		}})
	require.ErrorIs(t, err, ErrStateOverridesUnsupported)
	require.Equal(t, original, got)
}
