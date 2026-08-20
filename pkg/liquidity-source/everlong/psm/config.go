package everlongpsm

import (
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// Config describes one PermissionlessPSM deployment. The whitelist mapping is not
// enumerable on-chain, so the candidate stables are configuration; the lister keeps only
// the ones the PSM actually reports as listed (stables(addr) != 0) at listing time.
type Config struct {
	DexID   string              `json:"dexId"`
	ChainID valueobject.ChainID `json:"chainID"`
	// PSM is the PermissionlessPSM contract (also the swap entrypoint and, for the
	// deposit direction, the approval target).
	PSM string `json:"psm"`
	// Stables to probe for whitelisting (e.g. USDC, USDT). One pool per listed stable.
	Stables []string `json:"stables"`
	// FeeCaller is the address the tracker prices for — both feeBpFor and the cap hook's
	// getMaxPsmOutflow are keyed by CALLER. At execution that caller is the adapter, so
	// this MUST be the deployed adapter address wherever the hook carries a per-caller
	// entry (callerDiscountBp) or a per-caller outflow ceiling; otherwise the quote and
	// the fill price different actors. The zero address (default) is only correct while
	// no such entry exists — TestFeeIsCallerInvariant is the tripwire for that.
	FeeCaller string `json:"feeCaller,omitempty"`
	// GasDeposit / GasRedeem override the per-direction gas estimates.
	GasDeposit int64 `json:"gasDeposit,omitempty"`
	GasRedeem  int64 `json:"gasRedeem,omitempty"`
}
