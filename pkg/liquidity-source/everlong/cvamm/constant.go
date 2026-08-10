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
)

// Default gas per direction, MEASURED on real settled fills against the deployed
// Berachain venue and rounded up for headroom: volatile-in 163,422-163,989 and
// stable-in 209,769-297,333 across small and near-capacity fills. The two legs differ
// structurally — volatile-in is closed form (one curve solve) while stable-in runs a
// seeded bisection under DELEGATECALL, which is also why only stable-in varies with
// size. Overridable per deployment via Config/StaticExtra.
const (
	defaultGasStableIn   int64 = 310000
	defaultGasVolatileIn int64 = 170000
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
	ErrOverflow             = errors.New("amount overflow")
)
