package everlongpsm

import (
	"math/big"
)

// StaticExtra is immutable per pool (one pool per whitelisted stable).
type StaticExtra struct {
	// ProfileVersion forces pools persisted before the exact production attestation to
	// relist rather than silently retaining the former generic-hook behavior.
	ProfileVersion uint8  `json:"profileVersion"`
	PSM            string `json:"psm"`
	// PSMCodeHash pins the reviewed direct PermissionlessPSM runtime. Hook topology
	// alone cannot attest the venue's accounting and partial-fill semantics.
	PSMCodeHash string `json:"psmCodeHash"`
	DebtToken   string `json:"debtToken"`
	Stable      string `json:"stable"`
	// MetaCore is the protocol core whose paused() the PSM ORs into its own on every
	// swap (`notPaused` checks `paused || metaCore.paused()`). Immutable on the PSM (set
	// in the constructor, no setter), so it is resolved once at listing.
	MetaCore string `json:"metaCore,omitempty"`
	// FeeCaller is the exact direct EverlongPsmAdapter address presented to feeBpFor at
	// execution. Caller policy is address-keyed, and the runtime hash prevents a wrong
	// unrelated contract from satisfying the nonzero-code check.
	FeeCaller         string `json:"feeCaller"`
	FeeCallerCodeHash string `json:"feeCallerCodeHash"`
	// FeeHook and its runtime hash pin the only book-independent policy implementation
	// whose sampled rates UpdateBalance is allowed to reuse within a route.
	FeeHook         string `json:"feeHook"`
	FeeHookCodeHash string `json:"feeHookCodeHash"`
	// The supported topology has one append-only listed stable and no cap/yield hooks.
	// The tracker re-attests those live pointers and the list cardinality every refresh.
	ListedStableCount int `json:"listedStableCount"`
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
	Paused    bool `json:"paused"`
	PSMBonded bool `json:"psmBonded"`
	// feeBpFor for the exact configured execution caller: the FINISHED rate, not a hook
	// parameter. The attested PsmFlatFeeHook is book-independent, so this rate remains
	// exact after UpdateBalance; another runtime or caller never reaches the simulator.
	EntryFeeBp *big.Int `json:"entryBp,omitempty"`
	ExitFeeBp  *big.Int `json:"exitBp,omitempty"`
	// Mint room under the structural mintCap. A nonzero cap hook is unsupported.
	AvailableMint *big.Int `json:"availMint"`
	// Per-stable book; the redeem burn cannot exceed it (the chain underflows past it).
	DebtTokenMinted *big.Int `json:"minted"`
	// Stable payable right now. A nonzero yield hook is unsupported, so this is idle
	// HONEY held directly by the PSM and can be replayed exactly.
	AvailableReserve *big.Int `json:"reserve"`
}

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
