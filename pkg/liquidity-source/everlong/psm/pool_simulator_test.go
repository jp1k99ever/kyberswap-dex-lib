package everlongpsm

import (
	"math/big"
	"testing"

	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

const (
	unitDebt     = "0x0000000000000000000000000000000000000001"
	unitStable   = "0x0000000000000000000000000000000000000002"
	unitPSM      = "0x0000000000000000000000000000000000000003"
	unitMetaCore = "0x0000000000000000000000000000000000000004"
	unitCaller   = "0x0000000000000000000000000000000000000005"
)

func unitConfig() *Config {
	return &Config{DexID: DexType, ChainID: valueobject.ChainIDBerachain,
		PSM: unitPSM, Stables: []string{unitStable}, FeeCaller: unitCaller}
}

func unitStaticExtra(t *testing.T, wadOffset string) string {
	t.Helper()
	offset, ok := new(big.Int).SetString(wadOffset, 10)
	require.True(t, ok)
	require.True(t, offset.IsUint64())
	snapshot := validProfileSnapshot()
	snapshot.WadOffset = offset.Uint64()
	cfg := unitConfig()
	configHash, err := psmConfigHash(cfg)
	require.NoError(t, err)
	static, err := staticExtraFromProfile(snapshot, cfg, configHash)
	require.NoError(t, err)
	b, err := json.Marshal(static)
	require.NoError(t, err)
	return string(b)
}

// Vectors are hand-derived from PermissionlessPSM.sol (previewDeposit/previewRedeem +
// the execution-path capacity and accounting checks) for an 18d debt token against a 6d
// stable (wadOffset 1e12). availMint/reserve are the contract's own hook-aware capacity
// views (availableMint/availableReserve), not raw storage words.
func simWith(t *testing.T, availMint, minted, reserve, entryBp, exitBp string, paused bool) *PoolSimulator {
	t.Helper()
	p := entity.Pool{
		Address:  unitPSM + "-" + unitStable,
		Exchange: "everlong-psm",
		Type:     DexType,
		Tokens: []*entity.PoolToken{
			{Address: unitDebt, Decimals: 18, Swappable: true},
			{Address: unitStable, Decimals: 6, Swappable: true},
		},
		Reserves:    entity.PoolReserves{"0", "0"},
		StaticExtra: unitStaticExtra(t, "1000000000000"),
		Extra: `{"paused":` + map[bool]string{true: "true", false: "false"}[paused] + `,"psmBonded":true` +
			`,"entryBp":` + entryBp + `,"exitBp":` + exitBp +
			`,"availMint":` + availMint + `,"minted":` + minted + `,"reserve":` + reserve + `}`,
		BlockNumber: 1,
	}
	sim, err := NewPoolSimulator(p)
	require.NoError(t, err)
	return sim
}

// The live pair is NECT/HONEY. Both are 18 decimals, so wadOffset is 1 and amounts pass
// through unscaled. Every other test here uses a 6-decimal stable, so this is the only
// one covering what is actually deployed.
func TestEqualDecimalsUnscaled(t *testing.T) {
	p := entity.Pool{
		Address:  unitPSM + "-" + unitStable,
		Exchange: "everlong-psm",
		Type:     DexType,
		Tokens: []*entity.PoolToken{
			{Address: unitDebt, Decimals: 18, Swappable: true},
			{Address: unitStable, Decimals: 18, Swappable: true},
		},
		Reserves:    entity.PoolReserves{"0", "0"},
		StaticExtra: unitStaticExtra(t, "1"),
		// 5 bp both directions, matching the deployed fee law's current output.
		Extra: `{"paused":false,"psmBonded":true,"entryBp":5,"exitBp":5,` +
			`"availMint":1000000000000000000000,"minted":500000000000000000000,` +
			`"reserve":500000000000000000000}`,
		BlockNumber: 1,
	}
	s, err := NewPoolSimulator(p)
	require.NoError(t, err)

	// deposit: 100 HONEY -> gross 100e18, fee ceil(100e18*5/1e4) = 5e16, out 99.95
	res, err := quote(t, s, unitStable, unitDebt, "100000000000000000000")
	require.NoError(t, err)
	require.Equal(t, "99950000000000000000", res.TokenAmountOut.Amount.String())
	require.Zero(t, res.RemainingTokenAmountIn.Amount.Sign(), "offset 1 leaves no dust")

	// redeem: 100 NECT -> gross 100e18 stable, same 5 bp, out 99.95
	res, err = quote(t, s, unitDebt, unitStable, "100000000000000000000")
	require.NoError(t, err)
	require.Equal(t, "99950000000000000000", res.TokenAmountOut.Amount.String())
	require.Zero(t, res.RemainingTokenAmountIn.Amount.Sign())
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
	res, err := quote(t, s, unitStable, unitDebt, "100000000")
	require.NoError(t, err)
	require.Equal(t, "100000000000000000000", res.TokenAmountOut.Amount.String())
	require.Zero(t, res.RemainingTokenAmountIn.Amount.Sign())

	// 10bp entry fee: gross 100e18, fee 1e17, out 99.9e18
	s = simWith(t, "1000000000000000000000", "0", "0", "10", "0", false)
	res, err = quote(t, s, unitStable, unitDebt, "100000000")
	require.NoError(t, err)
	require.Equal(t, "99900000000000000000", res.TokenAmountOut.Amount.String())
	require.Equal(t, "100000000000000000000", res.SwapInfo.(SwapInfo).GrossDebt.String())

	// cap clamp: 50 debt of room -> 50 USDC used, 50 returned
	s = simWith(t, "50000000000000000000", "0", "0", "0", "0", false)
	res, err = quote(t, s, unitStable, unitDebt, "100000000")
	require.NoError(t, err)
	require.Equal(t, "50000000000000000000", res.TokenAmountOut.Amount.String())
	require.Equal(t, "50000000", res.RemainingTokenAmountIn.Amount.String())

	// exhausted cap rejects
	s = simWith(t, "0", "10000000000000000000", "0", "0", "0", false)
	_, err = quote(t, s, unitStable, unitDebt, "1000000")
	require.ErrorIs(t, err, ErrCapExhausted)

	// paused rejects
	s = simWith(t, "1000000000000000000000", "0", "0", "0", "0", true)
	_, err = quote(t, s, unitStable, unitDebt, "1000000")
	require.ErrorIs(t, err, ErrPaused)
}

func TestRedeem(t *testing.T) {
	// 25bp exit fee: 100 debt -> gross 100e6, fee 250000, out 99.75 USDC
	s := simWith(t, "0", "200000000000000000000", "200000000", "0", "25", false)
	res, err := quote(t, s, unitDebt, unitStable, "100000000000000000000")
	require.NoError(t, err)
	require.Equal(t, "99750000", res.TokenAmountOut.Amount.String())
	require.Equal(t, "100000000", res.SwapInfo.(SwapInfo).GrossStable.String())

	// sub-offset dust is returned, not burned for nothing:
	// 1e15+5 debt -> gross 1000, fee ceil(1000*25/1e4)=3, out 997, dust 5 back
	res, err = quote(t, s, unitDebt, unitStable, "1000000000000005")
	require.NoError(t, err)
	require.Equal(t, "997", res.TokenAmountOut.Amount.String())
	require.Equal(t, "5", res.RemainingTokenAmountIn.Amount.String())

	// minted bound clamps the burn
	s = simWith(t, "0", "50000000000000000000", "200000000", "0", "0", false)
	res, err = quote(t, s, unitDebt, unitStable, "100000000000000000000")
	require.NoError(t, err)
	require.Equal(t, "50000000", res.TokenAmountOut.Amount.String())
	require.Equal(t, "50000000000000000000", res.RemainingTokenAmountIn.Amount.String())

	// reserve bound clamps the payout
	s = simWith(t, "0", "200000000000000000000", "30000000", "0", "0", false)
	res, err = quote(t, s, unitDebt, unitStable, "100000000000000000000")
	require.NoError(t, err)
	require.Equal(t, "30000000", res.TokenAmountOut.Amount.String())

	// nothing minted rejects
	s = simWith(t, "0", "0", "200000000", "0", "0", false)
	_, err = quote(t, s, unitDebt, unitStable, "1000000000000")
	require.ErrorIs(t, err, ErrNothingToRedeem)

}

// A direction missing from a legacy/corrupt snapshot must not quote off a stale or
// assumed rate. Current production tracking requires both feeBpFor legs to decode.
func TestClosedDirectionDoesNotQuote(t *testing.T) {
	s := simWith(t, "1000000000000000000000", "200000000000000000000", "200000000", "10", "25", false)
	s.Extra.EntryFeeBp = nil
	_, err := quote(t, s, unitStable, unitDebt, "100000000")
	require.ErrorIs(t, err, ErrInvalidSnapshot)

	s.Extra.ExitFeeBp = nil
	_, err = quote(t, s, unitDebt, unitStable, "100000000000000000000")
	require.ErrorIs(t, err, ErrInvalidSnapshot)
}

func TestUpdateBalanceRoundTrip(t *testing.T) {
	s := simWith(t, "1000000000000000000000", "0", "0", "10", "25", false)
	res, err := quote(t, s, unitStable, unitDebt, "100000000")
	require.NoError(t, err)
	s.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
	require.Equal(t, "100000000000000000000", s.Extra.DebtTokenMinted.String())
	require.Equal(t, "100000000", s.Extra.AvailableReserve.String())
	require.Equal(t, "900000000000000000000", s.Extra.AvailableMint.String())

	// the deposited stable is now redeemable
	res, err = quote(t, s, unitDebt, unitStable, "99900000000000000000")
	require.NoError(t, err)
	require.True(t, res.TokenAmountOut.Amount.Sign() > 0)

	// clone isolation
	clone := s.CloneState().(*PoolSimulator)
	clone.UpdateBalance(pool.UpdateBalanceParams{SwapInfo: res.SwapInfo})
	require.Equal(t, "100000000000000000000", s.Extra.DebtTokenMinted.String())
	require.NotEqual(t, s.Extra.DebtTokenMinted.String(), clone.Extra.DebtTokenMinted.String())
}

// TestNilSnapshotFieldsRefuse: a pool persisted before a field existed unmarshals it as
// nil. The redeem path reads three such fields — one of them only in UpdateBalance, which
// would panic after the quote had already been handed to the router.
func TestNilSnapshotFieldsRefuse(t *testing.T) {
	for _, name := range []string{"minted", "reserve", "availMint"} {
		s := simWith(t, "1000000000000000000000", "200000000000000000000", "200000000", "5", "5", false)
		switch name {
		case "minted":
			s.Extra.DebtTokenMinted = nil
		case "reserve":
			s.Extra.AvailableReserve = nil
		case "availMint":
			s.Extra.AvailableMint = nil
		}
		_, err := quote(t, s, unitDebt, unitStable, "100000000000000000000")
		require.ErrorIs(t, err, ErrInvalidSnapshot, "nil %s must refuse, not panic", name)
	}
}

func TestSimulatorRejectsPreAttestationStaticExtra(t *testing.T) {
	p := entity.Pool{
		Address: unitPSM + "-" + unitStable, Exchange: DexType, Type: DexType,
		Tokens:      []*entity.PoolToken{{Address: unitDebt}, {Address: unitStable}},
		Reserves:    entity.PoolReserves{"0", "0"},
		StaticExtra: `{"psm":"` + unitPSM + `","wadOffset":1}`,
		Extra:       `{"paused":false,"psmBonded":true,"entryBp":5,"exitBp":5,"availMint":1,"minted":1,"reserve":1}`,
		BlockNumber: 1,
	}
	_, err := NewPoolSimulator(p)
	require.ErrorIs(t, err, ErrUnsupportedProfile)
}

func TestCalcAmountOutRejectsBypassedCorruptState(t *testing.T) {
	params := pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: unitStable, Amount: big.NewInt(1_000_000)},
		TokenOut:      unitDebt,
	}
	tests := []struct {
		name   string
		mutate func(*PoolSimulator)
		err    error
	}{
		{"legacy profile", func(s *PoolSimulator) { s.StaticExtra.ProfileVersion = 0 }, ErrUnsupportedProfile},
		{"wrong psm runtime", func(s *PoolSimulator) { s.StaticExtra.PSMCodeHash = "0x01" }, ErrUnsupportedProfile},
		{"revoked psm bond", func(s *PoolSimulator) { s.Extra.PSMBonded = false }, ErrInvalidSnapshot},
		{"missing fee", func(s *PoolSimulator) { s.Extra.EntryFeeBp = nil }, ErrInvalidSnapshot},
		{"negative fee", func(s *PoolSimulator) { s.Extra.EntryFeeBp = big.NewInt(-1) }, ErrInvalidSnapshot},
		{"fee at bp", func(s *PoolSimulator) { s.Extra.ExitFeeBp = new(big.Int).Set(bigBp) }, ErrInvalidSnapshot},
		{"missing mint room", func(s *PoolSimulator) { s.Extra.AvailableMint = nil }, ErrInvalidSnapshot},
		{"missing book", func(s *PoolSimulator) { s.Extra.DebtTokenMinted = nil }, ErrInvalidSnapshot},
		{"missing reserve", func(s *PoolSimulator) { s.Extra.AvailableReserve = nil }, ErrInvalidSnapshot},
		{"negative mint room", func(s *PoolSimulator) { s.Extra.AvailableMint = big.NewInt(-1) }, ErrInvalidSnapshot},
		{"negative book", func(s *PoolSimulator) { s.Extra.DebtTokenMinted = big.NewInt(-1) }, ErrInvalidSnapshot},
		{"negative reserve", func(s *PoolSimulator) { s.Extra.AvailableReserve = big.NewInt(-1) }, ErrInvalidSnapshot},
		{"token binding", func(s *PoolSimulator) { s.Info.Tokens[0] = unitStable }, ErrUnsupportedProfile},
		{"pool address", func(s *PoolSimulator) { s.Info.Address = unitPSM }, ErrUnsupportedProfile},
		{"exchange", func(s *PoolSimulator) { s.Info.Exchange = "detached" }, ErrUnsupportedProfile},
		{"pool type", func(s *PoolSimulator) { s.Info.Type = "detached" }, ErrUnsupportedProfile},
		{"zero block", func(s *PoolSimulator) { s.Info.BlockNumber = 0 }, ErrUnsupportedProfile},
		{"negative deposit gas", func(s *PoolSimulator) { s.StaticExtra.GasDeposit = -1 }, ErrUnsupportedProfile},
		{"negative redeem gas", func(s *PoolSimulator) { s.StaticExtra.GasRedeem = -1 }, ErrUnsupportedProfile},
		{"config hash", func(s *PoolSimulator) { s.StaticExtra.ConfigHash = "0x01" }, ErrUnsupportedProfile},
		{"profile hash", func(s *PoolSimulator) { s.StaticExtra.ProfileHash = "0x01" }, ErrUnsupportedProfile},
		{"dual debt-token mutation", func(s *PoolSimulator) {
			s.StaticExtra.DebtToken = "0x0000000000000000000000000000000000000006"
			s.Info.Tokens[0] = s.StaticExtra.DebtToken
		}, ErrUnsupportedProfile},
		{"dual stable mutation", func(s *PoolSimulator) {
			s.StaticExtra.Stable = "0x0000000000000000000000000000000000000006"
			s.Info.Tokens[1] = s.StaticExtra.Stable
			s.Info.Address = unitPSM + "-" + s.StaticExtra.Stable
		}, ErrUnsupportedProfile},
		{"dual psm mutation", func(s *PoolSimulator) {
			s.StaticExtra.PSM = "0x0000000000000000000000000000000000000006"
			s.Info.Address = s.StaticExtra.PSM + "-" + unitStable
		}, ErrUnsupportedProfile},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sim := simWith(t, "1000000000000000000000", "1", "1", "5", "5", false)
			tc.mutate(sim)
			_, err := sim.CalcAmountOut(params)
			require.ErrorIs(t, err, tc.err)
		})
	}
}
