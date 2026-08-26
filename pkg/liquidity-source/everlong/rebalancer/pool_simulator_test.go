package everlongrebalancer

import (
	"math/big"
	"os"
	"testing"

	"github.com/goccy/go-json"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	everlongcvamm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/cvamm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

const (
	testNECT = "0x1ce0a25d13ce4d52071ae7e02cf1f6606f4c79d3" // stable, 18 dec
	testWBTC = "0x0555e30da8f98308edb960aa94c0db47230d2b9c" // volatile, 8 dec
)

// fixture is a pinned-block state capture from live Berachain (block 24710262): the
// rebalancer + ALM adapter + CollVault snapshot the simulator prices from. Quote-level
// wei-exactness is asserted against the shipped-library fixture grid in math_test.go;
// the tests here exercise the composed venue behavior on a real state.
type fixture struct {
	Block                            int64  `json:"block"`
	C                                string `json:"C"`
	D                                string `json:"D"`
	R                                string `json:"R"`
	Spread                           int64  `json:"spread"`
	AlmStableReserve                 string `json:"almStableReserve"`
	AlmVolatileReserve               string `json:"almVolatileReserve"`
	AlmSupply                        string `json:"almSupply"`
	CvTotalAssets                    string `json:"cvTotalAssets"`
	CvTotalSupply                    string `json:"cvTotalSupply"`
	CvDecimalsOffset                 uint8  `json:"cvDecimalsOffset"`
	WithdrawFeeBp                    int64  `json:"withdrawFeeBp"`
	RefStableReserve                 string `json:"refStableReserve"`
	RefAssetReserve                  string `json:"refAssetReserve"`
	RefRawReferenceWad               string `json:"refRawReferenceWad"`
	MinNetDebt                       string `json:"minNetDebt"`
	DebtGasCompensation              string `json:"debtGasCompensation"`
	MaxDeleverageInExpected          string `json:"maxDeleverageInExpected"`
	MaxLotBoundaryRefRawReferenceWad string `json:"maxLotBoundaryRefRawReferenceWad"`
	MaxLotBoundaryExpected           string `json:"maxLotBoundaryExpected"`
	LeverageExecutions               []struct {
		Shares       string `json:"shares"`
		VolatileIn   string `json:"volatileIn"`
		NetStableOut string `json:"netStableOut"`
	} `json:"leverageExecutions"`
	LeverageRevertsAboveMaxLot string `json:"leverageRevertsAboveMaxLot"`
	DeleverageExecutions       []struct {
		GrossStableIn string `json:"grossStableIn"`
		NetStableIn   string `json:"netStableIn"`
		VolatileOut   string `json:"volatileOut"`
	} `json:"deleverageExecutions"`
	DeleverageRevertsAtFullDebt string `json:"deleverageRevertsAtFullDebt"`
	PreviewOracle               []struct {
		Shares   string `json:"shares"`
		Mint     bool   `json:"mint"`
		Stable   string `json:"stable"`
		Volatile string `json:"volatile"`
	} `json:"previewOracle"`
}

func bi(t *testing.T, s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	require.True(t, ok, "bad big int fixture %q", s)
	return v
}

func loadFixture(t *testing.T) fixture {
	raw, err := os.ReadFile("testdata/berachain_block_24710262.json")
	require.NoError(t, err)
	var f fixture
	require.NoError(t, json.Unmarshal(raw, &f))
	return f
}

func vaultStateFromFixture(t *testing.T, f fixture) *VaultState {
	return &VaultState{
		Collateral: bi(t, f.C), Debt: bi(t, f.D), PriceWad: bi(t, f.R),
		SpreadPpm:        big.NewInt(f.Spread),
		AlmStableReserve: bi(t, f.AlmStableReserve), AlmVolatileReserve: bi(t, f.AlmVolatileReserve),
		AlmSupply:     bi(t, f.AlmSupply),
		CvTotalAssets: bi(t, f.CvTotalAssets), CvTotalSupply: bi(t, f.CvTotalSupply),
		CvDecimalsOffset: f.CvDecimalsOffset, WithdrawFeeBp: big.NewInt(f.WithdrawFeeBp),
		MinNetDebt: bi(t, f.MinNetDebt), DebtGasCompensation: bi(t, f.DebtGasCompensation),
		RefStableReserve: bi(t, f.RefStableReserve), RefAssetReserve: bi(t, f.RefAssetReserve),
		RefRawReferenceWad: bi(t, f.RefRawReferenceWad),
	}
}

func newTestPoolEntity(t *testing.T) entity.Pool {
	f := loadFixture(t)
	state := vaultStateFromFixture(t, f)
	// Same-block words omitted by the original arithmetic-only fixture. They are now
	// mandatory because a routable simulator must attest the underlying CVAMM book and
	// replay the wrapper RVPS rather than fall back to the pre-fill reservation.
	state.AlmIdleStable = bi(t, "400170421078105")
	state.AlmIdleVolatile = bi(t, "22")
	state.RvpsWad = bi(t, "50542765636070561249556032")
	state.AlmResvPriceWad = bi(t, "638569604086845466156025208308271")
	extraBytes, err := json.Marshal(state)
	require.NoError(t, err)
	staticExtraBytes, err := json.Marshal(StaticExtra{
		Rebalancer:         "0xa6b848d899189d263a9398f1df4534af7b06d6b3",
		Swapper:            "0x27775ec38e2b394738b73c0d25f63e20063df054",
		CollVault:          "0x9e7f375c351a251e80eb89ad33ca62b270fd9b4a",
		ALM:                "0xbd10884d6b55eda1d872cd5108b8aabdc0c3f6ca",
		ALMAdapterCodeHash: supportedAlmAdapterCodeHash,
		UnderlyingCvamm:    "0xf5124f5605ce1e91a7429b837b7dac8f9e5378dd",
		CvDecimalsOffset:   f.CvDecimalsOffset,
		CurveParams:        berachainCurveParams(),
	})
	require.NoError(t, err)

	return entity.Pool{
		Address:     "0x27775ec38e2b394738b73c0d25f63e20063df054",
		Exchange:    "everlong-rebalancer",
		Type:        DexType,
		BlockNumber: 1,
		Tokens: []*entity.PoolToken{
			{Address: testNECT, Decimals: 18, Swappable: true},
			{Address: testWBTC, Decimals: 8, Swappable: true},
		},
		Reserves:    entity.PoolReserves{f.AlmStableReserve, f.AlmVolatileReserve},
		Extra:       string(extraBytes),
		StaticExtra: string(staticExtraBytes),
	}
}

func newTestPoolSimulator(t *testing.T) *PoolSimulator {
	p := newTestPoolEntity(t)
	baseExtra, err := json.Marshal(everlongcvamm.Extra{
		Support: everlongcvamm.Support{
			AWad: uint256.MustFromDecimal("34000000000000000000"),
			XLo:  uint256.MustFromDecimal("31865306097213932"),
			XHi:  uint256.MustFromDecimal("1099912607172170593"),
			YHi:  uint256.MustFromDecimal("24096289941794300"),
		},
		XWad:                uint256.MustFromDecimal("498580384806401754"),
		AnchorSqrtX96:       uint256.MustFromDecimal("2002090499947173553980875996007816177"),
		Kappa:               uint256.MustFromDecimal("22849333026605"),
		FeeStableInWad:      uint256.MustFromDecimal("20962195950181662"),
		FeeVolatileInWad:    uint256.MustFromDecimal("13974797300121108"),
		IdleStable:          uint256.MustFromDecimal("400170421078105"),
		IdleVolatile:        uint256.MustFromDecimal("22"),
		ReservationPriceWad: uint256.MustFromDecimal("638569604086845466156025208308271"),
	})
	require.NoError(t, err)
	base, err := everlongcvamm.NewPoolSimulator(entity.Pool{
		Address:     "0xf5124f5605ce1e91a7429b837b7dac8f9e5378dd",
		Exchange:    everlongcvamm.DexType,
		Type:        everlongcvamm.DexType,
		Tokens:      p.Tokens,
		Reserves:    entity.PoolReserves{"275607106040001229469", "422007"},
		Extra:       string(baseExtra),
		StaticExtra: "{}",
		BlockNumber: p.BlockNumber,
	})
	require.NoError(t, err)

	sim, err := NewPoolSimulatorWithBases(p, map[string]pool.IPoolSimulator{base.GetAddress(): base})
	require.NoError(t, err)
	return sim
}

// TestPreviewTokenAmountsMatchesDeployedSwapper: the ERC-4626 -> ALM proportional
// split is the one leg of the settlement path the CollRebalancerMath grid cannot
// cover, so it is pinned against the DEPLOYED CollateralRebalancerSwapper's own
// previewTokenAmounts at the same state — both roundings (mint up / redeem down).
func TestPreviewTokenAmountsMatchesDeployedSwapper(t *testing.T) {
	f := loadFixture(t)
	require.NotEmpty(t, f.PreviewOracle)
	state := vaultStateFromFixture(t, f)

	for i, tc := range f.PreviewOracle {
		stable, volatile, ok := state.previewTokenAmounts(bi(t, tc.Shares), tc.Mint)
		require.True(t, ok, "case %d shares=%s mint=%v", i, tc.Shares, tc.Mint)
		require.Equal(t, tc.Stable, stable.String(),
			"case %d shares=%s mint=%v stable", i, tc.Shares, tc.Mint)
		require.Equal(t, tc.Volatile, volatile.String(),
			"case %d shares=%s mint=%v volatile", i, tc.Shares, tc.Mint)
	}

	// Zero shares reverts NothingToFill on-chain; the port must decline, not quote.
	_, _, ok := state.previewTokenAmounts(big.NewInt(0), true)
	require.False(t, ok)
	_, _, ok = state.previewTokenAmounts(big.NewInt(0), false)
	require.False(t, ok)
}

// quoteableDirection picks the direction the pinned state actually accepts: the CR
// curve realigns one way at a time, so exactly one of the two should quote.
func quoteableDirection(t *testing.T, sim *PoolSimulator) (tokenIn, tokenOut string, amountIn *big.Int) {
	f := loadFixture(t)
	state := vaultStateFromFixture(t, f)
	cp := berachainCurveParams()
	dir, fullIn, fullOut := cp.rebalanceState(state)
	require.NotZero(t, dir, "pinned state must accept a fill in one direction")
	require.True(t, fullIn.Sign() > 0 && fullOut.Sign() > 0)
	if dir == 1 { // leverage: volatile in
		_, volatileIn, ok := state.previewTokenAmounts(fullIn, true)
		require.True(t, ok)
		return testWBTC, testNECT, volatileIn
	}
	// deleverage: net stable in
	stableOut, _, ok := cp.deleverageLegsAt(state, fullIn)
	require.True(t, ok)
	return testNECT, testWBTC, new(big.Int).Sub(fullIn, stableOut)
}

// TestCalcAmountOutComposesQuotes: end-to-end on the pinned state. The consumed leg
// plus the reported remainder must equal the input, and the output must re-derive
// exactly from the checked quotes at the sizes SwapInfo reports.
func TestCalcAmountOutComposesQuotes(t *testing.T) {
	sim := newTestPoolSimulator(t)
	tokenIn, tokenOut, amountIn := quoteableDirection(t, sim)

	result, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn},
		TokenOut:      tokenOut,
	})
	require.NoError(t, err)
	require.True(t, result.TokenAmountOut.Amount.Sign() > 0)

	si, ok := result.SwapInfo.(SwapInfo)
	require.True(t, ok)

	f := loadFixture(t)
	state := vaultStateFromFixture(t, f)
	cp := berachainCurveParams()
	if si.IsLeverage {
		require.True(t, si.VolatileLeg.Cmp(amountIn) <= 0, "must not consume more than the input")
		spent := new(big.Int).Add(si.VolatileLeg, result.RemainingTokenAmountIn.Amount)
		require.Zero(t, amountIn.Cmp(spent), "consumed + remaining must equal the input")
		oracleNet, ok2 := cp.quoteLeverageAt(state, si.CollVaultShares)
		require.True(t, ok2)
		require.Zero(t, oracleNet.Cmp(result.TokenAmountOut.Amount))
	} else {
		net := new(big.Int).Sub(si.GrossStableIn, si.StableLeg)
		require.True(t, net.Cmp(amountIn) <= 0, "forward net must fit within the input")
		spent := new(big.Int).Add(net, result.RemainingTokenAmountIn.Amount)
		require.Zero(t, amountIn.Cmp(spent), "net + remaining must equal the input")
		stableOut, volatileOut, lok := cp.deleverageLegsAt(state, si.GrossStableIn)
		require.True(t, lok)
		require.Zero(t, stableOut.Cmp(si.StableLeg))
		require.Zero(t, volatileOut.Cmp(result.TokenAmountOut.Amount))
	}
}

