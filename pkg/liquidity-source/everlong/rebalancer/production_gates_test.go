package everlongrebalancer

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

func productionGateFixture() (StaticExtra, *rpcState) {
	swapper := common.HexToAddress("0x0000000000000000000000000000000000000011")
	rawAllowlist := common.HexToAddress("0x0000000000000000000000000000000000000012")
	se := StaticExtra{
		Swapper:                    swapper.Hex(),
		ALM:                        "0x0000000000000000000000000000000000000013",
		UnderlyingCvamm:            "0x0000000000000000000000000000000000000014",
		MintAllowlist:              "0x0000000000000000000000000000000000000015",
		UnderlyingDepositAllowlist: rawAllowlist.Hex(),
		ImplementationCodeHash:     supportedRebalancerImplementationCodeHash,
		SwapperCodeHash:            supportedSettlementSwapperCodeHash,
		MathCodeHash:               supportedCollRebalancerMathCodeHash,
	}
	rd := &rpcState{
		settlementSw:             swapper,
		flashFeeBp:               new(big.Int),
		mintAllowed:              big.NewInt(1),
		rawDepositAllowlist:      rawAllowlist,
		underlyingDepositAllowed: big.NewInt(1),
		almPaused:                new(big.Int),
	}
	return se, rd
}

func TestIndependentDepositAllowlistGates(t *testing.T) {
	t.Run("both memberships open", func(t *testing.T) {
		se, rd := productionGateFixture()
		lev, dlv := gateReasons(&se, rd)
		require.Empty(t, lev)
		require.Empty(t, dlv)
	})

	t.Run("swapper missing from wrapper list", func(t *testing.T) {
		se, rd := productionGateFixture()
		rd.mintAllowed.SetInt64(0)
		lev, dlv := gateReasons(&se, rd)
		require.Contains(t, lev, "swapper")
		require.Empty(t, dlv, "withdraw does not traverse either deposit gate")
	})

	t.Run("wrapper missing from raw ALM list", func(t *testing.T) {
		se, rd := productionGateFixture()
		rd.underlyingDepositAllowed.SetInt64(0)
		lev, dlv := gateReasons(&se, rd)
		require.Contains(t, lev, "wrapper")
		require.Empty(t, dlv)
	})

	t.Run("raw list rotation", func(t *testing.T) {
		se, rd := productionGateFixture()
		rd.rawDepositAllowlist = common.HexToAddress("0x0000000000000000000000000000000000000099")
		lev, dlv := gateReasons(&se, rd)
		require.Contains(t, lev, "rotated")
		require.Empty(t, dlv)
	})

	t.Run("raw list read failed", func(t *testing.T) {
		se, rd := productionGateFixture()
		rd.rawDepositAllowlist = common.Address{}
		lev, dlv := gateReasons(&se, rd)
		require.Contains(t, lev, "unreadable")
		require.Empty(t, dlv)
	})
}

func TestRuntimeAndInterestProfilesFailClosed(t *testing.T) {
	t.Run("runtime attestations", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*StaticExtra)
		}{
			{"implementation", func(se *StaticExtra) { se.ImplementationCodeHash = "0xdeadbeef" }},
			{"swapper", func(se *StaticExtra) { se.SwapperCodeHash = "0xdeadbeef" }},
			{"math", func(se *StaticExtra) { se.MathCodeHash = "0xdeadbeef" }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				se, rd := productionGateFixture()
				tc.mutate(&se)
				lev, dlv := gateReasons(&se, rd)
				require.NotEmpty(t, lev)
				require.NotEmpty(t, dlv)
			})
		}
	})

	t.Run("non-zero interest", func(t *testing.T) {
		se, rd := productionGateFixture()
		se.PositionManager = "0x0000000000000000000000000000000000000021"
		rd.interestRate = big.NewInt(1)
		rd.pmPaused = new(big.Int)
		rd.pmSunsetting = new(big.Int)
		rd.maxSysDebt = big.NewInt(1)
		rd.defaultedDebt = new(big.Int)
		rd.activeDebt = new(big.Int)
		rd.borrowingRate = new(big.Int)
		lev, dlv := gateReasons(&se, rd)
		require.Contains(t, lev, "interest")
		require.Contains(t, dlv, "interest")
	})
}

func TestCoupledFactoryRejectsEveryUnknownRuntime(t *testing.T) {
	known := newTestPoolSimulator(t)
	for _, tc := range []struct {
		name string
		err  error
		set  func(*StaticExtra)
	}{
		{"implementation", ErrUnsupportedImplementation, func(se *StaticExtra) { se.ImplementationCodeHash = "" }},
		{"swapper", ErrUnsupportedSwapper, func(se *StaticExtra) { se.SwapperCodeHash = "" }},
		{"math", ErrUnsupportedMath, func(se *StaticExtra) { se.MathCodeHash = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPoolEntity(t)
			var se StaticExtra
			require.NoError(t, json.Unmarshal([]byte(p.StaticExtra), &se))
			tc.set(&se)
			raw, err := json.Marshal(se)
			require.NoError(t, err)
			p.StaticExtra = string(raw)
			_, err = NewPoolSimulatorWithBases(p,
				map[string]pool.IPoolSimulator{se.UnderlyingCvamm: known.basePool})
			require.ErrorIs(t, err, tc.err)
		})
	}
}
