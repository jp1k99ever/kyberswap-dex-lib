package everlongrebalancer

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRedeemLegsFloorIdleSeparately: CvammALM.withdraw floors the accounted and idle
// parts separately, so the physical legs can land a wei under the combined-total
// preview — and never above it.
func TestRedeemLegsFloorIdleSeparately(t *testing.T) {
	s := &VaultState{
		AlmStableReserve:   big.NewInt(1_000_003),
		AlmVolatileReserve: big.NewInt(2_000_005),
		AlmIdleStable:      big.NewInt(7),
		AlmIdleVolatile:    big.NewInt(11),
		AlmSupply:          big.NewInt(1_000),
	}
	for shares := int64(1); shares < 400; shares++ {
		a := big.NewInt(shares)
		gotS, gotV := s.redeemLegs(a)
		wantS := mulDiv(s.AlmStableReserve, a, s.AlmSupply)
		wantV := mulDiv(s.AlmVolatileReserve, a, s.AlmSupply)
		dS := new(big.Int).Sub(wantS, gotS)
		dV := new(big.Int).Sub(wantV, gotV)
		require.True(t, dS.Sign() >= 0 && dS.Cmp(big.NewInt(1)) <= 0, "stable: %s vs preview %s", gotS, wantS)
		require.True(t, dV.Sign() >= 0 && dV.Cmp(big.NewInt(1)) <= 0, "volatile: %s vs preview %s", gotV, wantV)
	}
	// at least one size actually loses the wei, else the split is not exercised
	var lost bool
	for shares := int64(1); shares < 400 && !lost; shares++ {
		gotS, _ := s.redeemLegs(big.NewInt(shares))
		lost = gotS.Cmp(mulDiv(s.AlmStableReserve, big.NewInt(shares), s.AlmSupply)) < 0
	}
	require.True(t, lost)

	// without the idle words the legs ARE the preview
	s.AlmIdleStable, s.AlmIdleVolatile = nil, nil
	gotS, _ := s.redeemLegs(big.NewInt(123))
	require.Zero(t, gotS.Cmp(mulDiv(s.AlmStableReserve, big.NewInt(123), s.AlmSupply)))
}

// TestRedeemAlmSharesMatchesVaultTransferOnly: CollateralVault.redeem returns the
// ERC-4626 preview but transfers the raw ratio; the swapper reverts when they differ,
// which happens as soon as totalAssets and totalSupply part ways.
func TestRedeemAlmSharesMatchesVaultTransferOnly(t *testing.T) {
	s := &VaultState{CvTotalAssets: big.NewInt(612_503_052_989_603), CvTotalSupply: big.NewInt(612_503_052_989_603)}
	raw, ok := s.redeemAlmShares(big.NewInt(325_591_518_529))
	require.True(t, ok, "assets == supply: preview and transfer agree")
	require.Zero(t, raw.Cmp(big.NewInt(325_591_518_529)))

	// assets = 2*supply: for n = supply/2 the preview is floor(n(2S+1)/(S+1)) = S-1
	// while the transfer is n*2S/S = S.
	s.CvTotalAssets = new(big.Int).Mul(s.CvTotalSupply, big.NewInt(2))
	_, ok = s.redeemAlmShares(new(big.Int).Quo(s.CvTotalSupply, big.NewInt(2)))
	require.False(t, ok, "a donated vault trips the mismatch")
}

// TestBorrowerGatesFollowRecoveryMode: BorrowerOperations forbids collateral withdrawal
// in recovery mode (TCR < CCR), lets debt grow there only at ICR >= CCR without a
// decrease, and in normal mode keeps ICR >= MCR and TCR >= CCR.
func TestBorrowerGatesFollowRecoveryMode(t *testing.T) {
	wad := bigWad
	s := &VaultState{
		Collateral: big.NewInt(100), // shares
		Debt:       new(big.Int).Mul(big.NewInt(50), wad),
		// computeCR is coll*price/debt with the result in WAD: with 100 raw shares and
		// 50 WAD of debt, a price of 2 WAD*WAD puts the ICR at 4.0 WAD.
		IcrPriceWad:   new(big.Int).Mul(new(big.Int).Mul(big.NewInt(2), wad), wad),
		McrWad:        new(big.Int).Mul(big.NewInt(11), new(big.Int).Quo(wad, big.NewInt(10))),
		CcrWad:        new(big.Int).Mul(big.NewInt(15), new(big.Int).Quo(wad, big.NewInt(10))),
		SysCollateral: big.NewInt(1_000),
		SysDebt:       new(big.Int).Mul(big.NewInt(1_000), wad), // TCR 2.0: normal mode
	}
	lev := func(dColl, dDebt int64) bool {
		return s.borrowerGatesAccept(true, new(big.Int).Add(s.Collateral, big.NewInt(dColl)),
			new(big.Int).Add(s.Debt, new(big.Int).Mul(big.NewInt(dDebt), wad)))
	}
	dlv := func(dColl, dDebt int64) bool {
		return s.borrowerGatesAccept(false, new(big.Int).Sub(s.Collateral, big.NewInt(dColl)),
			new(big.Int).Sub(s.Debt, new(big.Int).Mul(big.NewInt(dDebt), wad)))
	}
	require.True(t, lev(10, 10))
	require.True(t, dlv(10, 10))
	require.False(t, lev(10, 200), "ICR would drop below MCR")
	require.False(t, dlv(90, 10), "ICR would drop below MCR")
	// TCR floor: a debt increase that pushes the system under CCR
	s.SysDebt = new(big.Int).Mul(big.NewInt(1_330), wad) // TCR 1.503
	require.False(t, lev(1, 10), "TCR would drop below CCR")
	require.True(t, dlv(1, 10), "repaying debt raises the TCR")

	// recovery mode
	s.SysDebt = new(big.Int).Mul(big.NewInt(1_400), wad) // TCR 1.43 < CCR
	require.False(t, dlv(1, 1), "no collateral withdrawal in recovery mode")
	require.True(t, lev(100, 10), "ICR improves and stays above CCR")
	require.False(t, lev(1, 40), "ICR decreases")

	// untracked words skip the gates
	s.CcrWad = nil
	require.True(t, dlv(1, 1))
}
