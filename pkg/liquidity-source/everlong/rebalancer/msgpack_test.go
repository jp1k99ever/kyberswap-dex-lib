package everlongrebalancer

import (
	"bytes"
	"testing"

	"github.com/KyberNetwork/msgpack/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

// TestMsgpackRoundTrip guards the pool-service -> router-service hop. The deployed
// curve constants travel inside the simulator as *big.Int (several in fixed-size
// arrays); if any of them decoded as nil factory wiring would panic. A serialized bare
// simulator must remain unroutable: the base interface is deliberately not persisted,
// and only NewPoolSimulatorWithBases may attest and enable it.
func TestMsgpackRoundTrip(t *testing.T) {
	sim, err := NewPoolSimulator(newTestPoolEntity(t))
	require.NoError(t, err)
	tokenIn, tokenOut, amountIn := quoteableDirection(t, sim)

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

	cp, decodedCp := sim.StaticExtra.CurveParams, decoded.StaticExtra.CurveParams
	assert.Equal(t, cp.LeverageRatioWad.String(), decodedCp.LeverageRatioWad.String())
	assert.Equal(t, cp.HWall.String(), decodedCp.HWall.String())
	assert.Equal(t, cp.DWall.String(), decodedCp.DWall.String())
	assert.Equal(t, cp.PhysicalCrFloorWad.String(), decodedCp.PhysicalCrFloorWad.String())
	for i := range cp.BezierPhi {
		require.NotNil(t, decodedCp.BezierPhi[i], "BezierPhi[%d] decoded nil", i)
		assert.Equal(t, cp.BezierPhi[i].String(), decodedCp.BezierPhi[i].String())
	}
	for i := range cp.BezierIntegral {
		require.NotNil(t, decodedCp.BezierIntegral[i], "BezierIntegral[%d] decoded nil", i)
		assert.Equal(t, cp.BezierIntegral[i].String(), decodedCp.BezierIntegral[i].String())
	}
	assert.Equal(t, sim.Extra.Collateral.String(), decoded.Extra.Collateral.String())
	assert.Equal(t, sim.Extra.PriceWad.String(), decoded.Extra.PriceWad.String())
	assert.Equal(t, sim.StaticExtra.Swapper, decoded.StaticExtra.Swapper)
	assert.Equal(t, sim.StaticExtra.ImplementationCodeHash, decoded.StaticExtra.ImplementationCodeHash)
	assert.Equal(t, sim.StaticExtra.SwapperCodeHash, decoded.StaticExtra.SwapperCodeHash)
	assert.Equal(t, sim.StaticExtra.MathCodeHash, decoded.StaticExtra.MathCodeHash)
	assert.Equal(t, sim.StaticExtra.UnderlyingDepositAllowlist,
		decoded.StaticExtra.UnderlyingDepositAllowlist)

	// Both sides remain fail-closed until the exact base is wired by the meta factory.
	params := pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn},
		TokenOut:      tokenOut,
	}
	_, err = sim.CalcAmountOut(params)
	require.ErrorIs(t, err, ErrInexactCoupledState)
	_, err = decoded.CalcAmountOut(params)
	require.ErrorIs(t, err, ErrInexactCoupledState)
}
