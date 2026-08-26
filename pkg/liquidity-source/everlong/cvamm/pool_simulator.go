package everlongcvamm

import (
	"math/big"

	"github.com/goccy/go-json"
	"github.com/holiman/uint256"
	"github.com/samber/lo"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/util/big256"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/util/bignumber"
)

// PoolSimulator prices a CvammALM venue: a single-LP AMM that holds both tokens itself
// and evaluates a closed-form reservation curve — no pool contract, no ticks, no rungs.
// token0 is the 18-decimal stable, token1 the volatile leg (pinned by the contract, not
// sorted by address). Exact-input only; the fee is a directional OUTPUT haircut sampled
// pre-trade; partial fills are normal (the band's finite support truncates rather than
// reverts) and the unspent input is returned as RemainingTokenAmountIn.
type PoolSimulator struct {
	pool.Pool
	StaticExtra StaticExtra
	Extra       Extra

	// accounted tradeable reserves (idle excluded) — the on-chain solvency clamp caps
	// the gross payout at these, and so does the quote.
	reserveStable   *uint256.Int
	reserveVolatile *uint256.Int
	// The ALM's idle balances, carried ABSOLUTELY: seeded from the snapshot when the
	// venue reported them, then moved by everything that moves idle on-chain — a swap's
	// output fee stays inside the ALM as idle, and a coupled mint/burn takes or releases
	// a pro-rata slice of it. Couplers read totals (getTotalAmounts) through
	// GetTotalReserves, so this has to move exactly when the venue's does.
	idleAccStable   *uint256.Int
	idleAccVolatile *uint256.Int
	gasStableIn     int64
	gasVolatileIn   int64
	// stateExact is cleared when a requested transition cannot be replayed exactly.
	// liquidityStateExact additionally records whether the snapshot carried both idle
	// buckets, which reverse coupling needs even though a direct first quote does not.
	// feeStateExact starts true because poolFeeDirectional was sampled at the snapshot;
	// after a book move it remains true only if the full law re-attests and reconstructs.
	stateExact          bool
	liquidityStateExact bool
	feeStateExact       bool
	// reservationMarkX is the exact CvammCurve coordinate selected by the deployed
	// ClammAlmAdapter's reservation-price conversion. The reservation price, anchor and
	// support do not move during swaps or ALM deposits/withdrawals, so solving the
	// 128-step inverse once avoids nesting it inside the rebalancer's lot bisections.
	reservationMarkX *big.Int
}

var _ = pool.RegisterFactory0(DexType, NewPoolSimulator)

func NewPoolSimulator(p entity.Pool) (*PoolSimulator, error) {
	var extra Extra
	if err := json.Unmarshal([]byte(p.Extra), &extra); err != nil {
		return nil, err
	}
	var staticExtra StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &staticExtra); err != nil {
		return nil, err
	}
	if err := validateEntityProfile(p, &staticExtra); err != nil {
		return nil, err
	}

	info := pool.PoolInfo{
		Address:     p.Address,
		Exchange:    p.Exchange,
		Type:        p.Type,
		Tokens:      lo.Map(p.Tokens, func(e *entity.PoolToken, _ int) string { return e.Address }),
		Reserves:    lo.Map(p.Reserves, func(e string, _ int) *big.Int { return bignumber.NewBig(e) }),
		BlockNumber: p.BlockNumber,
	}
	if len(info.Reserves) != 2 {
		return nil, ErrInvalidToken
	}
	// bignumber.NewBig returns nil on a malformed reserve string, and FromBig(nil) reports
	// overflow=false — so the guard below would pass a nil through to the quote path.
	if info.Reserves[0] == nil || info.Reserves[1] == nil {
		return nil, ErrOverflow
	}
	reserveStable, overflow := uint256.FromBig(info.Reserves[0])
	if overflow {
		return nil, ErrOverflow
	}
	reserveVolatile, overflow := uint256.FromBig(info.Reserves[1])
	if overflow {
		return nil, ErrOverflow
	}

	gasStableIn, gasVolatileIn := staticExtra.GasStableIn, staticExtra.GasVolatileIn
	if gasStableIn == 0 {
		gasStableIn = defaultGasStableIn
	}
	if gasVolatileIn == 0 {
		gasVolatileIn = defaultGasVolatileIn
	}

	reservationMarkX := reservationMarkCoordinate(&extra)
	sampledFeesExact := extra.FeeStableInWad != nil && extra.FeeVolatileInWad != nil &&
		extra.FeeStableInWad.Lt(uWad) && extra.FeeVolatileInWad.Lt(uWad)
	return &PoolSimulator{
		Pool:            pool.Pool{Info: info},
		StaticExtra:     staticExtra,
		Extra:           extra,
		reserveStable:   reserveStable,
		reserveVolatile: reserveVolatile,
		idleAccStable:   idleOrZero(extra.IdleStable),
		idleAccVolatile: idleOrZero(extra.IdleVolatile),
		gasStableIn:     gasStableIn,
		gasVolatileIn:   gasVolatileIn,
		stateExact:      true,
		liquidityStateExact: extra.IdleStable != nil && extra.IdleVolatile != nil &&
			extra.Kappa != nil && !extra.Kappa.IsZero() && reservationMarkX != nil,
		feeStateExact:    sampledFeesExact,
		reservationMarkX: reservationMarkX,
	}, nil
}

