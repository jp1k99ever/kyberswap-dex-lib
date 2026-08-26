package everlongpsm

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/KyberNetwork/msgpack/v5"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

// TestMsgpackRoundTrip guards the pool-service -> router-service path. Decoding a
// simulator bypasses NewPoolSimulator, so CalcAmountOut itself must both preserve and
// enforce the production attestation and exact mutable snapshot.
func TestMsgpackRoundTrip(t *testing.T) {
	sim := simWith(t, "1000000000000000000000", "100000000000000000000",
		"100000000", "5", "7", false)
	params := pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: unitStable, Amount: big.NewInt(100_000_000)},
		TokenOut:      unitDebt,
	}
	want, err := sim.CalcAmountOut(params)
	require.NoError(t, err)

	var buf bytes.Buffer
	enc := msgpack.NewEncoder(&buf)
	enc.IncludeUnexported(true)
	enc.SetForceAsArray(true)
	require.NoError(t, enc.Encode(sim))

	dec := msgpack.NewDecoder(&buf)
	dec.IncludeUnexported(true)
	var decoded PoolSimulator
	require.NoError(t, dec.Decode(&decoded))
	require.Equal(t, productionPSMCodeHash, decoded.StaticExtra.PSMCodeHash)
	require.Equal(t, productionFeeHookCodeHash, decoded.StaticExtra.FeeHookCodeHash)
	require.Equal(t, unitCaller, decoded.StaticExtra.FeeCaller)
	require.Equal(t, productionFeeCallerCodeHash, decoded.StaticExtra.FeeCallerCodeHash)
	got, err := decoded.CalcAmountOut(params)
	require.NoError(t, err)
	require.Equal(t, want.TokenAmountOut.Amount, got.TokenAmountOut.Amount)

	decoded.StaticExtra.ProfileVersion = 0
	_, err = decoded.CalcAmountOut(params)
	require.ErrorIs(t, err, ErrUnsupportedProfile)

	decoded.StaticExtra.ProfileVersion = productionProfileVersion
	decoded.Extra.AvailableReserve = big.NewInt(-1)
	_, err = decoded.CalcAmountOut(params)
	require.ErrorIs(t, err, ErrInvalidSnapshot)
}
