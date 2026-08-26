package everlongrebalancer

import (
	"math/big"
)

type StaticExtra struct {
	// Resolved on-chain at listing time from the configured rebalancer.
	Rebalancer string `json:"reb"`
	Swapper    string `json:"swapper"` // CollateralRebalancerSwapper — swap call + approval target
	CollVault  string `json:"cv"`
	ALM        string `json:"alm"` // the swapper's ALM adapter (reserves/valuation reads)
	// ALMAdapterCodeHash attests the exact non-proxy wrapper implementation whose
	// reservationValuePerShareWad rounding the simulator mirrors. The swapper's ALM
	// pointer is immutable; a new swapper is relisted and re-attested.
	ALMAdapterCodeHash string `json:"almCodeHash,omitempty"`
	// The CvammALM the adapter wraps (resolved from its alm() getter at listing) — the
	// everlong-cvamm base pool for meta coupling.
	UnderlyingCvamm string `json:"ucv,omitempty"`
	// 18 - collVault.assetDecimals() (constant per deployment).
	CvDecimalsOffset uint8 `json:"cdo"`
	// Deployed CollRebalancerMath (config): the executor's exact deleverage re-derivation.
	Math string `json:"math,omitempty"`
	// PositionManager / BorrowerOperations of the CDP the position lives in, and the
	// immutable per-position gas compensation. Resolved on-chain at listing; needed for
	// the minimum-net-debt bound on deleverage (see VaultState.MinNetDebt).
	PositionManager    string `json:"pm,omitempty"`
	BorrowerOperations string `json:"bo,omitempty"`
	// Core is BorrowerOperations.CORE(): the protocol core whose CCR gates every CDP
	// adjustment (recovery mode, and the TCR floor in normal mode). Immutable on BO.
	Core string `json:"core,omitempty"`
	// MintAllowlist gates the ALM adapter's buyShares; the swapper must stay on it for
	// leverage to settle. Immutable on the adapter.
	MintAllowlist string `json:"mintAllowlist,omitempty"`
	// Implementation is the rebalancer proxy's EIP-1967 implementation at listing. The
	// CollRebalancerMath the executor re-derives against is LINKED into that code, so a
	// new implementation is the one way the math can change under a listed pool: the
	// tracker re-reads the slot each refresh and blocks both directions on a change.
	Implementation string `json:"impl,omitempty"`
	// StableToken is the debt token the swapper flash-mints (derived from the swapper
	// at listing); the tracker's from-scoped flash-fee probe is asked of it.
	StableToken         string   `json:"stableToken,omitempty"`
	DebtGasCompensation *big.Int `json:"gasComp,omitempty"`
	// The rebalancer's managed CDP vault — the Positions/pending-rewards key for the
	// interest-drift venue and the ICR context. Resolved on-chain at listing.
	ManagedVault string `json:"mv,omitempty"`
	// The deployed rebalancer's frozen curve constants.
	CurveParams CurveParams `json:"curve"`
	// GasLeverage / GasDeleverage override the measured defaults when non-zero.
	GasLeverage   int64 `json:"gasLev,omitempty"`
	GasDeleverage int64 `json:"gasDlv,omitempty"`
}

// Extra is the per-refresh vault snapshot the simulator prices from.
type Extra = VaultState