// CalcAmountOut mirrors CvammSwapLib.execute step for step (no price bound): coordinate
// fill -> solvency clamp -> pre-trade output-side fee. Pure — no state is written.
func (p *PoolSimulator) CalcAmountOut(params pool.CalcAmountOutParams) (*pool.CalcAmountOutResult, error) {
	if !p.profileValid() {
		return nil, ErrInvalidProfile
	}
	indexIn, indexOut := p.GetTokenIndex(params.TokenAmountIn.Token), p.GetTokenIndex(params.TokenOut)
	if indexIn < 0 || indexOut < 0 || indexIn == indexOut {
		return nil, ErrInvalidToken
	}
	stableIn := indexIn == 0
	if !p.stateExact {
		return nil, ErrInexactLiquidityState
	}
	if !p.feeStateExact {
		return nil, ErrInexactFeeState
	}

	e := &p.Extra
	if e.Paused {
		return nil, ErrPaused
	}
	if e.XWad == nil || e.AnchorSqrtX96 == nil || e.Kappa == nil ||
		e.Support.AWad == nil || e.Support.XLo == nil || e.Support.XHi == nil || e.Support.YHi == nil {
		return nil, ErrRetractedBook
	}

	var amountIn uint256.Int
	if overflow := amountIn.SetFromBig(params.TokenAmountIn.Amount); overflow || amountIn.IsZero() {
		return nil, ErrOverflow
	}

	var gross, xAfter, unspent uint256.Int
	if err := swapExactInX96(&gross, &xAfter, &unspent, &e.Support, e.AnchorSqrtX96, e.Kappa,
		e.XWad, stableIn, &amountIn); err != nil {
		return nil, err
	}
	var used uint256.Int
	used.Sub(&amountIn, &unspent)
	if used.IsZero() || gross.IsZero() {
		return nil, ErrSwapExhausted
	}

	// The curve PRICES; the accounted reserves are authoritative for SOLVENCY. A fill
	// walking to the band edge can quote a hair above the cached balance — clamp DOWN,
	// as the venue does, so the quote never exceeds what the book holds.
	available := p.reserveStable
	if stableIn {
		available = p.reserveVolatile
	}
	if gross.Gt(available) {
		gross.Set(available)
	}
	if gross.IsZero() {
		return nil, ErrSwapExhausted
	}

	// Fee on the output leg, sampled pre-trade, floored — so the net rounds up by <= 1
	// base unit in the taker's favour, matching the chain.
	feeWad := e.FeeVolatileInWad
	if stableIn {
		feeWad = e.FeeStableInWad
	}
	var fee, netOut uint256.Int
	if feeWad != nil && !feeWad.Lt(uWad) {
		// A fee at or above 100% would underflow `gross - fee` below and quote a wrapped
		// amount. The venue caps its own fee at WAD, so this is an impossible read.
		return nil, ErrInvalidFee
	}
	if feeWad != nil {
		big256.MulDivDown(&fee, &gross, feeWad, uWad)
	}
	netOut.Sub(&gross, &fee)
	if netOut.IsZero() {
		return nil, ErrZeroAmountOut
	}

	remainingTokenAmountIn := &pool.TokenAmount{Token: params.TokenAmountIn.Token, Amount: bignumber.ZeroBI}
	if !unspent.IsZero() {
		remainingTokenAmountIn.Amount = unspent.ToBig()
	}

	gas := p.gasVolatileIn
	if stableIn {
		gas = p.gasStableIn
	}

	return &pool.CalcAmountOutResult{
		TokenAmountOut:         &pool.TokenAmount{Token: params.TokenOut, Amount: netOut.ToBig()},
		Fee:                    &pool.TokenAmount{Token: params.TokenOut, Amount: fee.ToBig()},
		RemainingTokenAmountIn: remainingTokenAmountIn,
		Gas:                    gas,
		SwapInfo: SwapInfo{
			XAfter:       &xAfter,
			AmountInUsed: &used,
			GrossOut:     &gross,
			FeeOut:       new(uint256.Int).Set(&fee),
			StableIn:     stableIn,
		},
	}, nil
}

