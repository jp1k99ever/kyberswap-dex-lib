package everlongcvamm

import (
	"testing"

	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

func TestConfigHashBindsListingInputs(t *testing.T) {
	base := ALMConfig{
		Address:       "0xf5124f5605ce1e91a7429b837b7dac8f9e5378dd",
		Adapter:       "0x0000000000000000000000000000000000000001",
		GasStableIn:   400_000,
		GasVolatileIn: 300_000,
	}
	want, err := cvammConfigHash(DexType, valueobject.ChainIDBerachain, base)
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		dexID   string
		chainID valueobject.ChainID
		mutate  func(*ALMConfig)
	}{
		{"dex", "detached", valueobject.ChainIDBerachain, func(*ALMConfig) {}},
		{"chain", DexType, valueobject.ChainIDEthereum, func(*ALMConfig) {}},
		{"alm", DexType, valueobject.ChainIDBerachain, func(c *ALMConfig) {
			c.Address = "0x0000000000000000000000000000000000000002"
		}},
		{"adapter", DexType, valueobject.ChainIDBerachain, func(c *ALMConfig) {
			c.Adapter = "0x0000000000000000000000000000000000000002"
		}},
		{"stable gas", DexType, valueobject.ChainIDBerachain, func(c *ALMConfig) { c.GasStableIn++ }},
		{"volatile gas", DexType, valueobject.ChainIDBerachain, func(c *ALMConfig) { c.GasVolatileIn++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := base
			tc.mutate(&changed)
			got, err := cvammConfigHash(tc.dexID, tc.chainID, changed)
			require.NoError(t, err)
			require.NotEqual(t, want, got)
		})
	}
}

func TestConfigHashCanonicalizesAddresses(t *testing.T) {
	a := ALMConfig{Address: "0xF5124F5605ce1e91A7429B837b7daC8f9E5378dd"}
	b := ALMConfig{Address: "0xf5124f5605ce1e91a7429b837b7dac8f9e5378dd"}
	ha, err := cvammConfigHash(DexType, valueobject.ChainIDBerachain, a)
	require.NoError(t, err)
	hb, err := cvammConfigHash(DexType, valueobject.ChainIDBerachain, b)
	require.NoError(t, err)
	require.Equal(t, ha, hb)
}

func TestConfigHashRejectsInvalidExecutionConfig(t *testing.T) {
	for _, tc := range []struct {
		dex string
		alm ALMConfig
	}{
		{"", ALMConfig{Address: testALM}},
		{DexType, ALMConfig{Address: testALM, GasStableIn: -1}},
		{DexType, ALMConfig{Address: testALM, GasVolatileIn: -1}},
		{DexType, ALMConfig{Address: "not-an-address"}},
		{DexType, ALMConfig{Address: testALM, Adapter: "not-an-address"}},
	} {
		_, err := cvammConfigHash(tc.dex, valueobject.ChainIDBerachain, tc.alm)
		require.ErrorIs(t, err, ErrInvalidProfile)
	}
}

func TestStaticProfileHashBindsDiscoveredIdentity(t *testing.T) {
	var base StaticExtra
	require.NoError(t, json.Unmarshal([]byte(testStaticExtra(t)), &base))
	want := base.ProfileHash

	for _, tc := range []struct {
		name   string
		mutate func(*StaticExtra)
	}{
		{"token0", func(se *StaticExtra) {
			se.Token0 = "0x0000000000000000000000000000000000000003"
		}},
		{"token1", func(se *StaticExtra) {
			se.Token1 = "0x0000000000000000000000000000000000000003"
		}},
		{"implementation", func(se *StaticExtra) {
			se.Implementation = "0x0000000000000000000000000000000000000003"
		}},
		{"implementation hash", func(se *StaticExtra) {
			se.ImplementationCodeHash = "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
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
