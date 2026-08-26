package everlongrebalancer

import (
	"encoding/binary"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

func TestMetadataMatchesImplementation(t *testing.T) {
	swapper := common.HexToAddress("0x0000000000000000000000000000000000000001")
	vault := common.HexToAddress("0x0000000000000000000000000000000000000002")
	implementation := common.HexToAddress("0x0000000000000000000000000000000000000003")

	metadata := Metadata{
		Swapper:        swapper.Hex(),
		ManagedVault:   vault.Hex(),
		Implementation: implementation.Hex(),
	}
	require.True(t, metadata.matches(swapper, vault, implementation))
	require.False(t, metadata.matches(swapper, vault,
		common.HexToAddress("0x0000000000000000000000000000000000000004")),
		"an implementation-only upgrade must relist the pool")

	metadata.Implementation = "" // cursor persisted by the previous integration version
	require.False(t, metadata.matches(swapper, vault, implementation),
		"an old cursor must relist once to pin the implementation")
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

	// Solidity's opaque metadata is outside the executable program and must not attest it.
	metadata := append([]byte{0xa1, opPush20}, math.Bytes()...)
	metadata = append(metadata, opDelegateCall)
	withMetadata := append([]byte{0x00}, metadata...)
	var trailer [2]byte
	binary.BigEndian.PutUint16(trailer[:], uint16(len(metadata)))
	withMetadata = append(withMetadata, trailer[:]...)
	require.False(t, runtimeLinksLibrary(withMetadata, math))
}
