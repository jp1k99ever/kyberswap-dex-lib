package everlongcvamm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// StaticExtra is the immutable per-venue metadata written at listing time. The pool
// address IS the ALM address, so only the optional periphery and gas overrides live here.
type StaticExtra struct {
	// FeeHook resolved at listing. The fee-law terms live on it, and pinning it here is
	// what lets them be read in the SAME round as everything else — a second round could
	// not run on the batched path, which left that path without a fee law at all. The
	// refresh re-reads feeHook() and skips the terms if it has moved.
	FeeHook       string `json:"feeHook,omitempty"`
	Adapter       string `json:"adapter,omitempty"`
	GasStableIn   int64  `json:"gasSIn,omitempty"`
	GasVolatileIn int64  `json:"gasVIn,omitempty"`
}

// Support is the funded band from getSupport(): anchor-free, invalidated only by a
// CurveRetuned (tracked implicitly — every refresh re-reads it). xLo is the high-price
// edge (volatile leg exactly zero there), xHi the low-price edge (stable leg exactly
// zero), yHi = y(xHi), the stable offset.
type Support struct {
	AWad *uint256.Int `json:"a"`
	XLo  *uint256.Int `json:"xLo"`
	XHi  *uint256.Int `json:"xHi"`
	YHi  *uint256.Int `json:"yHi"`
}

// Extra is the per-refresh venue state — everything a quote needs, all read from the
// ALM pinned at one block. XWad is the AUTHORITATIVE inventory coordinate (the price is
// a lossy floored sqrt of it and is deliberately not stored). Reserves are the accounted
// tradeable reserves (idle excluded) — authoritative for the solvency clamp. The
// directional fees are READ, never computed: their realized-variance input has no getter
// and no event on-chain.
type Extra struct {
	Support          Support      `json:"sup"`
	XWad             *uint256.Int `json:"x"`
	AnchorSqrtX96    *uint256.Int `json:"anchor"`
	Kappa            *uint256.Int `json:"kappa"`
	FeeStableInWad   *uint256.Int `json:"feeS"`
	FeeVolatileInWad *uint256.Int `json:"feeV"`
	Paused           bool         `json:"paused,omitempty"`

	// Fee-law terms needed to re-derive the fee after a fill — see fee_law_exact.go for
	// the law and fee_law.go for the saturated bound behind it. nil disables both and
	// leaves the conservative fold.
	MidFeeWad           *uint256.Int `json:"midFee,omitempty"`
	DirSkewWad          *uint256.Int `json:"dirSkew,omitempty"`
	InvSkewKappaWad     *uint256.Int `json:"invK,omitempty"`
	InvSkewBandWad      *uint256.Int `json:"invBand,omitempty"`
	ReservationPriceWad *uint256.Int `json:"resvP,omitempty"`
	// Hook floor at the LIVE push rate (ffadState().rateWad run through the hook's own
	// smoothstep, off its public FFAD constants), which makes max(law, floor) exact. When
	// those constants do not decode (another hook build) this falls back to the floor at
	// a SATURATED push rate — an upper bound on the live floor, so a re-derived fee below
	// it declines rather than over-quotes.
	FloorStableInWad   *uint256.Int `json:"floorS,omitempty"`
	FloorVolatileInWad *uint256.Int `json:"floorV,omitempty"`
	// Zero curvature selects the law's scalar branch, where the fee is LpFee regardless
	// of the multiplier.
	CurvatureWad *uint256.Int `json:"curv,omitempty"`
	LpFeeWad     *uint256.Int `json:"lpFee,omitempty"`
	// The remaining terms of the exact law. `rv` is NOT among them — it has no getter, and
	// it reaches the fee only through a scalar a swap cannot move, so it is solved from the
	// sampled fee rather than read.
	OutFeeWad      *uint256.Int `json:"outFee,omitempty"`
	VolSigmaRefWad *uint256.Int `json:"sigmaRef,omitempty"`
	VolBetaWad     *uint256.Int `json:"volBeta,omitempty"`
	VolMinWad      *uint256.Int `json:"volMin,omitempty"`
	VolMaxWad      *uint256.Int `json:"volMax,omitempty"`
}

// supportRaw is the ethrpc decode target for getSupport() (tuple field order = ABI order).
// ffadStateRaw decodes ffadState(): only the push rate prices anything.
type ffadStateRaw struct {
	RateWad     *big.Int
	ObservedAt  *big.Int
	AssetOracle common.Address
	RoundId     *big.Int
}

type supportRaw struct {
	AWad *big.Int
	XLo  *big.Int
	XHi  *big.Int
	YHi  *big.Int
}

// PoolMeta carries what the executor needs to build the direct
// CvammALM.swap(stableIn, amountIn, minAmountOut, sqrtPriceLimitX96, to, deadline) call:
// stableIn = (tokenIn == token0), sqrtPriceLimitX96 = 0 (no limit), and the ALM pulls the
// input under an allowance. Exact-input only; partial fills are NORMAL — read
// amountInUsed. Adapter (if set) is an exactInputSingle shim that fails
// closed on partial fills.
type PoolMeta struct {
	ALM     string `json:"alm"`
	Adapter string `json:"adapter,omitempty"`
	// The shared executor metadata (pool.MetaInfo): the approval target and the block
	// the snapshot was read at.
	ApprovalAddress string `json:"approvalAddress"`
	BlockNumber     uint64 `json:"blockNumber"`
}

// SwapInfo carries the post-fill state from CalcAmountOut to UpdateBalance so the
// transition is never recomputed. GrossOut is the output-leg reserve decrease (net out +
// fee: the fee leaves the priced book into idle).
type SwapInfo struct {
	XAfter       *uint256.Int `json:"x"`
	AmountInUsed *uint256.Int `json:"in"`
	GrossOut     *uint256.Int `json:"out"`
	FeeOut       *uint256.Int `json:"fee,omitempty"` // output-side fee: leaves accounted reserves into ALM idle
	StableIn     bool         `json:"sIn"`
}
