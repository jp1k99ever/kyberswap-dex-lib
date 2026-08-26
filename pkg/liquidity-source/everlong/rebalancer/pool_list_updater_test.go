package everlongrebalancer

import (
	"encoding/binary"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

func TestMetadataMatchesImplementation(t *testing.T) {
	swapper := common.HexToAddress("0x0000000000000000000000000000000000000001")
	vault := common.HexToAddress("0x0000000000000000000000000000000000000002")
	implementation := common.HexToAddress("0x0000000000000000000000000000000000000003")
	depositAllowlist := common.HexToAddress("0x0000000000000000000000000000000000000005")
	stable := common.HexToAddress("0x0000000000000000000000000000000000000006").Hex()
	volatile := common.HexToAddress("0x0000000000000000000000000000000000000007").Hex()
	configHash := "0xconfig"

	metadata := Metadata{
		Swapper:                    swapper.Hex(),
		ManagedVault:               vault.Hex(),
		Implementation:             implementation.Hex(),
		ALMAdapterCodeHash:         supportedAlmAdapterCodeHash,
		ImplementationCodeHash:     supportedRebalancerImplementationCodeHash,
		SwapperCodeHash:            supportedSettlementSwapperCodeHash,
		MathCodeHash:               supportedCollRebalancerMathCodeHash,
		UnderlyingDepositAllowlist: depositAllowlist.Hex(),
		ConfigHash:                 configHash,
		StableToken:                stable,
		VolatileToken:              volatile,
	}
	require.True(t, metadata.matches(swapper, vault, implementation, depositAllowlist,
		stable, volatile, configHash))
	require.False(t, metadata.matches(swapper, vault,
		common.HexToAddress("0x0000000000000000000000000000000000000004"), depositAllowlist,
		stable, volatile, configHash),
		"an implementation-only upgrade must relist the pool")
	require.False(t, metadata.matches(swapper, vault, implementation,
		common.HexToAddress("0x0000000000000000000000000000000000000008"),
		stable, volatile, configHash),
		"a raw ALM allowlist rotation must relist the pool")
	require.False(t, metadata.matches(swapper, vault, implementation, depositAllowlist,
		stable, volatile, "0xchanged"),
		"a config-only change must relist the pool")
	require.False(t, metadata.matches(swapper, vault, implementation, depositAllowlist,
		stable, common.HexToAddress("0x0000000000000000000000000000000000000009").Hex(), configHash),
		"an on-chain pair change must relist the pool")

	metadata.Implementation = "" // cursor persisted by the previous integration version
	require.False(t, metadata.matches(swapper, vault, implementation, depositAllowlist,
		stable, volatile, configHash),
		"an old cursor must relist once to pin the implementation")
	metadata.Implementation = implementation.Hex()
	metadata.VolatileToken = "" // cursor persisted before token1 was bound in StaticExtra
	require.False(t, metadata.matches(swapper, vault, implementation, depositAllowlist,
		stable, volatile, configHash), "an old cursor must relist once to pin token1")
}

func TestRuntimeCodeHashMatchesExactRuntime(t *testing.T) {
	code := []byte{0x60, 0x00, 0x60, 0x01}
	want := crypto.Keccak256Hash(code).Hex()
	require.True(t, runtimeCodeHashMatches(code, want))
	require.True(t, runtimeCodeHashMatches(code, strings.ToUpper(want)))
	require.False(t, runtimeCodeHashMatches(append(code, 0x00), want))
	require.False(t, runtimeCodeHashMatches(nil, crypto.Keccak256Hash(nil).Hex()),
		"an empty account must never satisfy a runtime attestation")
}

func TestConfigFingerprintCoversEveryStaticInput(t *testing.T) {
	base := *berachainTestConfig()
	cp := berachainCurveParams()
	hash := func(cfg Config, curve CurveParams) (string, error) {
		return (&PoolsListUpdater{config: &cfg}).configFingerprint(
			curve, base.Stable, base.Volatile)
	}
	want, err := hash(base, cp)
	require.NoError(t, err)
	mutations := []func(*Config, *CurveParams){
		func(c *Config, _ *CurveParams) { c.DexID += "-changed" },
		func(c *Config, _ *CurveParams) { c.ChainID++ },
		func(c *Config, _ *CurveParams) { c.Rebalancer = common.Address{0x01}.Hex() },
		func(c *Config, _ *CurveParams) { c.Stable = common.Address{0x02}.Hex() },
		func(c *Config, _ *CurveParams) { c.Volatile = common.Address{0x03}.Hex() },
		func(c *Config, _ *CurveParams) { c.Math = common.Address{0x04}.Hex() },
		func(c *Config, _ *CurveParams) { c.GasLeverage++ },
		func(c *Config, _ *CurveParams) { c.GasDeleverage++ },
		func(_ *Config, curve *CurveParams) { curve.HJoin = new(big.Int).Add(curve.HJoin, big.NewInt(1)) },
	}
	for i, mutate := range mutations {
		cfg, curve := base, cp
		mutate(&cfg, &curve)
		got, err := hash(cfg, curve)
		require.True(t, err != nil || got != want, "mutation %d was missing from the cursor", i)
	}

	// Stable/Volatile are optional assertions. Omitting an assertion that agreed with
	// the on-chain pair does not change the semantic profile; the derived pair remains
	// in the digest and therefore still protects a persisted pool.
	withoutAssertions := base
	withoutAssertions.Stable, withoutAssertions.Volatile = "", ""
	got, err := hash(withoutAssertions, cp)
	require.NoError(t, err)
	require.Equal(t, want, got)

	negativeGas := base
	negativeGas.GasLeverage = -1
	_, err = hash(negativeGas, cp)
	require.ErrorIs(t, err, ErrInvalidPoolProfile)
}

func TestRuntimeLinksLibrary(t *testing.T) {
	math := common.HexToAddress("0x4eBD7A6543Ace6076F089082931c380a3675bC5c")
	other := common.HexToAddress("0x0000000000000000000000000000000000000001")

	linked := append([]byte{opPush20}, math.Bytes()...)
	linked = append(linked, 0x5a, opDelegateCall) // GAS; DELEGATECALL
	require.True(t, runtimeLinksLibrary(linked, math))
	require.False(t, runtimeLinksLibrary(linked, other))

	// A raw substring is not a link: the address is data belonging to PUSH32.
	push32Data := make([]byte, 32)
	copy(push32Data[7:], math.Bytes())
	nested := append([]byte{opPush32}, push32Data...)
	nested = append(nested, opDelegateCall)
	require.False(t, runtimeLinksLibrary(nested, math))

	// Nor is an ordinary address constant in code that never delegates.
	noCall := append([]byte{opPush20}, math.Bytes()...)
	noCall = append(noCall, 0x00)
	require.False(t, runtimeLinksLibrary(noCall, math))

	// A different address loaded before the delegatecall is the actual candidate target;
	// an older matching constant must not carry across it.
	shadowed := append([]byte{opPush20}, math.Bytes()...)
	shadowed = append(shadowed, opPush20)
	shadowed = append(shadowed, other.Bytes()...)
	shadowed = append(shadowed, 0x5a, opDelegateCall)
	require.False(t, runtimeLinksLibrary(shadowed, math))

	// A matching constant is not the target merely because a later delegatecall happens
	// before another PUSH20. The target below is supplied by a different stack value.
	unrelated := append([]byte{opPush20}, math.Bytes()...)
	unrelated = append(unrelated, opPush1, 0x00, 0x50, 0x5a, opDelegateCall)
	require.False(t, runtimeLinksLibrary(unrelated, math))

	// Solidity's opaque metadata is outside the executable program and must not attest it.
	metadata := append([]byte{0xa1, opPush20}, math.Bytes()...)
	metadata = append(metadata, opDelegateCall)
	withMetadata := append([]byte{0x00}, metadata...)
	var trailer [2]byte
	binary.BigEndian.PutUint16(trailer[:], uint16(len(metadata)))
	withMetadata = append(withMetadata, trailer[:]...)
	require.False(t, runtimeLinksLibrary(withMetadata, math))
}
