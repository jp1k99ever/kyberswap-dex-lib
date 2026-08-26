package everlongcvamm

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSupportedHookHotFloorMatchesReviewedBytecode(t *testing.T) {
	w := func(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }
	rate := w("83735027590824611874")

	require.Equal(t, "4525020581477280", supportedHookHotFloor(true, rate).String())
	require.Equal(t, "7541700969128801", supportedHookHotFloor(false, rate).String())
	require.Zero(t, supportedHookHotFloor(true, ffadReferenceRateWad).Sign())
	require.Equal(t, ffadStableInFloorWad, supportedHookHotFloor(true, ffadSaturationRateWad))
	require.Equal(t, ffadVolatileInFloorWad,
		supportedHookHotFloor(false, new(big.Int).Lsh(big.NewInt(1), 127)))
}
