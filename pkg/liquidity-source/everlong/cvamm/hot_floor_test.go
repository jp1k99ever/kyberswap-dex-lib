package everlongcvamm

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHotFloorAtMatchesHook pins hotFloorAt to ClammFeeHook.hotFeeFloorWad
// (0xC2d0…a10b on Berachain) at a mid-ramp push rate observed live: the chain answered
// 4525020581477280 / 7541700969128801 for rate 83735027590824611874 with the hook's
// public constants (ref 40e18, sat 160e18, levels 1.5% / 2.5%).
func TestHotFloorAtMatchesHook(t *testing.T) {
	w := func(s string) *big.Int { v, _ := new(big.Int).SetString(s, 10); return v }
	ref, sat := w("40000000000000000000"), w("160000000000000000000")
	levelS, levelV := w("15000000000000000"), w("25000000000000000")
	rate := w("83735027590824611874")

	require.Equal(t, "4525020581477280", hotFloorAt(true, rate, ref, sat, levelS).String())
	require.Equal(t, "7541700969128801", hotFloorAt(true, rate, ref, sat, levelV).String())

	// edges: disabled, at/below reference, at/above saturation
	require.Zero(t, hotFloorAt(false, rate, ref, sat, levelS).Sign())
	require.Zero(t, hotFloorAt(true, ref, ref, sat, levelS).Sign())
	require.Equal(t, levelS, hotFloorAt(true, sat, ref, sat, levelS))
	require.Equal(t, levelS, hotFloorAt(true, new(big.Int).Lsh(big.NewInt(1), 127), ref, sat, levelS))
}
