package everlongcvamm

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// StaticExtra is the immutable per-venue metadata written at listing time. The pool
// address IS the ALM address, so only the optional periphery and gas overrides live here.
type StaticExtra struct {
	// ProfileVersion and ConfigHash make persisted pools self-identifying. The simulator
	// revalidates this profile on every quote because msgpack restores the concrete object
	// without calling NewPoolSimulator; an old/detached cache entry must not pair arbitrary
	// advertised tokens with calls to the real ALM.
	ProfileVersion uint64              `json:"profileVersion"`
	DexID          string              `json:"dexId"`
	ChainID        valueobject.ChainID `json:"chainId"`
	ALM            string              `json:"alm"`
	Token0         string              `json:"token0"`
	Token1         string              `json:"token1"`
	ConfigHash     string              `json:"configHash"`
	ProfileHash    string              `json:"profileHash"`
	// FeeHook resolved at listing. The fee-law terms live on it, and pinning it here is
	// what lets them be read in the SAME round as everything else — a second round could
	// not run on the batched path, which left that path without a fee law at all. The
	// refresh re-reads feeHook() and skips the terms if it has moved.
	FeeHook string `json:"feeHook,omitempty"`
	// Implementation pins the EIP-1967 implementation whose storage layout and fee
	// law this integration mirrors. The tracker re-reads the slot at the snapshot block
	// before it consumes the layout-bound realized-variance word.
	Implementation         string `json:"impl,omitempty"`
	ImplementationCodeHash string `json:"implCodeHash,omitempty"`
	Adapter                string `json:"adapter,omitempty"`
	GasStableIn            int64  `json:"gasSIn,omitempty"`
	GasVolatileIn          int64  `json:"gasVIn,omitempty"`
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
// directional fees are read for the first quote. RealizedVarianceWad is read directly
// from the implementation-pinned ERC-7201 slot so subsequent quotes can reconstruct
// the fee exactly; it is deliberately absent for state-override snapshots.
type Extra struct {
	Support          Support      `json:"sup"`
	XWad             *uint256.Int `json:"x"`
	AnchorSqrtX96    *uint256.Int `json:"anchor"`
	Kappa            *uint256.Int `json:"kappa"`
	FeeStableInWad   *uint256.Int `json:"feeS"`
	FeeVolatileInWad *uint256.Int `json:"feeV"`
	FeeHookActive    bool         `json:"dynamicFee,omitempty"`
	Paused           bool         `json:"paused,omitempty"`
	// The ALM's idle balances: the part of getTotalAmounts held off the curve. Deposits
	// take a pro-rata slice of them and withdrawals release one, each floored on its own,
	// which is what makes a coupled mint/burn reproducible to the wei. nil disables
	// reverse coupling because the bucket split cannot be inferred exactly.
	IdleStable   *uint256.Int `json:"idleS,omitempty"`
	IdleVolatile *uint256.Int `json:"idleV,omitempty"`

	// Fee-law terms needed to re-derive the fee after a fill. Missing terms disable a
	// chained quote; there is no inferred-scalar or conservative-bound fallback.
	MidFeeWad           *uint256.Int `json:"midFee,omitempty"`
	DirSkewWad          *uint256.Int `json:"dirSkew,omitempty"`
	InvSkewKappaWad     *uint256.Int `json:"invK,omitempty"`
	InvSkewBandWad      *uint256.Int `json:"invBand,omitempty"`
	ReservationPriceWad *uint256.Int `json:"resvP,omitempty"`
	// Hook floor at the LIVE push rate (ffadState().rateWad run through the hook's public
	// FFAD constants). HotFloorsExact distinguishes these exact words from the saturated
	// probes retained only for snapshot diagnostics.
	FloorStableInWad   *uint256.Int `json:"floorS,omitempty"`
	FloorVolatileInWad *uint256.Int `json:"floorV,omitempty"`
	HotFloorsExact     bool         `json:"floorExact,omitempty"`
	// Zero curvature selects the law's scalar branch, where the fee is LpFee regardless
	// of the multiplier.
	CurvatureWad *uint256.Int `json:"curv,omitempty"`
	LpFeeWad     *uint256.Int `json:"lpFee,omitempty"`
	// The remaining terms of the exact law. RealizedVarianceWad is the layout-bound
	// CvammStore.rv word read at the same block as this snapshot.
	OutFeeWad           *uint256.Int `json:"outFee,omitempty"`
	VolSigmaRefWad      *uint256.Int `json:"sigmaRef,omitempty"`
	VolBetaWad          *uint256.Int `json:"volBeta,omitempty"`
	VolMinWad           *uint256.Int `json:"volMin,omitempty"`
	VolMaxWad           *uint256.Int `json:"volMax,omitempty"`
	RealizedVarianceWad *uint256.Int `json:"rv,omitempty"`
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