// TestCalcAmountOutIsPureAndDeterministic: repeated quoting must not drift.
func TestCalcAmountOutIsPureAndDeterministic(t *testing.T) {
	sim := newTestPoolSimulator(t)
	tokenIn, tokenOut, amountIn := quoteableDirection(t, sim)
	params := pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn},
		TokenOut:      tokenOut,
	}
	first, err := sim.CalcAmountOut(params)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		again, err := sim.CalcAmountOut(params)
		require.NoError(t, err)
		require.Zero(t, first.TokenAmountOut.Amount.Cmp(again.TokenAmountOut.Amount))
	}
}

// TestUpdateBalanceAndClone: after a fill the same request must price differently
// (state moved), while a pre-update clone still prices the original.
func TestUpdateBalanceAndClone(t *testing.T) {
	sim := newTestPoolSimulator(t)
	tokenIn, tokenOut, amountIn := quoteableDirection(t, sim)
	params := pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: tokenIn, Amount: amountIn},
		TokenOut:      tokenOut,
	}
	first, err := sim.CalcAmountOut(params)
	require.NoError(t, err)

	backup := sim.CloneState()

	sim.UpdateBalance(pool.UpdateBalanceParams{
		TokenAmountIn:  pool.TokenAmount{Token: tokenIn, Amount: amountIn},
		TokenAmountOut: *first.TokenAmountOut,
		Fee:            *first.Fee,
		SwapInfo:       first.SwapInfo,
	})

	second, err := sim.CalcAmountOut(params)
	if err == nil {
		require.NotZero(t, second.TokenAmountOut.Amount.Cmp(first.TokenAmountOut.Amount),
			"a fill moves the position — the next quote must differ")
	}

	fromBackup, err := backup.CalcAmountOut(params)
	require.NoError(t, err)
	require.Zero(t, first.TokenAmountOut.Amount.Cmp(fromBackup.TokenAmountOut.Amount),
		"CloneState must fully insulate the backup from UpdateBalance")
}

