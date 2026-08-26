package everlongrebalancer

import (
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/goccy/go-json"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	everlongcvamm "github.com/KyberNetwork/kyberswap-dex-lib/pkg/liquidity-source/everlong/cvamm"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
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
	staticExtra := StaticExtra{
		Rebalancer:                 "0xa6b848d899189d263a9398f1df4534af7b06d6b3",
		Swapper:                    "0x27775ec38e2b394738b73c0d25f63e20063df054",
		CollVault:                  "0x9e7f375c351a251e80eb89ad33ca62b270fd9b4a",
		ALM:                        "0xbd10884d6b55eda1d872cd5108b8aabdc0c3f6ca",
		ALMAdapterCodeHash:         supportedAlmAdapterCodeHash,
		ImplementationCodeHash:     supportedRebalancerImplementationCodeHash,
		SwapperCodeHash:            supportedSettlementSwapperCodeHash,
		MathCodeHash:               supportedCollRebalancerMathCodeHash,
		UnderlyingCvamm:            "0xf5124f5605ce1e91a7429b837b7dac8f9e5378dd",
		UnderlyingDepositAllowlist: "0x39775655b6dac328fed814b732d688b0ff85cbd4",
		CvDecimalsOffset:           f.CvDecimalsOffset,
		Math:                       "0x4ebd7a6543ace6076f089082931c380a3675bc5c",
		PositionManager:            "0x0000000000000000000000000000000000000021",
		BorrowerOperations:         "0x0000000000000000000000000000000000000022",
		Core:                       "0x0000000000000000000000000000000000000023",
		Implementation:             "0xa4e063cdbfd0f309055dad68278038790bd05df1",
		StableToken:                testNECT,
		DebtGasCompensation:        bi(t, f.DebtGasCompensation),
		ManagedVault:               "0x0000000000000000000000000000000000000024",
		CurveParams:                berachainCurveParams(),
		VolatileToken:              testWBTC,
		DexID:                      DexType,
		ChainID:                    valueobject.ChainIDBerachain,
	}
	staticExtra.ConfigHash, err = staticConfigFingerprint(&staticExtra)
	require.NoError(t, err)
	staticExtra.ProfileHash, err = staticProfileFingerprint(&staticExtra)
	require.NoError(t, err)
	staticExtraBytes, err := json.Marshal(staticExtra)
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

func sealTestStaticProfile(t *testing.T, se *StaticExtra) {
	t.Helper()
	var err error
	se.ProfileHash, err = staticProfileFingerprint(se)
	require.NoError(t, err)
}

func mutateTestStaticExtra(t *testing.T, p *entity.Pool, mutate func(*StaticExtra)) {
	t.Helper()
	var se StaticExtra
	require.NoError(t, json.Unmarshal([]byte(p.StaticExtra), &se))
	mutate(&se)
	raw, err := json.Marshal(se)
	require.NoError(t, err)
	p.StaticExtra = string(raw)
}

func TestNewPoolSimulatorRejectsInvalidImmutableProfile(t *testing.T) {
	for _, tc := range []struct {
		name   string
		want   error
		mutate func(*testing.T, *entity.Pool)
	}{
		{
			name: "pool is not swapper",
			want: ErrInvalidPoolProfile,
			mutate: func(_ *testing.T, p *entity.Pool) {
				p.Address = "0x00000000000000000000000000000000000000ff"
			},
		},
		{
			name: "not exactly two tokens",
			want: ErrInvalidPoolProfile,
			mutate: func(_ *testing.T, p *entity.Pool) {
				p.Tokens = p.Tokens[:1]
			},
		},
		{
			name: "token zero is not stable",
			want: ErrInvalidPoolProfile,
			mutate: func(_ *testing.T, p *entity.Pool) {
				p.Tokens[0].Address = "0x00000000000000000000000000000000000000ff"
			},
		},
		{
			name: "required address absent",
			want: ErrInvalidPoolProfile,
			mutate: func(t *testing.T, p *entity.Pool) {
				mutateTestStaticExtra(t, p, func(se *StaticExtra) { se.Math = "" })
			},
		},
		{
			name: "runtime unsupported",
			want: ErrUnsupportedImplementation,
			mutate: func(t *testing.T, p *entity.Pool) {
				mutateTestStaticExtra(t, p, func(se *StaticExtra) {
					se.ImplementationCodeHash = "0xdeadbeef"
				})
			},
		},
		{
			name: "curve word absent",
			want: ErrInvalidCurveParams,
			mutate: func(t *testing.T, p *entity.Pool) {
				mutateTestStaticExtra(t, p, func(se *StaticExtra) { se.CurveParams.BezierPhi[2] = nil })
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestPoolEntity(t)
			tc.mutate(t, &p)
			_, err := NewPoolSimulator(p)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func newTestPoolSimulator(t *testing.T) *PoolSimulator {
	p := newTestPoolEntity(t)
	baseAddress := "0xf5124f5605ce1e91a7429b837b7dac8f9e5378dd"
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
	// Keep this composed-venue fixture in the current CVAMM cache format. These two
	// digests deliberately mirror that package's v1 configured-identity and immutable
	// static-profile tuples; a future profile version must update this cross-package
	// fixture rather than silently constructing a stale base pool.
	baseProfile, err := json.Marshal(struct {
		Version       uint64              `json:"version"`
		DexID         string              `json:"dexId"`
		ChainID       valueobject.ChainID `json:"chainId"`
		ALM           string              `json:"alm"`
		Adapter       string              `json:"adapter"`
		GasStableIn   int64               `json:"gasStableIn"`
		GasVolatileIn int64               `json:"gasVolatileIn"`
	}{
		Version: 1, DexID: everlongcvamm.DexType, ChainID: valueobject.ChainIDBerachain,
		ALM: baseAddress,
	})
	require.NoError(t, err)
	baseStaticExtra := everlongcvamm.StaticExtra{
		ProfileVersion:         1,
		DexID:                  everlongcvamm.DexType,
		ChainID:                valueobject.ChainIDBerachain,
		ALM:                    baseAddress,
		Token0:                 testNECT,
		Token1:                 testWBTC,
		ConfigHash:             crypto.Keccak256Hash(baseProfile).Hex(),
		Implementation:         "0x0a430e21ecad92d8eb556ff5101db9b973a6dba8",
		ImplementationCodeHash: "0xcc6532930b94e24d165751accb1e4283bf6effe09e19e894ae2be5aa0643eba3",
	}
	staticProfile, err := json.Marshal(struct {
		Version                uint64              `json:"version"`
		DexID                  string              `json:"dexId"`
		ChainID                valueobject.ChainID `json:"chainId"`
		ALM                    string              `json:"alm"`
		Token0                 string              `json:"token0"`
		Token1                 string              `json:"token1"`
		Implementation         string              `json:"implementation"`
		ImplementationCodeHash string              `json:"implementationCodeHash"`
		Adapter                string              `json:"adapter"`
		GasStableIn            int64               `json:"gasStableIn"`
		GasVolatileIn          int64               `json:"gasVolatileIn"`
	}{
		Version:                baseStaticExtra.ProfileVersion,
		DexID:                  baseStaticExtra.DexID,
		ChainID:                baseStaticExtra.ChainID,
		ALM:                    baseStaticExtra.ALM,
		Token0:                 baseStaticExtra.Token0,
		Token1:                 baseStaticExtra.Token1,
		Implementation:         baseStaticExtra.Implementation,
		ImplementationCodeHash: strings.ToLower(baseStaticExtra.ImplementationCodeHash),
		Adapter:                baseStaticExtra.Adapter,
		GasStableIn:            baseStaticExtra.GasStableIn,
		GasVolatileIn:          baseStaticExtra.GasVolatileIn,
	})
	require.NoError(t, err)
	baseStaticExtra.ProfileHash = crypto.Keccak256Hash(staticProfile).Hex()
	baseStatic, err := json.Marshal(baseStaticExtra)
	require.NoError(t, err)
	base, err := everlongcvamm.NewPoolSimulator(entity.Pool{
		Address:     baseAddress,
		Exchange:    everlongcvamm.DexType,
		Type:        everlongcvamm.DexType,
		Tokens:      p.Tokens,
		Reserves:    entity.PoolReserves{"275607106040001229469", "422007"},
		Extra:       string(baseExtra),
		StaticExtra: string(baseStatic),
		BlockNumber: p.BlockNumber,
	})
	require.NoError(t, err)

	sim, err := NewPoolSimulatorWithBases(p, map[string]pool.IPoolSimulator{base.GetAddress(): base})
	require.NoError(t, err)
	return sim
}

func TestComposedCvammFixtureUsesCurrentProfile(t *testing.T) {
	sim := newTestPoolSimulator(t)
	require.NotNil(t, sim.basePool,
		"the cross-package fixture must satisfy the current CVAMM cache profile")
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

func TestDeleverageUsesExactPhysicalNetWithoutDust(t *testing.T) {
	sim := newTestPoolSimulator(t)
	amountIn := new(big.Int).Mul(big.NewInt(5), big.NewInt(1e18))
	result, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testNECT, Amount: amountIn},
		TokenOut:      testWBTC,
	})
	require.NoError(t, err)
	si := result.SwapInfo.(SwapInfo)
	require.False(t, si.IsLeverage)
	physicalNet := new(big.Int).Sub(si.GrossStableIn, si.StableLeg)
	require.Equal(t, physicalNet.String(),
		new(big.Int).Sub(amountIn, result.RemainingTokenAmountIn.Amount).String(),
		"the route must deliver exactly gross minus the raw ALM's physical stable release")

	next := new(big.Int).Add(si.GrossStableIn, big.NewInt(1))
	if next.Cmp(sim.curveParams().maxDeleverageIn(&sim.Extra)) <= 0 {
		stableOut, ok := sim.curveParams().physicalDeleverageStableAt(&sim.Extra, next)
		if ok {
			require.True(t, new(big.Int).Sub(next, stableOut).Cmp(amountIn) > 0,
				"the exact physical-net inversion must choose the largest fitting gross")
		}
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

// TestBothDirectionsDisabledByBorrowInterest: exact execution checks system-wide debt
// totals as well as this position. Until all of those totals can be projected to quote
// time, any non-zero rate makes both directions unsupported.
func TestBothDirectionsDisabledByBorrowInterest(t *testing.T) {
	sim := newTestPoolSimulator(t)
	sim.Extra.InterestRate = big.NewInt(1)

	_, err := sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testWBTC, Amount: big.NewInt(37048)},
		TokenOut:      testNECT,
	})
	require.ErrorIs(t, err, ErrInterestRateUnsupported)

	net, _ := new(big.Int).SetString("500660601604514559", 10)
	_, err = sim.CalcAmountOut(pool.CalcAmountOutParams{
		TokenAmountIn: pool.TokenAmount{Token: testNECT, Amount: net},
		TokenOut:      testWBTC,
	})
	require.ErrorIs(t, err, ErrInterestRateUnsupported)
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