// CalcAmountIn is intentionally rejected: the venue is exact-input only (the fee is an
// output-side haircut and the curve solves the forward direction only).
func (p *PoolSimulator) CalcAmountIn(pool.CalcAmountInParams) (*pool.CalcAmountInResult, error) {
	return nil, ErrExactOutNotSupported
}

// UpdateBalance replays the fill from SwapInfo: the input leg grows by the amount
// actually used, the output leg shrinks by the GROSS output (net + fee — the fee leaves
// the priced book into idle), and the coordinate moves to xAfter. kappa, the anchor and
// the band never move on a swap. All pointers are reassigned wholesale (copy-on-write).
func (p *PoolSimulator) UpdateBalance(params pool.UpdateBalanceParams) {
	si, ok := params.SwapInfo.(SwapInfo)
	if !ok {
		return
	}
	preX, preStable, preVolatile := p.Extra.XWad.ToBig(), p.reserveStable.ToBig(), p.reserveVolatile.ToBig()
	p.Extra.XWad = new(uint256.Int).Set(si.XAfter)
	if si.StableIn {
		p.reserveStable = new(uint256.Int).Add(p.reserveStable, si.AmountInUsed)
		p.reserveVolatile = new(uint256.Int).Sub(p.reserveVolatile, si.GrossOut)
	} else {
		p.reserveVolatile = new(uint256.Int).Add(p.reserveVolatile, si.AmountInUsed)
		p.reserveStable = new(uint256.Int).Sub(p.reserveStable, si.GrossOut)
	}
	p.Info.Reserves = []*big.Int{p.reserveStable.ToBig(), p.reserveVolatile.ToBig()}
	if si.FeeOut != nil { // the fee stays inside the ALM as idle
		if si.StableIn {
			p.idleAccVolatile = new(uint256.Int).Add(p.idleAccVolatile, si.FeeOut)
		} else {
			p.idleAccStable = new(uint256.Int).Add(p.idleAccStable, si.FeeOut)
		}
	}
	p.reSampleAfterBookMove(preX, preStable, preVolatile)
}

// GetTotalReserves is the TOTAL ALM inventory delta domain (accounted + fees idled since
// this snapshot) — what the rebalancer's getTotalAmounts-based words move by.
func (p *PoolSimulator) GetTotalReserves() []*big.Int {
	return []*big.Int{
		new(big.Int).Add(p.reserveStable.ToBig(), p.idleAccStable.ToBig()),
		new(big.Int).Add(p.reserveVolatile.ToBig(), p.idleAccVolatile.ToBig()),
	}
}

