package everlongpsm

import (
	"errors"
)

const (
	DexType = "everlong-psm"

	psmMethodDebtToken       = "debtToken"
	psmMethodFeeHook         = "feeHook"
	psmMethodPaused          = "paused"
	psmMethodStables         = "stables"
	psmMethodMintCap         = "mintCap"
	psmMethodDebtTokenMinted = "debtTokenMinted"
	erc20MethodBalanceOf     = "balanceOf"
	feeHookMethodCalcFee     = "calcFee"

	// IFeeHook.Action values (enum order: DEPOSIT, MINT, WITHDRAW, REDEEM).
	actionDeposit uint8 = 0
	actionRedeem  uint8 = 3

	// Estimated from the call shape (two mints + transferFrom / burn + two transfers);
	// override per deployment via Config once measured on real fills.
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
	ErrInvalidSnapshot = errors.New("invalid snapshot value")
)
