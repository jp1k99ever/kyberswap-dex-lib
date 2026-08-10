package everlongpsm

import (
	"math/big"
)

// StaticExtra is immutable per pool (one pool per whitelisted stable).
type StaticExtra struct {
	PSM string `json:"psm"`
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
	// EntryFeeBp / ExitFeeBp: feeHook.calcFee for the configured caller (bp of the
	// debt-token gross on deposit, of the stable gross on redeem).
	EntryFeeBp *big.Int `json:"entryBp"`
	ExitFeeBp  *big.Int `json:"exitBp"`
	// MintCap / DebtTokenMinted: the per-stable debt issuance room. A deposit needs
	// minted + gross <= cap.
	MintCap         *big.Int `json:"cap"`
	DebtTokenMinted *big.Int `json:"minted"`
	// StableReserve: the PSM's stable balance backing the redeem direction.
	StableReserve *big.Int `json:"reserve"`
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
}