// CurrentInventoryXWad exposes a copy of the curve coordinate to loosely-coupled
// simulators. A direct swap moves x; pro-rata ALM liquidity changes do not. Callers can
// therefore detect when a same-route swap invalidated an external spot/reference gate
// that cannot be reconstructed from the local book alone.
func (p *PoolSimulator) CurrentInventoryXWad() *big.Int {
	if p.Extra.XWad == nil {
		return nil
	}
	return p.Extra.XWad.ToBig()
}

// SnapshotBlockNumber identifies the on-chain snapshot this exact liquidity state came
// from. Meta pools must not combine it with another venue snapshot from a different
// block merely because their visible reserve summaries happen to collide.
func (p *PoolSimulator) SnapshotBlockNumber() uint64 {
	return p.Info.BlockNumber
}

// ReservationValuePerShareWad mirrors the deployed ClammAlmAdapter getter against this
// simulator's CURRENT book. It intentionally recomputes the whole once-floored mark;
// incrementing a previously floored rvps by a separately floored fee delta can drift by
// one wei. reservationPriceWad and totalSupply are carried by the rebalancer snapshot.
func (p *PoolSimulator) ReservationValuePerShareWad(reservationPriceWad, totalSupply *big.Int) (
	*big.Int, bool) {
	if !p.IsLiquidityStateExact() || reservationPriceWad == nil || reservationPriceWad.Sign() <= 0 ||
		totalSupply == nil || totalSupply.Sign() <= 0 || p.Extra.Kappa == nil ||
		p.Extra.Kappa.IsZero() || p.Extra.ReservationPriceWad == nil ||
		reservationPriceWad.Cmp(p.Extra.ReservationPriceWad.ToBig()) != 0 ||
		p.reservationMarkX == nil {
		return nil, false
	}

	stable, volatile := new(big.Int), new(big.Int)
	// CvammALM._bookIsFunded also requires at least one accounted reserve. A retracted
	// book retains a non-zero kappa but the getter deliberately values idle only, avoiding
	// double-counting the retracted curve inventory.
	if !p.reserveStable.IsZero() || !p.reserveVolatile.IsZero() {
		stable, volatile = reservesAt(&p.Extra.Support, p.Extra.AnchorSqrtX96.ToBig(),
			p.Extra.Kappa.ToBig(), p.reservationMarkX)
	}
	stable.Add(stable, p.idleAccStable.ToBig())
	volatile.Add(volatile, p.idleAccVolatile.ToBig())
	value := new(big.Int).Add(stable, mulDivFloorBig(volatile, reservationPriceWad, bigWadFee))
	return mulDivFloorBig(value, bigWadFee, totalSupply), true
}

var (
	maxDirectReservationPriceWad, _ = new(big.Int).SetString(
		"18446744073709551615999999999999999999", 10)
	maxRawReservationPriceWad, _ = new(big.Int).SetString(
		"340256786836388094070642339899681172762184831912254825631", 10)
	q96Reservation = new(big.Int).Lsh(big.NewInt(1), 96)
)