// SwapInfo carries the exact fill so UpdateBalance replays it without recomputation,
// and the executor learns the exact contract args.
//
// Leverage (volatile -> stable): the executor calls
// swapVolatileForStable(CollVaultShares, maxStableIn, maxVolatileIn, minNetStableOut, receiver)
// paying VolatileLeg wei of the volatile token; the flash-minted stable leg nets out and
// the caller receives the quoted net stable.
//
// Deleverage (stable -> volatile): the executor calls
// swapStableForVolatile(GrossStableIn, maxNetStableIn, minVolatileOut, receiver).
// NOTE the swapper transferFroms maxNetStableIn UP FRONT and refunds the excess within
// the same call — the payer must hold and approve maxNetStableIn (>= the true net; a
// snug cap risks an on-chain revert on state drift), even though only the net is spent.
type SwapInfo struct {
	IsLeverage bool `json:"lev"`
	// CollVaultShares: shares minted (leverage) or burned (deleverage).
	CollVaultShares *big.Int `json:"shares"`
	// GrossStableIn: deleverage only — the stableDebtIn contract argument.
	GrossStableIn *big.Int `json:"grossIn,omitempty"`
	// StableLeg / VolatileLeg: token amounts entering (leverage) or leaving
	// (deleverage) the ALM.
	StableLeg   *big.Int `json:"stableLeg"`
	VolatileLeg *big.Int `json:"volatileLeg"`
	// AlmShares: ALM shares minted/burned by the CollVault for this fill (preview values —
	// the executor-facing amounts).
	AlmShares *big.Int `json:"almShares"`
	// Post-fill position state.
	NewCollateral *big.Int `json:"newC"`
	NewDebt       *big.Int `json:"newD"`
	// Exact post-fill CollVault words and reservation value, computed at quote time so
	// UpdateBalance assigns instead of recomputing. On deleverage these already account
	// for the fee shares being re-minted (only NET shares burn) and the raw-ratio asset
	// release; AlmBurned is that raw-ratio ALM share amount (differs from AlmShares by
	// bounded preview-vs-actual rounding).
	// AlmMint is the ALM-side shape of a leverage fill (deposit, then the swapper's
	// sell-back of the shares the CollVault did not need), so a coupled base pool can
	// replay the venue's own two steps instead of one pro-rata scalar.
	AlmMintedShares  *big.Int `json:"almMinted,omitempty"`
	AlmUsedStable    *big.Int `json:"almUsedS,omitempty"`
	AlmUsedVolatile  *big.Int `json:"almUsedV,omitempty"`
	AlmSoldBackShare *big.Int `json:"almSoldBack,omitempty"`
	// FlashStableCap is the maxStableIn the executor passes on leverage: the swapper's
	// preview of the stable leg (rounded up), which the ALM's deposit sizes the mint by.
	FlashStableCap *big.Int `json:"flashCap,omitempty"`
	// IdleStableDelta / IdleVolatileDelta: the signed move of the ALM's idle words (the
	// deposit parks a pro-rata part of the legs there; a redeem releases it).
	IdleStableDelta   *big.Int `json:"dIdleS,omitempty"`
	IdleVolatileDelta *big.Int `json:"dIdleV,omitempty"`
	PostCvTotalAssets *big.Int `json:"postCta,omitempty"`
	PostCvTotalSupply *big.Int `json:"postCts,omitempty"`
	// PostRvpsWad is the legacy ClammAlmAdapter reservationValuePerShareWad after the
	// exact underlying deposit/withdraw floors. It is not invariant under a pro-rata
	// liquidity move because kappa, idle and supply are floored independently.
	PostRvpsWad  *big.Int `json:"postRvps,omitempty"`
	PostPriceWad *big.Int `json:"postR,omitempty"`
	AlmBurned    *big.Int `json:"almBurned,omitempty"`
}

// exchangeStateRaw / totalAmountsRaw / reservesAtReferenceRaw are ethrpc decode
// targets (field order = ABI output order).
type exchangeStateRaw struct {
	Collateral *big.Int
	Debt       *big.Int
	PriceWad   *big.Int
	SpreadPpm  *big.Int
}

type totalAmountsRaw struct {
	StableReserve   *big.Int
	VolatileReserve *big.Int
}

type systemBalancesRaw struct {
	Coll  *big.Int
	Debt  *big.Int
	Price *big.Int
}

type reservesAtReferenceRaw struct {
	StableReserve   *big.Int
	AssetReserve    *big.Int
	RawReferenceWad *big.Int
}

// positionsRaw decodes PositionManager.Positions(managedVault) — the raw (unprojected)
// stored debt and the position's interest index snapshot.
type positionsRaw struct {
	Debt                *big.Int
	Coll                *big.Int
	Stake               *big.Int
	Status              uint8
	ArrayIndex          *big.Int
	ActiveInterestIndex *big.Int
}

// pendingRewardsRaw decodes getPendingCollAndDebtRewards(managedVault): (coll, debt).
type pendingRewardsRaw struct {
	CollReward *big.Int
	DebtReward *big.Int
}

// PoolMeta tells the executor where and how to settle.
type PoolMeta struct {
	Swapper    string `json:"swapper"`
	Rebalancer string `json:"rebalancer"`
	// Math / LeverageRatioWad let the executor re-derive the exact deleverage gross when
	// the runtime amount falls below the quoted net (gross<->net is not linear).
	Math             string   `json:"math,omitempty"`
	LeverageRatioWad *big.Int `json:"rWad,omitempty"`
	// UpfrontNetStablePull: deleverage pulls maxNetStableIn up front (see SwapInfo).
	UpfrontNetStablePull bool `json:"upfrontNetStablePull"`
	// The shared executor metadata (pool.MetaInfo): the approval target and the block
	// the snapshot was read at.
	ApprovalAddress string `json:"approvalAddress"`
	BlockNumber     uint64 `json:"blockNumber"`
}

// Stable is the debt token address, or "" on a StaticExtra persisted before it was
// derived from the swapper.
func (se *StaticExtra) Stable() string { return se.StableToken }
