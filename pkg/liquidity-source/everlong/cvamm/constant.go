package everlongcvamm

import (
	"errors"
)

const (
	DexType = "everlong-cvamm"

	almMethodToken0             = "token0"
	almMethodToken1             = "token1"
	almMethodGetSupport         = "getSupport"
	almMethodXWad               = "xWad"
	almMethodAnchorSqrtCurveX96 = "anchorSqrtCurveX96"
	almMethodKappa              = "kappa"
	almMethodReserveStable      = "reserveStable"
	almMethodReserveVolatile    = "reserveVolatile"
	almMethodPoolFeeDirectional = "poolFeeDirectional"
	almMethodPaused             = "paused"
	almMethodFeeHook            = "feeHook"
	almMethodReservationPrice   = "reservationPriceWad"
	almMethodFfadState          = "ffadState"
	hookMethodMidFee            = "inventoryBalancedFeeWad"
	hookMethodDirSkew           = "dirSkewWad"
	hookMethodInvSkewKappa      = "invSkewKappaWad"
	hookMethodInvSkewBand       = "invSkewBandWad"
	hookMethodHotFeeFloor       = "hotFeeFloorWad"
	hookMethodCurvature         = "inventoryFeeCurvatureWad"
	hookMethodLpFee             = "lpFeeWad"
	hookMethodOutFee            = "inventoryImbalancedFeeWad"
	hookMethodVolSigmaRef       = "volSigmaRefWad"
	hookMethodVolBeta           = "volBetaWad"
	hookMethodVolMin            = "volMinWad"
	hookMethodVolMax            = "volMaxWad"
	hookMethodFfadEnabled       = "ffadEnabled"
	hookMethodFfadRefRate       = "FFAD_REFERENCE_RATE_WAD"
	hookMethodFfadSatRate       = "FFAD_SATURATION_RATE_WAD"
	hookMethodFfadStableFloor   = "FFAD_STABLE_IN_FLOOR_WAD"
	hookMethodFfadVolatileFloor = "FFAD_VOLATILE_IN_FLOOR_WAD"
)

// Default gas per direction, MEASURED THROUGH THE ADAPTER against the deployed
// Berachain venue (ks-dex-adapter-lib fork tests), which is the path a route actually
// pays for — direct-venue numbers understate it. Worst case per direction across the
// settled replays and the oversized partial fills: stable-in 321,747 (its bisection
// scales with size, and the partial path is the ceiling), volatile-in 239,082 (closed
// form, and its partial path is cheaper at 193,104). The defaults carry ~25% over those:
// a fork replays warm storage while a production fill pays cold SLOAD across the ALM and
// its fee hook. Overridable per deployment via Config/StaticExtra.
const (
	defaultGasStableIn   int64 = 400000
	defaultGasVolatileIn int64 = 300000
)

var (
	ErrExactOutNotSupported = errors.New("exact-output swaps are not supported (the fee is an output haircut; the curve solves forward only)")
	ErrInvalidToken         = errors.New("invalid token")
	ErrPaused               = errors.New("venue is paused")
	ErrRetractedBook        = errors.New("book is retracted (kappa == 0) or has no anchor")
	ErrCurveDomain          = errors.New("coordinate outside the curve domain")
	ErrCurveAmplification   = errors.New("amplification outside (WAD/2, 1000*WAD]")
	ErrSwapExhausted        = errors.New("nothing fills (input below normalized resolution or support exhausted)")
	ErrZeroAmountOut        = errors.New("zero amount out")
	ErrInvalidFee           = errors.New("directional fee at or above 100%")
	ErrOverflow             = errors.New("amount overflow")
)