// reservationMarkCoordinate performs the expensive, immutable part of
// ClammAlmAdapter.reservationValuePerShareWad. It mirrors
// ClammRawPriceMath.sqrtPriceX96/reciprocalSqrtPriceX96, CvammALM._invertSqrtX96,
// and CvammCurve.xAtSqrtX96 with each Solidity floor intact.
func reservationMarkCoordinate(e *Extra) *big.Int {
	if e == nil || e.ReservationPriceWad == nil || e.ReservationPriceWad.IsZero() ||
		e.AnchorSqrtX96 == nil || e.Support.AWad == nil || e.Support.XLo == nil ||
		e.Support.XHi == nil {
		return nil
	}
	reservation := e.ReservationPriceWad.ToBig()
	if reservation.Cmp(maxRawReservationPriceWad) > 0 {
		return nil
	}
	var directSqrt *big.Int
	if reservation.Cmp(maxDirectReservationPriceWad) <= 0 {
		directSqrt = new(big.Int).Sqrt(mulDivFloorBig(reservation, bigQ192, bigWadFee))
	} else {
		directSqrt = mulDivFloorBig(new(big.Int).Sqrt(reservation), q96Reservation,
			big.NewInt(1_000_000_000))
	}
	minSqrt, maxSqrt := big256.MinSqrtRatio.ToBig(), big256.MaxSqrtRatio.ToBig()
	if directSqrt.Cmp(minSqrt) <= 0 || directSqrt.Cmp(maxSqrt) >= 0 {
		return nil
	}
	poolSqrt := new(big.Int).Div(bigQ192, directSqrt)
	if poolSqrt.Cmp(minSqrt) <= 0 || poolSqrt.Cmp(maxSqrt) >= 0 {
		return nil
	}
	curveSqrt := new(big.Int).Div(bigQ192, poolSqrt)
	if curveSqrt.Sign() <= 0 || curveSqrt.BitLen() > 160 {
		return nil
	}
	return xAtCurveSqrt(&e.Support, e.AnchorSqrtX96.ToBig(), curveSqrt)
}

// xAtCurveSqrt mirrors CvammCurve.xAtSqrtX96 -> xAtPrice, including its 128-step
// bisection and final clamp into the funded support.
func xAtCurveSqrt(sup *Support, anchorSqrtX96, curveSqrtX96 *big.Int) *big.Int {
	if sup == nil || sup.AWad == nil || sup.XLo == nil || sup.XHi == nil ||
		anchorSqrtX96 == nil || anchorSqrtX96.Sign() <= 0 || curveSqrtX96 == nil ||
		curveSqrtX96.Sign() <= 0 {
		return nil
	}
	ratio := mulDivFloorBig(curveSqrtX96, bigWadFee, anchorSqrtX96)
	target := mulDivFloorBig(ratio, ratio, bigWadFee)
	a := sup.AWad.ToBig()
	lo := new(big.Int).Set(uMinXWad.ToBig())
	hi := new(big.Int).Set(uMaxXWad.ToBig())
	pLo, pHi := priceAtXWad(lo, a), priceAtXWad(hi, a)
	if pLo == nil || pHi == nil {
		return nil
	}
	var x *big.Int
	switch {
	case target.Cmp(pLo) >= 0:
		x = lo
	case target.Cmp(pHi) <= 0:
		x = hi
	default:
		for i := 0; i < 128; i++ {
			mid := new(big.Int).Rsh(new(big.Int).Add(lo, hi), 1)
			if mid.Cmp(lo) == 0 {
				break
			}
			p := priceAtXWad(mid, a)
			if p == nil {
				return nil
			}
			if p.Cmp(target) > 0 {
				lo = mid
			} else {
				hi = mid
			}
		}
		x = new(big.Int).Rsh(new(big.Int).Add(lo, hi), 1)
	}
	if x.Cmp(sup.XLo.ToBig()) < 0 {
		return sup.XLo.ToBig()
	}
	if x.Cmp(sup.XHi.ToBig()) > 0 {
		return sup.XHi.ToBig()
	}
	return x
}

