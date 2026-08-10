package everlongpsm

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

// Vectors are hand-derived from PermissionlessPSM.sol (previewDeposit/previewRedeem +
// the execution-path cap and accounting checks) for an 18d debt token against a 6d
// stable (wadOffset 1e12); to be cross-checked against the deployed bytecode once the
// PSM ships (no deployment exists yet — see the deployment script's EverUSD instance).
func simWith(t *testing.T, capWad, minted, reserve, entryBp, exitBp string, paused bool) *PoolSimulator {
	t.Helper()
	p := entity.Pool{
		Address:  "0xpsm-0xusdc",
		Exchange: "everlong-psm",
		Type:     DexType,
		Tokens: []*entity.PoolToken{
			{Address: "0xdebt", Decimals: 18, Swappable: true},
			{Address: "0xusdc", Decimals: 6, Swappable: true},
		},
		Reserves: entity.PoolReserves{"0", "0"},
		StaticExtra: `{"psm":"0xpsm","wadOffset":1000000000000}`,
		Extra: `{"paused":` + map[bool]string{true: "true", false: "false"}[paused] +
			`,"entryBp":` + entryBp + `,"exitBp":` + exitBp +
			`,"cap":` + capWad + `,"minted":` + minted + `,"reserve":` + reserve + `}`,
	}
	sim, err := NewPoolSimulator(p)
	require.NoError(t, err)
	return sim
}

func quote(t *testing.T, s *PoolSimulator, tokenIn, tokenOut, amountIn string) (*pool.CalcAmountOutResult, error) {
	t.Helper()
	in, _ := new(big.Int).SetString(amountIn, 10)
	return s.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: in},
		TokenOut:      tokenOut,
	})
}

func TestDeposit(t *testing.T) {
	// no fee: 100 USDC -> 100 debt
	s := simWith(t, "1000000000000000000000", "0", "0", "0", "0", false)
	res, err := quote(t, s, "0xusdc", "0xdebt", "100000000")
	require.NoError(t, err)
	require.Equal(t, "100000000000000000000", res.TokenAmountOut.Amount.String())
	require.Zero(t, res.RemainingTokenAmountIn.Amount.Sign())

	// 10bp entry fee: gross 100e18, fee 1e17, out 99.9e18
	s = simWith(t, "1000000000000000000000", "0", "0", "10", "0", false)
	res, err = quote(t, s, "0xusdc", "0xdebt", "100000000")
	require.NoError(t, err)
	require.Equal(t, "99900000000000000000", res.TokenAmountOut.Amount.String())
	require.Equal(t, "100000000000000000000", res.SwapInfo.(SwapInfo).GrossDebt.String())

	// cap clamp: 50 debt of room -> 50 USDC used, 50 returned
	s = simWith(t, "50000000000000000000", "0", "0", "0", "0", false)
	res, err = quote(t, s, "0xusdc", "0xdebt", "100000000")
	require.NoError(t, err)
	require.Equal(t, "50000000000000000000", res.TokenAmountOut.Amount.String())
	require.Equal(t, "50000000", res.RemainingTokenAmountIn.Amount.String())

	// exhausted cap rejects
	s = simWith(t, "10000000000000000000", "10000000000000000000", "0", "0", "0", false)
	_, err = quote(t, s, "0xusdc", "0xdebt", "1000000")
	require.ErrorIs(t, err, ErrCapExhausted)

	// paused rejects
	s = simWith(t, "1000000000000000000000", "0", "0", "0", "0", true)
	_, err = quote(t, s, "0xusdc", "0xdebt", "1000000")
	require.ErrorIs(t, err, ErrPaused)
}

func TestRedeem(t *testing.T) {
	// 25bp exit fee: 100 debt -> gross 100e6, fee 250000, out 99.75 USDC
	s := simWith(t, "0", "200000000000000000000", "200000000", "0", "25", false)
	res, err := quote(t, s, "0xdebt", "0xusdc", "100000000000000000000")
	require.NoError(t, err)
	require.Equal(t, "99750000", res.TokenAmountOut.Amount.String())
	require.Equal(t, "100000000", res.SwapInfo.(SwapInfo).GrossStable.String())

	// sub-offset dust is returned, not burned for nothing:
	// 1e15+5 debt -> gross 1000, fee ceil(1000*25/1e4)=3, out 997, dust 5 back
	res, err = quote(t, s, "0xdebt", "0xusdc", "1000000000000005")
	require.NoError(t, err)
	require.Equal(t, "997", res.TokenAmountOut.Amount.String())
	require.Equal(t, "5", res.RemainingTokenAmountIn.Amount.String())

	// minted bound clamps the burn
	s = simWith(t, "0", "50000000000000000000", "200000000", "0", "0", false)
	res, err = quote(t, s, "0xdebt", "0xusdc", "100000000000000000000")
	require.NoError(t, err)
	require.Equal(t, "50000000", res.TokenAmountOut.Amount.String())
	require.Equal(t, "50000000000000000000", res.RemainingTokenAmountIn.Amount.String())

	// reserve bound clamps the payout
	s = simWith(t, "0", "200000000000000000000", "30000000", "0", "0", false)
	res, err = quote(t, s, "0xdebt", "0xusdc", "100000000000000000000")
	require.NoError(t, err)
	require.Equal(t, "30000000", res.TokenAmountOut.Amount.String())

	// nothing minted rejects
	s = simWith(t, "0", "0", "200000000", "0", "0", false)
	_, err = quote(t, s, "0xdebt", "0xusdc", "1000000000000")
	require.ErrorIs(t, err, ErrNothingToRedeem)
}

func TestUpdateBalanceRoundTrip(t *testing.T) {
	s := simWith(t, "1000000000000000000000", "0", "0", "10", "25", false)
	res, err := quote(t, s, "0xusdc", "0xdebt", "100000000")
	require.NoError(t, err)
	s.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
	require.Equal(t, "100000000000000000000", s.Extra.DebtTokenMinted.String())
	require.Equal(t, "100000000", s.Extra.StableReserve.String())

	// the deposited stable is now redeemable
	res, err = quote(t, s, "0xdebt", "0xusdc", "99900000000000000000")
	require.NoError(t, err)
	require.True(t, res.TokenAmountOut.Amount.Sign() > 0)

	// clone isolation
	clone := s.CloneState().(*PoolSimulator)
	clone.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
	require.Equal(t, "100000000000000000000", s.Extra.DebtTokenMinted.String())
	require.NotEqual(t, s.Extra.DebtTokenMinted.String(), clone.Extra.DebtTokenMinted.String())
}
