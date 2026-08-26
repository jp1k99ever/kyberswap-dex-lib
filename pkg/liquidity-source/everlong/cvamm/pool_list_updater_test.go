package everlongcvamm

import (
	"context"
	"testing"

	"github.com/KyberNetwork/ethrpc"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

func TestRemovedALMCursorIsPrunedForReadd(t *testing.T) {
	address, ok := normalizeAddress(testALM, false)
	require.True(t, ok)
	old := Metadata{
		Listed:          map[string]bool{address: true},
		Implementations: map[string]string{address: "0x0000000000000000000000000000000000000003"},
		Profiles:        map[string]string{address: "0xprofile"},
	}
	oldBytes, err := json.Marshal(old)
	require.NoError(t, err)

	// No RPC endpoint is available on purpose. An empty current ALM set must persist
	// the pruned cursor before RPC so a removal is reversible rather than sticky.
	updater := NewPoolsListUpdater(&Config{
		DexID: DexType, ChainID: valueobject.ChainIDBerachain,
	}, ethrpc.New("http://127.0.0.1:1"))
	pools, prunedBytes, err := updater.GetNewPools(context.Background(), oldBytes)
	require.NoError(t, err)
	require.Empty(t, pools)
	require.NotEqual(t, string(oldBytes), string(prunedBytes))

	var pruned Metadata
	require.NoError(t, json.Unmarshal(prunedBytes, &pruned))
	require.Empty(t, pruned.Listed)
	require.Empty(t, pruned.Implementations)
	require.Empty(t, pruned.Profiles)
	require.False(t, pruned.matches(address,
		"0x0000000000000000000000000000000000000003", "0xprofile"),
		"re-adding the same ALM must enter the ordinary listing path")
}