// ApplyLiquidityDelta replays a CollateralRebalancer fill's ALM leg on this sim —
// CvammALM.deposit when sharesDelta is positive, withdraw when it is negative — and
// returns the movement in both domains the coupler tracks: (dTotalStable, dTotalVolatile)
// for getTotalAmounts and (dAccStable, dAccVolatile) for the accounted reserves alone.
//
// The venue's own transition, not a model of it:
//
//	deposit(used0, used1) mints `shares`, takes i = floor(idle * shares / supply) of each
//	  leg out of the deposit into idle and the remainder into the priced reserves, then
//	  scales kappa by (supply + shares) / supply;
//	withdraw(shares) releases floor(reserve * shares / supply) and
//	  floor(idle * shares / supply) SEPARATELY and scales kappa by (supply - shares) /
//	  supply.
//
// Both legs are floored on their own, which is precisely what a single pro-rata scalar
// over the combined totals cannot reproduce; x, the anchor and the band are untouched by
// either path because kappa carries the whole size change (the curve is homogeneous of
// degree 1 in it).
//
// `used0`/`used1` are the amounts the ALM actually pulls on a mint and are required for
// it; a burn derives everything from `shares`. Missing idle or transition words makes
// the coupled state unpriceable; no proportional approximation is applied.
//
// Copy-on-write like UpdateBalance.
func (p *PoolSimulator) ApplyLiquidityDelta(sharesDelta, supplyBefore, used0, used1 *big.Int) (
	dTotalStable, dTotalVolatile, dAccStable, dAccVolatile *big.Int) {
	refuse := func() (*big.Int, *big.Int, *big.Int, *big.Int) {
		p.InvalidateLiquidityState()
		return nil, nil, nil, nil
	}
	if !p.IsLiquidityStateExact() || sharesDelta == nil || sharesDelta.Sign() == 0 ||
		supplyBefore == nil || supplyBefore.Sign() <= 0 || p.Extra.Kappa == nil ||
		p.Extra.Kappa.IsZero() {
		return refuse()
	}
	supplyAfter := new(big.Int).Add(supplyBefore, sharesDelta)
	if supplyAfter.Sign() <= 0 || sharesDelta.Sign() < 0 && supplyAfter.Cmp(big.NewInt(1_000)) < 0 {
		return refuse()
	}
	mint := sharesDelta.Sign() > 0
	if mint && (used0 == nil || used1 == nil || used0.Sign() < 0 || used1.Sign() < 0) {
		return refuse()
	}

	shares := new(big.Int).Abs(sharesDelta)
	// The live idle balance: the snapshot's, moved by every fee that has idled since.
	idle0, idle1 := p.idleAccStable.ToBig(), p.idleAccVolatile.ToBig()
	res0, res1 := p.reserveStable.ToBig(), p.reserveVolatile.ToBig()

	var dRes0, dRes1, dIdle0, dIdle1 *big.Int
	if mint {
		i0 := mulDivFloorBig(idle0, shares, supplyBefore)
		i1 := mulDivFloorBig(idle1, shares, supplyBefore)
		if i0.Cmp(used0) > 0 {
			i0 = new(big.Int).Set(used0)
		}
		if i1.Cmp(used1) > 0 {
			i1 = new(big.Int).Set(used1)
		}
		dRes0, dRes1 = new(big.Int).Sub(used0, i0), new(big.Int).Sub(used1, i1)
		dIdle0, dIdle1 = i0, i1
	} else {
		r0 := mulDivFloorBig(res0, shares, supplyBefore)
		r1 := mulDivFloorBig(res1, shares, supplyBefore)
		i0 := mulDivFloorBig(idle0, shares, supplyBefore)
		i1 := mulDivFloorBig(idle1, shares, supplyBefore)
		dRes0, dRes1 = new(big.Int).Neg(r0), new(big.Int).Neg(r1)
		dIdle0, dIdle1 = new(big.Int).Neg(i0), new(big.Int).Neg(i1)
	}

	newRes0 := new(big.Int).Add(res0, dRes0)
	newRes1 := new(big.Int).Add(res1, dRes1)
	newIdle0 := new(big.Int).Add(idle0, dIdle0)
	newIdle1 := new(big.Int).Add(idle1, dIdle1)
	if newRes0.Sign() < 0 || newRes1.Sign() < 0 || newIdle0.Sign() < 0 || newIdle1.Sign() < 0 {
		return refuse()
	}

	newX := p.Extra.XWad.ToBig()
	kappa := mulDivFloorBig(p.Extra.Kappa.ToBig(), supplyAfter, supplyBefore)

	rs, o1 := uint256.FromBig(newRes0)
	rv, o2 := uint256.FromBig(newRes1)
	k, o3 := uint256.FromBig(kappa)
	is, o4 := uint256.FromBig(newIdle0)
	iv, o5 := uint256.FromBig(newIdle1)
	xu, o6 := uint256.FromBig(newX)
	if o1 || o2 || o3 || o4 || o5 || o6 {
		return refuse()
	}

	preX, preStable, preVolatile := p.Extra.XWad.ToBig(), res0, res1
	p.reserveStable, p.reserveVolatile, p.Extra.Kappa = rs, rv, k
	p.idleAccStable, p.idleAccVolatile = is, iv
	p.Extra.XWad = xu
	p.Info.Reserves = []*big.Int{newRes0, newRes1}
	// The fee law reads the reserves and coordinate. Reconstruct it exactly or disable
	// the next direct quote; physical coupling remains exact either way.
	p.reSampleAfterBookMove(preX, preStable, preVolatile)

	return new(big.Int).Add(dRes0, dIdle0), new(big.Int).Add(dRes1, dIdle1), dRes0, dRes1
}

