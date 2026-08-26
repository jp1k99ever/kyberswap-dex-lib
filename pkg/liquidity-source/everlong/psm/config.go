package everlongpsm

import (
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// Config describes the reviewed PermissionlessPSM production profile. The current
// integration intentionally supports exactly one listed stable and the attested flat-fee
// hook with no cap/yield hooks; a richer topology is rejected instead of approximated.
type Config struct {
	DexID   string              `json:"dexId"`
	ChainID valueobject.ChainID `json:"chainID"`
	// PSM is the PermissionlessPSM contract (also the swap entrypoint and, for the
	// deposit direction, the approval target).
	PSM string `json:"psm"`
	// Stables must contain exactly the PSM's sole listed stable (Bera USD/BUSD on Berachain).
	Stables []string `json:"stables"`
	// FeeCaller is REQUIRED and must be the deployed direct EverlongPsmAdapter address
	// that calls the PSM. Its reviewed runtime is attested at the snapshot block. A
	// delegatecall executor, EOA, anticipated deployment address or unrelated contract is
	// rejected; the deployed hook has live caller exemptions, so none is a safe proxy.
	FeeCaller string `json:"feeCaller"`
	// GasDeposit / GasRedeem override the per-direction gas estimates.
	GasDeposit int64 `json:"gasDeposit,omitempty"`
	GasRedeem  int64 `json:"gasRedeem,omitempty"`
}
