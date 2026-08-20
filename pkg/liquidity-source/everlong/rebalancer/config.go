package everlongrebalancer

import (
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/valueobject"
)

// Config describes one Everlong CollateralRebalancer deployment (one rebalancer +
// CollateralRebalancerSwapper pair). One config entry per chain.
type Config struct {
	DexID   string              `json:"dexId"`
	ChainID valueobject.ChainID `json:"chainID"`
	// Rebalancer is the CollateralRebalancer proxy (exchangeState; resolves the
	// CollVault and the settlement Swapper on-chain).
	Rebalancer string `json:"rebalancer"`
	// Stable/Volatile are the two swap legs (e.g. NECT 18d / WBTC 8d on Berachain).
	Stable   string `json:"stable"`
	Volatile string `json:"volatile"`
	// Math is the deployed CollRebalancerMath the rebalancer links against. REQUIRED: the
	// executor re-derives the exact deleverage gross with it on a partial fill, so a
	// missing or wrong address reverts the fill rather than degrading it. It is linked
	// into the implementation bytecode and has no getter, so it cannot be resolved
	// on-chain — listing probes deleverageQuote against it and refuses if it does not
	// answer.
	Math string `json:"math"`
	// CurveParams overrides the built-in per-chain deployed-curve constants; leave nil
	// to use the built-ins (required for chains without a built-in entry).
	CurveParams *CurveParams `json:"curveParams,omitempty"`
	// GasLeverage / GasDeleverage override the measured per-direction gas estimates.
	GasLeverage   int64 `json:"gasLeverage,omitempty"`
	GasDeleverage int64 `json:"gasDeleverage,omitempty"`
}
