package everlongrebalancer

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

// supportedCurve is part of the runtime profile, not untrusted cache metadata. The
// allowlisted implementation/math hashes bind this exact frozen Berachain curve; a
// different usable-looking tuple is still different executable pricing semantics.
var supportedCurve = berachainCurveParams()

func validRequiredAddress(address string) bool {
	return common.IsHexAddress(address) && common.HexToAddress(address) != (common.Address{})
}

func sameBigInt(a, b *big.Int) bool {
	return a != nil && b != nil && a.Cmp(b) == 0
}

func sameCurve(a, b *CurveParams) bool {
	if a == nil || b == nil || !a.usable() || !b.usable() {
		return false
	}
	for _, pair := range [][2]*big.Int{
		{a.LeverageRatioWad, b.LeverageRatioWad},
		{a.HZero, b.HZero},
		{a.HJoin, b.HJoin},
		{a.HWall, b.HWall},
		{a.Width, b.Width},
		{a.DJoin, b.DJoin},
		{a.DWall, b.DWall},
		{a.RescueSpreadPpm, b.RescueSpreadPpm},
		{a.PhysicalCrFloorWad, b.PhysicalCrFloorWad},
	} {
		if !sameBigInt(pair[0], pair[1]) {
			return false
		}
	}
	for i := range a.BezierPhi {
		if !sameBigInt(a.BezierPhi[i], b.BezierPhi[i]) {
			return false
		}
	}
	for i := range a.BezierIntegral {
		if !sameBigInt(a.BezierIntegral[i], b.BezierIntegral[i]) {
			return false
		}
	}
	return true
}

// validateProfile rejects a pool object whose immutable identities no longer describe
// the one production stack whose bytecode and math this package mirrors. This method is
// intentionally safe to call on a directly-decoded msgpack object: no field is
// dereferenced before its presence has been checked.
func (s *PoolSimulator) validateProfile() error {
	se := &s.StaticExtra
	switch {
	case !strings.EqualFold(se.ALMAdapterCodeHash, supportedAlmAdapterCodeHash):
		return ErrUnsupportedAdapter
	case !strings.EqualFold(se.ImplementationCodeHash, supportedRebalancerImplementationCodeHash):
		return ErrUnsupportedImplementation
	case !strings.EqualFold(se.SwapperCodeHash, supportedSettlementSwapperCodeHash):
		return ErrUnsupportedSwapper
	case !strings.EqualFold(se.MathCodeHash, supportedCollRebalancerMathCodeHash):
		return ErrUnsupportedMath
	}

	required := []struct {
		name    string
		address string
	}{
		{"rebalancer", se.Rebalancer},
		{"swapper", se.Swapper},
		{"collVault", se.CollVault},
		{"alm", se.ALM},
		{"underlyingCvamm", se.UnderlyingCvamm},
		{"math", se.Math},
		{"positionManager", se.PositionManager},
		{"borrowerOperations", se.BorrowerOperations},
		{"core", se.Core},
		{"underlyingDepositAllowlist", se.UnderlyingDepositAllowlist},
		{"implementation", se.Implementation},
		{"stableToken", se.StableToken},
		{"volatileToken", se.VolatileToken},
		{"managedVault", se.ManagedVault},
	}
	for _, field := range required {
		if !validRequiredAddress(field.address) {
			return fmt.Errorf("%w: %s is missing, zero, or malformed", ErrInvalidPoolProfile, field.name)
		}
	}
	if strings.EqualFold(se.StableToken, se.VolatileToken) {
		return fmt.Errorf("%w: token identities are equal", ErrInvalidPoolProfile)
	}
	if !strings.EqualFold(s.Info.Address, se.Swapper) {
		return fmt.Errorf("%w: pool address is not the settlement swapper", ErrInvalidPoolProfile)
	}
	if s.Info.BlockNumber == 0 {
		return fmt.Errorf("%w: snapshot block is not pinned", ErrInvalidPoolProfile)
	}
	if len(s.Info.Tokens) != 2 ||
		!strings.EqualFold(s.Info.Tokens[0], se.StableToken) ||
		!strings.EqualFold(s.Info.Tokens[1], se.VolatileToken) {
		return fmt.Errorf("%w: token order is not [stable, volatile]", ErrInvalidPoolProfile)
	}
	if !sameCurve(&se.CurveParams, &supportedCurve) {
		return ErrInvalidCurveParams
	}
	if s.Extra.LiveCurve != nil && !sameCurve(s.Extra.LiveCurve, &supportedCurve) {
		return ErrInvalidCurveParams
	}
	return nil
}

// baseIdentityMatches binds the meta pool to the exact underlying address and token
// ordering listed by the rebalancer. Reserve coherence alone cannot detect a base-map
// collision with another two-token simulator carrying the same numbers.
func (s *PoolSimulator) baseIdentityMatches(base pool.IPoolSimulator) bool {
	if base == nil || !strings.EqualFold(base.GetAddress(), s.StaticExtra.UnderlyingCvamm) {
		return false
	}
	tokens := base.GetTokens()
	return len(tokens) == 2 &&
		strings.EqualFold(tokens[0], s.StaticExtra.StableToken) &&
		strings.EqualFold(tokens[1], s.StaticExtra.VolatileToken)
}
