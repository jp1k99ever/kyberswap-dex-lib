package everlongrebalancer

import (
	"errors"
	"math/big"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

const (
	DexType = "everlong-rebalancer"

	rebalancerMethodCollVault         = "collVault"
	rebalancerMethodPhysicalCrFloor   = "PHYSICAL_CR_FLOOR_WAD"
	rebalancerMethodLeverageCurve     = "leverageCurve"
	rebalancerMethodSettlementSwapper = "settlementSwapper"
	rebalancerMethodExchangeState     = "exchangeState"
	swapperMethodAlm                  = "alm"
	adapterMethodAlm                  = "alm"
	mathMethodDeleverageQuote         = "deleverageQuote"
	almMethodGetTotalAmounts          = "getTotalAmounts"
	almMethodGetReservesAtReference   = "getReservesAtReference"
	erc20MethodTotalSupply            = "totalSupply"
	cvMethodTotalAssets               = "totalAssets"
	cvMethodGetWithdrawFee            = "getWithdrawFee"
	cvMethodAssetDecimals             = "assetDecimals"
	rebalancerMethodPositionManager   = "positionManager"
	rebalancerMethodManagedVault      = "managedVault"
	rebalancerMethodMathLibrary       = "mathLibrary"
	almMethodRvpsWad                  = "reservationValuePerShareWad"
	cvammMethodReservationPriceWad    = "reservationPriceWad"
	cvammMethodIdleStable             = "idleStable"
	cvammMethodIdleVolatile           = "idleVolatile"
	boMethodCore                      = "CORE"
	coreMethodCcr                     = "CCR"
	pmMethodEntireSystemBalances      = "getEntireSystemBalances"
	allowlistMethodIsDepositAllowed   = "isDepositAllowed"
	almMethodMintAllowlist            = "mintAllowlist"
	debtMethodFlashFee                = "flashFee"
	coreMethodPaused                  = "paused"
	coreMethodIsPeriphery             = "isPeriphery"
	pmMethodPaused                    = "paused"
	pmMethodSunsetting                = "sunsetting"
	pmMethodMaxSystemDebt             = "maxSystemDebt"
	pmMethodDefaultedDebt             = "defaultedDebt"
	pmMethodTotalActiveDebt           = "getTotalActiveDebt"
	pmMethodBorrowingRate             = "getBorrowingRateWithDecay"
	boMethodIsApprovedDelegate        = "isApprovedDelegate"
	swapperMethodDebtToken            = "debtToken"
	swapperMethodVolatile             = "volatile"
	swapperMethodCollVault            = "collVault"
	swapperMethodCore                 = "core"
	almMethodPaused                   = "paused"
	pmMethodBorrowerOperations        = "borrowerOperations"
	pmMethodDebtGasCompensation       = "DEBT_GAS_COMPENSATION"
	boMethodMinNetDebt                = "minNetDebt"
	pmMethodInterestRate              = "interestRate"
	pmMethodMcr                       = "MCR"
	pmMethodFetchPrice                = "fetchPrice"
	pmMethodActiveInterestIndex       = "activeInterestIndex"
	pmMethodLastActiveIndexUpdate     = "lastActiveIndexUpdate"
	pmMethodPositions                 = "Positions"
	pmMethodPendingRewards            = "getPendingCollAndDebtRewards"

	// Per-direction gas for the ADAPTER path, which is what a route actually pays: the
	// adapter's share bisection and gross re-derivation run under DELEGATECALL on top of
	// the venue call. Fork measurements on the deployed stack, worst case per direction:
	// leverage 4,849,423 (hint-free, the deeper bracket) and deleverage 4,620,338 — also
	// hint-free, which seeds at the budget and needs the most steps. A hinted deleverage
	// is 3.72M and a stale-hint recovery 2.63M, but the default has to cover the
	// hint-free fallback because the adapter accepts zero hints.
	//
	// The defaults carry ~25% over those: a fork replays warm storage while a production
	// fill pays cold SLOAD across the rebalancer, CollVault, ALM and the CDP, and the
	// bisection depth moves with the position's scale. Undershooting risks a fill that
	// runs out of gas, but overshooting costs route ranking, so the margin covers the
	// measured variance rather than the chain's flat send limit. Overridable per
	// deployment via Config.
	defaultGasLeverage   int64 = 6_000_000
	defaultGasDeleverage int64 = 5_800_000
)

var (
	ErrInvalidToken        = errors.New("invalid token")
	ErrInvalidAmountIn     = errors.New("invalid amount in")
	ErrNotPriceable        = errors.New("vault state is not locally priceable (degenerate region)")
	ErrZeroAmountOut       = errors.New("zero amount out")
	ErrSwapRejected        = errors.New("the vault rejects this fill")
	ErrLeverageDisabled    = errors.New("leverage is disabled: the CDP has borrow interest enabled")
	ErrNoCurveParams       = errors.New("no curve params for this chain — the deployed rebalancer's constants are required")
	ErrInvalidCurveParams  = errors.New("invalid curve params")
	ErrUnderlyingCvamm     = errors.New("adapter alm() did not resolve the underlying CvammALM")
	ErrMissingBasePool     = errors.New("underlying everlong-cvamm base pool absent — refusing an uncoupled simulator")
	ErrMathNotConfigured   = errors.New("config.Math is unset, not an address, or does not answer deleverageQuote")
	ErrInvalidSnapshotWord = errors.New("invalid snapshot value")
	ErrVenueGateClosed     = errors.New("the venue would revert this direction")
	ErrSwapperIdentity     = errors.New("the settlement swapper does not belong to the configured rebalancer, or its pair disagrees with config")
	ErrMathMismatch        = errors.New("the deployed CollRebalancerMath disagrees with the local model")
	ErrMathNotLinked       = errors.New("the rebalancer implementation does not link the configured CollRebalancerMath")
	ErrGateDiscovery       = errors.New("a gate pointer the verified deployment exposes did not resolve")
	ErrFlashFeeNotExempt   = errors.New("the swapper is not flash-fee exempt on the debt token: every fill would revert NonZeroFlashFee")
)

// berachainCurveParams are the constants frozen into the deployed Berachain
// CollateralRebalancer + linked CollRebalancerMath (the 155%-wall "champion-v3"
// construction; read from the deployment source, parity-validated against the shipped
// library bytecode). Per-deployment values — a contract upgrade or another chain's
// deployment can differ, and Config.CurveParams overrides these.
func berachainCurveParams() CurveParams {
	wad := func(s string) *big.Int {
		v, _ := new(big.Int).SetString(s, 10)
		return v
	}
	return CurveParams{
		LeverageRatioWad: wad("444444444444444444"),
		HZero:            wad("562500000000000000"),
		HJoin:            wad("1010000000000000000"),
		HWall:            wad("1882448291726770582"),
		Width:            wad("872448291726770582"),
		DJoin:            wad("509975124224178054"),
		DWall:            wad("1214482768855981020"),
		RescueSpreadPpm:  big.NewInt(13_000),
		BezierPhi: [4]*big.Int{
			wad("995037190209989135"), wad("851783312849706840"),
			wad("738044106433170508"), wad("645161290322580645"),
		},
		BezierIntegral: [5]*big.Int{
			big.NewInt(0), wad("248759297552497283"), wad("461705125764923993"),
			wad("646216152373216620"), wad("807506474953861782"),
		},
		PhysicalCrFloorWad: wad("1820000000000000000"),
	}
}

// curveParamsByChain carries the per-deployment curve constants; Config.CurveParams
// overrides them, and chains without an entry must supply the override.
var curveParamsByChain = map[valueobject.ChainID]func() CurveParams{
	valueobject.ChainIDBerachain: berachainCurveParams,
}
