package uniswapv3

import (
	"math/big"

	"github.com/pkg/errors"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

const (
	DexTypeUniswapV3 = "uniswapv3"

	graphFirstLimit      = 1000
	defaultTokenDecimals = 18
	rpcChunkSize         = 100
	tickChunkSize        = 100
)

const (
	methodGetLiquidity = "liquidity"
	methodGetSlot0     = "slot0"
	methodTickSpacing  = "tickSpacing"
	methodTicks        = "ticks"
)

// CrossEmptyWordGas prices one swap-loop iteration that walks a tick-bitmap word without crossing
// an initialized tick: a cold SLOAD of the word (2100 under EIP-2929) plus the getSqrtRatioAtTick
// and computeSwapStep the step runs either way.
//
// It is an estimate, not a calibrated measurement, but it replaces an estimate of zero: the old
// model charged nothing for walking words, so a swap through a pool thin enough to spend its whole
// input on per-word rounding was priced as though it crossed no ticks and did no work. Ekubo's
// independently derived GasTickSpacingCrossed is 2507 for the same operation, which is the closest
// cross-check available in this repository.
const CrossEmptyWordGas = 2500

var (
	zeroBI     = big.NewInt(0)
	defaultGas = Gas{BaseGas: 109334, CrossInitTickGas: 21492, CrossEmptyWordGas: CrossEmptyWordGas}

	ErrOverflow            = errors.New("bigInt overflow int/uint256")
	ErrInvalidFeeTier      = errors.New("invalid feeTier")
	ErrTickNil             = errors.WithMessage(pool.ErrUnsupported, "tick is nil")
	ErrV3TicksEmpty        = errors.WithMessage(pool.ErrUnsupported, "v3Ticks empty")
	ErrInvalidToken        = errors.New("invalid token")
	ErrZeroAmount          = errors.New("zero amount")
	ErrInsufficientBalance = errors.New("insufficient balance")
	ErrBuyRestricted       = errors.New("token buy restricted")
)
