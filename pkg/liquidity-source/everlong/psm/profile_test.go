package everlongpsm

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient/gethclient"
	"github.com/goccy/go-json"
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

func TestConfigHashBindsEveryListingInput(t *testing.T) {
	base := unitConfig()
	base.GasDeposit, base.GasRedeem = 260_000, 220_000
	want, err := psmConfigHash(base)
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"dex", func(c *Config) { c.DexID = "detached" }},
		{"chain", func(c *Config) { c.ChainID = 1 }},
		{"psm", func(c *Config) { c.PSM = unitDebt }},
		{"stable", func(c *Config) { c.Stables[0] = unitDebt }},
		{"caller", func(c *Config) { c.FeeCaller = unitMetaCore }},
		{"deposit gas", func(c *Config) { c.GasDeposit++ }},
		{"redeem gas", func(c *Config) { c.GasRedeem++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := *base
			changed.Stables = append([]string(nil), base.Stables...)
			tc.mutate(&changed)
			got, err := psmConfigHash(&changed)
			require.NoError(t, err)
			require.NotEqual(t, want, got)
		})
	}
}

func TestConfigHashCanonicalizesAddresses(t *testing.T) {
	a := &Config{DexID: DexType, ChainID: 80094,
		PSM:       "0x0999417c0f9ded4356B099bcC83A16437B841323",
		Stables:   []string{"0xFCBD14DC51f0A4d49d5E53C2E0950e0bC26d0Dce"},
		FeeCaller: "0x4eBD7A6543Ace6076F089082931c380a3675bC5c"}
	b := *a
	b.PSM = strings.ToLower(a.PSM)
	b.Stables = []string{strings.ToLower(a.Stables[0])}
	b.FeeCaller = strings.ToLower(a.FeeCaller)
	ha, err := psmConfigHash(a)
	require.NoError(t, err)
	hb, err := psmConfigHash(&b)
	require.NoError(t, err)
	require.Equal(t, ha, hb)
}

func TestConfigHashRejectsInvalidExecutionConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"empty dex", func(c *Config) { c.DexID = "" }},
		{"zero chain", func(c *Config) { c.ChainID = 0 }},
		{"negative deposit gas", func(c *Config) { c.GasDeposit = -1 }},
		{"negative redeem gas", func(c *Config) { c.GasRedeem = -1 }},
		{"removed stable", func(c *Config) { c.Stables = nil }},
		{"malformed psm", func(c *Config) { c.PSM = "not-an-address" }},
		{"missing caller", func(c *Config) { c.FeeCaller = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := unitConfig()
			tc.mutate(cfg)
			_, err := psmConfigHash(cfg)
			require.Error(t, err)

			// The lister must fail on configuration alone rather than dereferencing
			// the deliberately absent RPC client.
			_, _, err = NewPoolsListUpdater(cfg, nil).GetNewPools(context.Background(), nil)
			require.Error(t, err)
		})
	}
}

func TestStaticProfileHashBindsDiscoveredIdentity(t *testing.T) {
	cfg := unitConfig()
	configHash, err := psmConfigHash(cfg)
	require.NoError(t, err)
	base, err := staticExtraFromProfile(validProfileSnapshot(), cfg, configHash)
	require.NoError(t, err)
	want := base.ProfileHash

	for _, tc := range []struct {
		name   string
		mutate func(*StaticExtra)
	}{
		{"debt token", func(s *StaticExtra) { s.DebtToken = unitMetaCore }},
		{"stable", func(s *StaticExtra) { s.Stable = unitMetaCore }},
		{"meta core", func(s *StaticExtra) { s.MetaCore = unitDebt }},
		{"caller", func(s *StaticExtra) { s.FeeCaller = unitMetaCore }},
		{"wad offset", func(s *StaticExtra) { s.WadOffset = big.NewInt(2) }},
		{"config hash", func(s *StaticExtra) { s.ConfigHash = common.Hash{}.Hex() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.mutate(&changed)
			got, err := staticProfileHash(&changed)
			require.NoError(t, err)
			require.NotEqual(t, want, got)
		})
	}
}

func TestTrackerRejectsDetachedProfileBeforeRPC(t *testing.T) {
	sim := simWith(t, "100", "1", "1", "5", "5", false)
	p := entity.Pool{
		Address: sim.Info.Address, Exchange: sim.Info.Exchange, Type: sim.Info.Type,
		Tokens:   []*entity.PoolToken{{Address: sim.Info.Tokens[0]}, {Address: sim.Info.Tokens[1]}},
		Reserves: entity.PoolReserves{"100", "1"}, BlockNumber: sim.Info.BlockNumber,
	}
	staticBytes, err := json.Marshal(sim.StaticExtra)
	require.NoError(t, err)
	p.StaticExtra = string(staticBytes)
	client := ethrpc.New("http://127.0.0.1:1")
	require.NoError(t, validateTrackerProfile(p, &sim.StaticExtra, unitConfig()))

	for _, tc := range []struct {
		name   string
		cfg    *Config
		mutate func(*entity.Pool)
	}{
		{"nil config", nil, nil},
		{"removed stable", &Config{DexID: DexType, ChainID: unitConfig().ChainID,
			PSM: unitPSM, FeeCaller: unitCaller}, nil},
		{"rotated psm", &Config{DexID: DexType, ChainID: unitConfig().ChainID,
			PSM: unitDebt, Stables: []string{unitStable}, FeeCaller: unitCaller}, nil},
		{"rotated stable", &Config{DexID: DexType, ChainID: unitConfig().ChainID,
			PSM: unitPSM, Stables: []string{unitDebt}, FeeCaller: unitCaller}, nil},
		{"rotated caller", &Config{DexID: DexType, ChainID: unitConfig().ChainID,
			PSM: unitPSM, Stables: []string{unitStable}, FeeCaller: unitMetaCore}, nil},
		{"changed dex", &Config{DexID: "detached", ChainID: unitConfig().ChainID,
			PSM: unitPSM, Stables: []string{unitStable}, FeeCaller: unitCaller}, nil},
		{"changed chain", &Config{DexID: DexType, ChainID: 1,
			PSM: unitPSM, Stables: []string{unitStable}, FeeCaller: unitCaller}, nil},
		{"changed gas", &Config{DexID: DexType, ChainID: unitConfig().ChainID,
			PSM: unitPSM, Stables: []string{unitStable}, FeeCaller: unitCaller, GasDeposit: 1}, nil},
		{"zero block", unitConfig(), func(p *entity.Pool) { p.BlockNumber = 0 }},
		{"wrong address", unitConfig(), func(p *entity.Pool) { p.Address = unitPSM }},
		{"wrong exchange", unitConfig(), func(p *entity.Pool) { p.Exchange = "detached" }},
		{"wrong type", unitConfig(), func(p *entity.Pool) { p.Type = "detached" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := p
			if tc.mutate != nil {
				tc.mutate(&changed)
			}
			_, err := NewPoolTracker(tc.cfg, client).GetNewPoolState(
				context.Background(), changed, pool.GetNewPoolStateParams{})
			require.ErrorIs(t, err, ErrProfileChanged,
				"profile drift must fail before the deliberately unreachable RPC endpoint")
		})
	}
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
