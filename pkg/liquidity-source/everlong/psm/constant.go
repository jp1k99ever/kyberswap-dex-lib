package everlongpsm

import (
	"errors"
)

const (
	DexType = "everlong-psm"

	psmMethodDebtToken        = "debtToken"
	psmMethodMetaCore         = "metaCore"
	psmMethodPaused           = "paused"
	psmMethodCapHook          = "capHook"
	psmMethodStables          = "stables"
	psmMethodFeeBpFor         = "feeBpFor"
	psmMethodAvailableMint    = "availableMint"
	psmMethodAvailableReserve = "availableReserve"
	psmMethodDebtTokenMinted  = "debtTokenMinted"
	capHookMethodMaxRedeem    = "maxRedeem"
	capHookMethodMaxOutflow   = "getMaxPsmOutflow"

	// Adapter-path fork measurements on the deployed venue top out at 192,050 across both
	// directions, so these keep headroom; overridable per deployment via Config.
	defaultGasDeposit int64 = 260_000
	defaultGasRedeem  int64 = 220_000
)

var (
	ErrInvalidToken    = errors.New("invalid token")
	ErrInvalidAmountIn = errors.New("invalid amount in")
	ErrPaused          = errors.New("psm is paused")
	ErrNotListed       = errors.New("stable is not whitelisted on the psm")
	ErrZeroAmountOut   = errors.New("zero amount out")
	ErrCapExhausted    = errors.New("mint cap leaves no room")
	ErrNothingToRedeem = errors.New("nothing minted against this stable / no reserve")
	ErrFeeUnavailable  = errors.New("the psm refuses to price this direction")
	ErrInvalidSnapshot = errors.New("invalid snapshot value")
)
