package everlongrebalancer

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/goccy/go-json"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
)

// supportedCurve is part of the runtime profile, not untrusted cache metadata. The
// allowlisted implementation/math hashes bind this exact frozen Berachain curve; a
// different usable-looking tuple is still different executable pricing semantics.
var supportedCurve = berachainCurveParams()

const rebalancerConfigProfileVersion uint64 = 1

// rebalancerConfigProfile is the canonical, semantic listing configuration. Stable and
// Volatile are always the pair derived from the settlement swapper: configuring either
// address is an optional assertion, so an omitted assertion and an equal assertion
// describe the same listed venue. Including the derived pair still binds a decoded
// simulator when both its StaticExtra token and matching PoolInfo token are corrupted.
type rebalancerConfigProfile struct {
	Version       uint64      `json:"version"`
	DexID         string      `json:"dexId"`
	ChainID       uint        `json:"chainId"`
	Rebalancer    string      `json:"rebalancer"`
	Stable        string      `json:"stable"`
	Volatile      string      `json:"volatile"`
	Math          string      `json:"math"`
	Curve         CurveParams `json:"curve"`
	GasLeverage   int64       `json:"gasLeverage"`
	GasDeleverage int64       `json:"gasDeleverage"`
}

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

func canonicalRequiredAddress(raw string) (string, bool) {
	if !validRequiredAddress(raw) {
		return "", false
	}
	address := common.HexToAddress(raw)
	return hexutil.Encode(address[:]), true
}

// rebalancerConfigFingerprint is shared by listing, tracking and decoded-simulator
// validation. Keeping one encoder is important: metadata-only digests can retire a
// listing cursor, but cannot protect a persisted pool once the tracker has received it.
func rebalancerConfigFingerprint(cfg *Config, cp CurveParams, stable, volatile string) (string, error) {
	if cfg == nil || cfg.DexID == "" || cfg.ChainID == 0 ||
		cfg.GasLeverage < 0 || cfg.GasDeleverage < 0 || !cp.usable() {
		return "", ErrInvalidPoolProfile
	}
	rebalancer, ok0 := canonicalRequiredAddress(cfg.Rebalancer)
	stable, ok1 := canonicalRequiredAddress(stable)
	volatile, ok2 := canonicalRequiredAddress(volatile)
	mathAddress, ok3 := canonicalRequiredAddress(cfg.Math)
	if !ok0 || !ok1 || !ok2 || !ok3 || strings.EqualFold(stable, volatile) {
		return "", ErrInvalidPoolProfile
	}
	if cfg.Stable != "" && !strings.EqualFold(cfg.Stable, stable) {
		return "", ErrInvalidPoolProfile
	}
	if cfg.Volatile != "" && !strings.EqualFold(cfg.Volatile, volatile) {
		return "", ErrInvalidPoolProfile
	}

	raw, err := json.Marshal(rebalancerConfigProfile{
		Version: rebalancerConfigProfileVersion,
		DexID:   cfg.DexID, ChainID: uint(cfg.ChainID),
		Rebalancer: rebalancer, Stable: stable, Volatile: volatile, Math: mathAddress,
		Curve: cp, GasLeverage: cfg.GasLeverage, GasDeleverage: cfg.GasDeleverage,
	})
	if err != nil {
		return "", err
	}
	return crypto.Keccak256Hash(raw).Hex(), nil
}

func staticConfigFingerprint(se *StaticExtra) (string, error) {
	if se == nil {
		return "", ErrInvalidPoolProfile
	}
	return rebalancerConfigFingerprint(&Config{
		DexID: se.DexID, ChainID: se.ChainID, Rebalancer: se.Rebalancer,
		Stable: se.StableToken, Volatile: se.VolatileToken, Math: se.Math,
		CurveParams: &se.CurveParams,
		GasLeverage: se.GasLeverage, GasDeleverage: se.GasDeleverage,
	}, se.CurveParams, se.StableToken, se.VolatileToken)
}

func staticProfileFingerprint(se *StaticExtra) (string, error) {
	if se == nil {
		return "", ErrInvalidPoolProfile
	}
	profile := *se
	profile.ProfileHash = ""
	raw, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	return crypto.Keccak256Hash(raw).Hex(), nil
}

// validateTrackerProfile binds a persisted entity to the tracker configuration before
// any RPC is planned or sent. Static self-consistency cannot retire a rebalancer removed
// or rotated in config; the current source configuration is the authority for that.
func validateTrackerProfile(p entity.Pool, se *StaticExtra, cfg *Config) error {
	if se == nil || len(p.Tokens) != 2 || p.Tokens[0] == nil || p.Tokens[1] == nil {
		return ErrInvalidPoolProfile
	}
	probe := PoolSimulator{
		Pool: pool.Pool{Info: pool.PoolInfo{
			Address: p.Address, Exchange: p.Exchange, Type: p.Type,
			Tokens:      []string{p.Tokens[0].Address, p.Tokens[1].Address},
			BlockNumber: p.BlockNumber,
		}},
		StaticExtra: *se,
	}
	if err := probe.validateProfile(); err != nil {
		return err
	}
	if cfg == nil || cfg.DexID != se.DexID || cfg.ChainID != se.ChainID ||
		!strings.EqualFold(cfg.Rebalancer, se.Rebalancer) ||
		!strings.EqualFold(cfg.Math, se.Math) || cfg.GasLeverage != se.GasLeverage ||
		cfg.GasDeleverage != se.GasDeleverage ||
		(cfg.Stable != "" && !strings.EqualFold(cfg.Stable, se.StableToken)) ||
		(cfg.Volatile != "" && !strings.EqualFold(cfg.Volatile, se.VolatileToken)) {
		return ErrInvalidPoolProfile
	}
	cp, err := (&PoolsListUpdater{config: cfg}).resolveCurveParams()
	if err != nil || !sameCurve(&cp, &se.CurveParams) {
		return ErrInvalidPoolProfile
	}
	wantHash, err := rebalancerConfigFingerprint(cfg, cp, se.StableToken, se.VolatileToken)
	if err != nil || !strings.EqualFold(wantHash, se.ConfigHash) {
		return ErrInvalidPoolProfile
	}
	return nil
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
	if s.Info.Type != DexType || se.DexID == "" || s.Info.Exchange != se.DexID || se.ChainID == 0 {
		return fmt.Errorf("%w: source identity is not bound", ErrInvalidPoolProfile)
	}
	if !sameCurve(&se.CurveParams, &supportedCurve) {
		return ErrInvalidCurveParams
	}
	if s.Extra.LiveCurve != nil && !sameCurve(s.Extra.LiveCurve, &supportedCurve) {
		return ErrInvalidCurveParams
	}
	wantConfigHash, err := staticConfigFingerprint(se)
	if err != nil || !strings.EqualFold(wantConfigHash, se.ConfigHash) {
		return ErrInvalidPoolProfile
	}
	wantProfileHash, err := staticProfileFingerprint(se)
	if err != nil || !strings.EqualFold(wantProfileHash, se.ProfileHash) {
		return ErrInvalidPoolProfile
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
