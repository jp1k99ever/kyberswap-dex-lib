package everlongcvamm

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/KyberNetwork/msgpack/v5"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMsgpackRoundTrip guards the pool-service -> router-service hop. This simulator
// keeps the accounted tradeable reserves — which drive the solvency clamp — and the
// per-direction gas in UNEXPORTED fields, so they survive only because pkg/msgpack sets
// IncludeUnexported(true). A change there would silently zero the reserves and clamp
// every quote to nothing.
func TestMsgpackRoundTrip(t *testing.T) {
	sIn, _ := fillableCases(t)
	var c fixtureCase
	for _, cand := range sIn {
		if cand.gross.GtUint64(1_000_000) {
			c = cand
			break
		}
	}
	require.NotNil(t, c.gross)
	sim := simFromFixture(t, c, nil)

	// Mirrors pkg/msgpack's encoder/decoder settings; that package cannot be imported
	// here because its generated registry imports this one.
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.IncludeUnexported(true)
	enc.SetForceAsArray(true)
	require.NoError(t, enc.Encode(sim))

	dec := msgpack.NewDecoder(&buf)
	dec.IncludeUnexported(true)
	var decoded PoolSimulator
	require.NoError(t, dec.Decode(&decoded))

	assert.Equal(t, sim.reserveStable.Dec(), decoded.reserveStable.Dec())
	assert.Equal(t, sim.reserveVolatile.Dec(), decoded.reserveVolatile.Dec())
	assert.Equal(t, sim.gasStableIn, decoded.gasStableIn)
	assert.Equal(t, sim.gasVolatileIn, decoded.gasVolatileIn)
	assert.Equal(t, sim.Extra.XWad.Dec(), decoded.Extra.XWad.Dec())
	assert.Equal(t, sim.Extra.Kappa.Dec(), decoded.Extra.Kappa.Dec())
	assert.Equal(t, sim.Extra.Support.AWad.Dec(), decoded.Extra.Support.AWad.Dec())
	assert.Equal(t, sim.Extra.FeeStableInWad.Dec(), decoded.Extra.FeeStableInWad.Dec())

	// And the round-tripped simulator must still quote identically.
	want, err := calc(sim, c)
	require.NoError(t, err)
	got, err := calc(&decoded, c)
	require.NoError(t, err)
	assert.Equal(t, want.TokenAmountOut.Amount.String(), got.TokenAmountOut.Amount.String())
	assert.Equal(t, want.Fee.Amount.String(), got.Fee.Amount.String())
	assert.Equal(t, want.Gas, got.Gas)

	// The clamp must still bind after the hop: shrink the decoded book and re-quote.
	decoded.reserveVolatile = uint256.NewInt(1)
	clamped, err := calc(&decoded, c)
	require.NoError(t, err)
	assert.Equal(t, "1", new(big.Int).Add(clamped.TokenAmountOut.Amount, clamped.Fee.Amount).String(),
		"a one-wei output reserve must clamp the gross payout to one wei")
}