func mulDivFloorBig(x, y, d *big.Int) *big.Int {
	out := new(big.Int).Mul(x, y)
	return out.Div(out, d)
}

func (p *PoolSimulator) CloneState() pool.IPoolSimulator {
	cloned := *p
	// UpdateBalance reassigns every mutable pointer wholesale, so the struct value copy
	// is a sufficient snapshot; Info.Reserves is re-sliced for index-safety.
	cloned.Info.Reserves = lo.Map(p.Info.Reserves, func(r *big.Int, _ int) *big.Int {
		return new(big.Int).Set(r)
	})
	return &cloned
}

func (p *PoolSimulator) GetMetaInfo(_, _ string) any {
	return PoolMeta{
		ALM:             p.Info.Address,
		Adapter:         p.StaticExtra.Adapter,
		ApprovalAddress: p.GetApprovalAddress("", ""),
		BlockNumber:     p.Info.BlockNumber,
	}
}

// GetApprovalAddress: the ALM pulls the input token from the caller with transferFrom.
func (p *PoolSimulator) GetApprovalAddress(_, _ string) string {
	return p.Info.Address
}

func idleOrZero(v *uint256.Int) *uint256.Int {
	if v == nil {
		return new(uint256.Int)
	}
	return v.Clone()
}

// IsLiquidityStateExact is the loose reverse-coupling capability exposed to the
// rebalancer without a package dependency.
func (p *PoolSimulator) IsLiquidityStateExact() bool {
	return p.profileValid() && p.stateExact && p.liquidityStateExact
}

func (p *PoolSimulator) profileValid() bool {
	if p == nil {
		return false
	}
	return validStaticProfile(&p.StaticExtra, p.Info.Address, p.Info.Exchange, p.Info.Type,
		p.Info.BlockNumber, p.Info.Tokens)
}

// InvalidateLiquidityState permanently fails this clone closed after a transition that
// could not be replayed. The tracker can restore service only by supplying a fresh pool.
func (p *PoolSimulator) InvalidateLiquidityState() {
	p.stateExact = false
	p.liquidityStateExact = false
}

// reSampleAfterBookMove re-derives both directional fees exactly at the current book.
// A flat fallback-fee venue needs no reconstruction; a dynamic venue with any absent or
// mismatching input becomes unpriceable for subsequent quotes.
func (p *PoolSimulator) reSampleAfterBookMove(preX, preStable, preVolatile *big.Int) {
	if !p.Extra.FeeHookActive {
		return
	}
	if a, b, ok := reSampleFeesExact(&p.Extra, preX, preStable, preVolatile,
		p.Extra.XWad.ToBig(), p.reserveStable.ToBig(), p.reserveVolatile.ToBig()); ok {
		p.Extra.FeeStableInWad, _ = uint256.FromBig(a)
		p.Extra.FeeVolatileInWad, _ = uint256.FromBig(b)
		return
	}
	p.feeStateExact = false
}