// TestExecutionParity is the end of the evidence chain for this venue: these are REAL
// settled fills through the deployed CollateralRebalancerSwapper at the pinned state.
// Both directions are asserted on the venue's own return values, and both refusal
// boundaries are asserted too — the leverage lot above the physical-CR cap, and the
// deleverage size above the CDP's minimum-net-debt bound.
func TestExecutionParity(t *testing.T) {
	f := loadFixture(t)
	require.NotEmpty(t, f.LeverageExecutions)
	require.NotEmpty(t, f.DeleverageExecutions)
	state := vaultStateFromFixture(t, f)
	cp := berachainCurveParams()

	// The CDP's minimum-net-debt bound, wei-exact against the on-chain boundary.
	require.Equal(t, f.MaxDeleverageInExpected, cp.maxDeleverageIn(state).String(),
		"deleverage lot must stop at debt - gasComp - minNetDebt")

	maxLot := cp.maxLeverageShares(state)
	for i, e := range f.LeverageExecutions {
		shares := bi(t, e.Shares)
		_, volatileIn, ok := state.previewTokenAmounts(shares, true)
		require.True(t, ok, "leverage case %d", i)
		require.Equal(t, e.VolatileIn, volatileIn.String(), "leverage case %d volatileIn", i)
		net, ok := cp.quoteLeverageAt(state, shares)
		require.True(t, ok, "leverage case %d", i)
		require.Equal(t, e.NetStableOut, net.String(), "leverage case %d netStableOut", i)
		require.True(t, shares.Cmp(maxLot) <= 0, "leverage case %d must sit within the max lot", i)
	}
	// A lot above the physical-CR cap reverts on-chain, so the cap must exclude it.
	require.True(t, bi(t, f.LeverageRevertsAboveMaxLot).Cmp(maxLot) > 0,
		"the max-lot cap must exclude the size that reverts on-chain")

	for i, e := range f.DeleverageExecutions {
		gross := bi(t, e.GrossStableIn)
		stableOut, volatileOut, ok := cp.deleverageLegsAt(state, gross)
		require.True(t, ok, "deleverage case %d", i)
		require.Equal(t, e.VolatileOut, volatileOut.String(), "deleverage case %d volatileOut", i)
		// The venue pulls the forward net; it floors where the swapper's own accounting
		// rounds up, so allow the documented one-wei-low difference and no more.
		net := new(big.Int).Sub(gross, stableOut)
		diff := new(big.Int).Sub(bi(t, e.NetStableIn), net)
		require.True(t, diff.Sign() >= 0 && diff.Cmp(big.NewInt(1)) <= 0,
			"deleverage case %d net: chain=%s local=%s (diff %s must be 0 or 1 wei)",
			i, e.NetStableIn, net, diff)
	}
	// Retiring the full debt reverts on-chain (it would leave net debt under the CDP
	// minimum), so the simulator must never size a fill that large.
	require.True(t, bi(t, f.DeleverageRevertsAtFullDebt).Cmp(cp.maxDeleverageIn(state)) > 0,
		"the deleverage cap must exclude the full-debt repayment that reverts on-chain")
}

