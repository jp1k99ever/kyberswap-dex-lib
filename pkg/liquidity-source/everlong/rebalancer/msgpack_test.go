package everlongrebalancer

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/KyberNetwork/msgpack/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

func msgpackRoundTrip(t *testing.T, sim *PoolSimulator) *PoolSimulator {
	t.Helper()
	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.IncludeUnexported(true)
	enc.SetForceAsArray(true)
	require.NoError(t, enc.Encode(sim))

	dec := msgpack.NewDecoder(&buf)
	dec.IncludeUnexported(true)
	var decoded PoolSimulator
	require.NoError(t, dec.Decode(&decoded))
	return &decoded
}

// TestMsgpackRoundTrip guards the pool-service -> router-service hop. The deployed
// curve constants travel inside the simulator as *big.Int (several in fixed-size
// arrays); if any of them decoded as nil factory wiring would panic. A serialized bare
// simulator must remain unroutable: the base interface is deliberately not persisted,
// and only NewPoolSimulatorWithBases may attest and enable it.
func TestMsgpackRoundTrip(t *testing.T) {
	sim, err := NewPoolSimulator(newTestPoolEntity(t))
	require.NoError(t, err)
	tokenIn, tokenOut, amountIn := quoteableDirection(t, sim)
	// Model a cached simulator that was routable before serialization. Interface fields
	// are not serialized, but the unexported scalar latch is: decoding must not let that
	// stale true value attest a missing base pool.
	sim.couplingExact = true

	// Mirrors pkg/msgpack's encoder/decoder settings; that package cannot be imported
	// here because its generated registry imports this one.
	decoded := msgpackRoundTrip(t, sim)
	require.True(t, decoded.couplingExact, "the regression must exercise a persisted true latch")
	require.Nil(t, decoded.basePool, "interface-backed base state is intentionally not serialized")

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
	assert.Equal(t, sim.StaticExtra.VolatileToken, decoded.StaticExtra.VolatileToken)
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

// TestCurrentFormatMsgpackProfileCorruptionFailsClosed proves that a cache object in
// the current wire shape cannot bypass the entity-pool constructor. Each mutation is
// encoded and decoded normally, with the old routable latch deliberately preserved;
// CalcAmountOut must reject the immutable profile before noticing the absent base.
func TestCurrentFormatMsgpackProfileCorruptionFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   error
		mutate func(*PoolSimulator)
	}{
		{
			name: "pool address",
			want: ErrInvalidPoolProfile,
			mutate: func(sim *PoolSimulator) {
				sim.Info.Address = "0x00000000000000000000000000000000000000ff"
			},
		},
		{
			name: "unpinned block",
			want: ErrInvalidPoolProfile,
			mutate: func(sim *PoolSimulator) {
				sim.Info.BlockNumber = 0
			},
		},
		{
			name: "token order",
			want: ErrInvalidPoolProfile,
			mutate: func(sim *PoolSimulator) {
				sim.Info.Tokens[0], sim.Info.Tokens[1] = sim.Info.Tokens[1], sim.Info.Tokens[0]
			},
		},
		{
			name: "token count",
			want: ErrInvalidPoolProfile,
			mutate: func(sim *PoolSimulator) {
				sim.Info.Tokens = sim.Info.Tokens[:1]
			},
		},
		{
			name: "required static address",
			want: ErrInvalidPoolProfile,
			mutate: func(sim *PoolSimulator) {
				sim.StaticExtra.Core = ""
			},
		},
		{
			name: "volatile identity",
			want: ErrInvalidPoolProfile,
			mutate: func(sim *PoolSimulator) {
				sim.StaticExtra.VolatileToken = "0x00000000000000000000000000000000000000ff"
			},
		},
		{
			name: "runtime hash",
			want: ErrUnsupportedSwapper,
			mutate: func(sim *PoolSimulator) {
				sim.StaticExtra.SwapperCodeHash = "0xdeadbeef"
			},
		},
		{
			name: "curve value",
			want: ErrInvalidCurveParams,
			mutate: func(sim *PoolSimulator) {
				sim.StaticExtra.CurveParams.HJoin = new(big.Int).Add(
					sim.StaticExtra.CurveParams.HJoin, big.NewInt(1))
			},
		},
		{
			name: "live curve value",
			want: ErrInvalidCurveParams,
			mutate: func(sim *PoolSimulator) {
				curve := berachainCurveParams()
				curve.PhysicalCrFloorWad.Add(curve.PhysicalCrFloorWad, big.NewInt(1))
				sim.Extra.LiveCurve = &curve
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sim, err := NewPoolSimulator(newTestPoolEntity(t))
			require.NoError(t, err)
			sim.couplingExact = true
			tc.mutate(sim)
			decoded := msgpackRoundTrip(t, sim)
			_, err = decoded.CalcAmountOut(pool.CalcAmountOutParams{
				TokenAmountIn: pool.TokenAmount{Token: testNECT, Amount: big.NewInt(1)},
				TokenOut:      testWBTC,
			})
			require.ErrorIs(t, err, tc.want)
		})
	}
}
