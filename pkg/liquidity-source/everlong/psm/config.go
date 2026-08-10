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
	// FeeCaller is the address the tracker quotes feeHook.calcFee for. The shipped
	// FeeHook keys fees per CALLER with a zero default, so this should be the aggregator
	// executor/adapter once known; the zero address (default) matches any caller without
	// a custom fee entry.
	FeeCaller string `json:"feeCaller,omitempty"`
	// GasDeposit / GasRedeem override the per-direction gas estimates.
	GasDeposit int64 `json:"gasDeposit,omitempty"`
	GasRedeem  int64 `json:"gasRedeem,omitempty"`
}
