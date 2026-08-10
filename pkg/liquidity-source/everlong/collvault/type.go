package everlongcollvault

import (
	"math/big"
)

type StaticExtra struct {
	// Resolved on-chain at listing time from the configured rebalancer.
	Rebalancer string `json:"reb"`
	Swapper    string `json:"swapper"` // CollateralRebalancerSwapper — swap call + approval target
	CollVault  string `json:"cv"`
	ALM        string `json:"alm"` // the swapper's ALM adapter (reserves/valuation reads)
	// 18 - collVault.assetDecimals() (constant per deployment).
	CvDecimalsOffset uint8 `json:"cdo"`
	// PositionManager / BorrowerOperations of the CDP the position lives in, and the
	// immutable per-position gas compensation. Resolved on-chain at listing; needed for
	// the minimum-net-debt bound on deleverage (see VaultState.MinNetDebt).
	PositionManager     string   `json:"pm,omitempty"`
	BorrowerOperations  string   `json:"bo,omitempty"`
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
	PostCvTotalAssets *big.Int `json:"postCta,omitempty"`
	PostCvTotalSupply *big.Int `json:"postCts,omitempty"`
	PostPriceWad      *big.Int `json:"postR,omitempty"`
	AlmBurned         *big.Int `json:"almBurned,omitempty"`
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
	// UpfrontNetStablePull: deleverage pulls maxNetStableIn up front (see SwapInfo).
	UpfrontNetStablePull bool `json:"upfrontNetStablePull"`
}
