package everlongpsm

import (
	"math/big"
)

// StaticExtra is immutable per pool (one pool per whitelisted stable).
type StaticExtra struct {
	PSM string `json:"psm"`
	// MetaCore is the protocol core whose paused() the PSM ORs into its own on every
	// swap (`notPaused` checks `paused || metaCore.paused()`). Immutable on the PSM (set
	// in the constructor, no setter), so it is resolved once at listing.
	MetaCore string `json:"metaCore,omitempty"`
	// WadOffset = 10^(debtToken.decimals - stable.decimals); frozen at whitelisting
	// (re-whitelisting after a decimals change would re-derive it, which also re-lists
	// the pool).
	WadOffset *big.Int `json:"wadOffset"`
	// GasDeposit / GasRedeem override the estimated defaults when non-zero.
	GasDeposit int64 `json:"gasDep,omitempty"`
	GasRedeem  int64 `json:"gasRed,omitempty"`
}

// Extra is the per-refresh snapshot the simulator prices from.
type Extra struct {
	Paused bool `json:"paused"`
	// feeBpFor for the configured caller: the FINISHED rate, not a hook parameter. nil
	// when the PSM refuses to price that direction. Sampled pre-trade like every PSM
	// entry point, so a second fill in one route reuses a rate the chain would re-read.
	EntryFeeBp *big.Int `json:"entryBp,omitempty"`
	ExitFeeBp  *big.Int `json:"exitBp,omitempty"`
	// Mint room after BOTH the structural mintCap and the cap hook.
	AvailableMint *big.Int `json:"availMint"`
	// Per-stable book; the redeem burn cannot exceed it (the chain underflows past it).
	DebtTokenMinted *big.Int `json:"minted"`
	// Cap hook's single-burn ceiling; nil when no cap hook is set.
	MaxRedeem *big.Int `json:"maxRedeem,omitempty"`
	// Cap hook's per-caller ceiling on the stable leaving (output + fee), for the
	// configured FeeCaller. Independent of MaxRedeem and reverts the same way. The
	// adapter re-reads it for ITSELF at execution, so this only has to be right enough
	// to route; see Config.FeeCaller.
	MaxOutflow *big.Int `json:"maxOutflow,omitempty"`
	// Stable payable right now: idle balance plus recallable yield-hook float.
	AvailableReserve *big.Int `json:"reserve"`
}

// A cap hook can also refuse a fill outright through onPSMDeposit/onPSMWithdraw, which
// mutate and whose REVERT is the enforcement — no view exposes them, so no quote can
// predict them. Such a fill fails at execution rather than partial-filling.

// SwapInfo carries the exact fill so UpdateBalance replays it without recomputation.
type SwapInfo struct {
	IsDeposit bool `json:"dep"`
	// GrossDebt: deposit — debt minted incl. fee (what the cap consumes);
	// redeem — the debt burned (the full input).
	GrossDebt *big.Int `json:"grossDebt"`
	// GrossStable: deposit — the stable pulled in; redeem — the stable leaving the PSM
	// (output + stable fee).
	GrossStable *big.Int `json:"grossStable"`
}

// PoolMeta tells the executor where to settle; the PSM is both the swap entrypoint and
// the deposit-side approval target (redeem burns straight from the caller).
type PoolMeta struct {
	PSM string `json:"psm"`
	// The shared executor metadata (pool.MetaInfo): the approval target (empty on the
	// redeem side, which burns from the caller) and the block the snapshot was read at.
	ApprovalAddress string `json:"approvalAddress"`
	BlockNumber     uint64 `json:"blockNumber"`
}