// TestLeverageDisabledByBorrowInterest: the rebalancer refuses to originate debt while
// the CDP charges borrow interest, so leverage must stop quoting — while deleverage,
// which only retires debt, stays live.
func TestLeverageDisabledByBorrowInterest(t *testing.T) {
	sim := newTestPoolSimulator(t)
	sim.Extra.InterestRate = big.NewInt(1)

	_, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testWBTC, Amount: big.NewInt(37048)},
		TokenOut:      testNECT,
	})
	require.ErrorIs(t, err, ErrLeverageDisabled)

	net, _ := new(big.Int).SetString("500660601604514559", 10)
	res, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testNECT, Amount: net},
		TokenOut:      testWBTC,
	})
	require.NoError(t, err, "deleverage only retires debt and must stay available")
	require.True(t, res.TokenAmountOut.Amount.Sign() > 0)
}

// TestMaxLeverageLotMatchesVenueBoundary pins the physical-CR bound against the venue
// itself: binary-searching the deployed swapper on a fork found the largest lot it
// accepts, and the model must reproduce it exactly — one share more reverts on-chain.
// This is what allows the cap to be applied with no safety shave.
func TestMaxLeverageLotMatchesVenueBoundary(t *testing.T) {
	f := loadFixture(t)
	state := vaultStateFromFixture(t, f)
	state.RefRawReferenceWad = bi(t, f.MaxLotBoundaryRefRawReferenceWad)
	cp := berachainCurveParams()
	require.Equal(t, f.MaxLotBoundaryExpected, cp.maxLeverageShares(state).String(),
		"the modelled max lot must equal the venue's own boundary")
}
