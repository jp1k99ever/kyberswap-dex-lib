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
// Berachain venue and rounded up for headroom. V1 impl: volatile-in 163,422-163,989,
// stable-in 209,769-297,333 across small and near-capacity fills. The V2 impl
// (FFAD fee hook, 2026-08-13) adds the hook's fee recompute to every fill —
// re-measured live post-upgrade: volatile-in 179,216-181,525, stable-in
// 218,592-218,920 on session-sized fills (the stable-in bisection still scales with
// size, so its V1 near-capacity ceiling keeps the 310k headroom). The two legs differ
// structurally — volatile-in is closed form (one curve solve) while stable-in runs a
// seeded bisection under DELEGATECALL. Overridable per deployment via Config/StaticExtra.
const (
	defaultGasStableIn   int64 = 310000
	defaultGasVolatileIn int64 = 200000
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
