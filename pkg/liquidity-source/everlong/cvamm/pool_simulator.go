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
	// fees accrued to ALM idle since this snapshot: swaps move accounted reserves by
	// -gross but total ALM inventory (getTotalAmounts) by -net — couplers need totals.
	idleAccStable   *uint256.Int
	idleAccVolatile *uint256.Int
	gasStableIn     int64
	gasVolatileIn   int64
	// solved once from the snapshot's own fee samples and shared by every clone of it
	feeSolve feeSolve
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

	return &PoolSimulator{
		Pool:            pool.Pool{Info: info},
		StaticExtra:     staticExtra,
		Extra:           extra,
		reserveStable:   reserveStable,
		reserveVolatile: reserveVolatile,
		idleAccStable:   new(uint256.Int),
		idleAccVolatile: new(uint256.Int),
		gasStableIn:     gasStableIn,
		gasVolatileIn:   gasVolatileIn,
	}, nil
}

// CalcAmountOut mirrors CvammSwapLib.execute step for step (no price bound): coordinate
// fill -> solvency clamp -> pre-trade output-side fee. Pure — no state is written.
func (p *PoolSimulator) CalcAmountOut(params pool.CalcAmountOutParams) (*pool.CalcAmountOutResult, error) {
	indexIn, indexOut := p.GetTokenIndex(params.TokenAmountIn.Token), p.GetTokenIndex(params.TokenOut)
	if indexIn < 0 || indexOut < 0 || indexIn == indexOut {
		return nil, ErrInvalidToken
	}
	stableIn := indexIn == 0

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
	// The fee law reads (reserves, sqrtCurve, rv, ffadPushRate); a fill moves the first
	// two, so the post-fill fee is not the one that was sampled. `rv` has no getter, but
	// it reaches the fee only through a scalar a swap cannot move, so that scalar is
	// solved from the fee already sampled this block and reused at the post-fill book.
	// reSampleFeesBest re-derives both legs and refuses when the modelled law does not
	// reproduce the pre-fill sample.
	if a, b, ok := reSampleFeesBest(&p.Extra, &p.feeSolve, preX, preStable, preVolatile,
		p.Extra.XWad.ToBig(), p.reserveStable.ToBig(), p.reserveVolatile.ToBig()); ok {
		p.Extra.FeeStableInWad, _ = uint256.FromBig(a)
		p.Extra.FeeVolatileInWad, _ = uint256.FromBig(b)
		return
	}
	// Outside that regime the fee is unknowable off-chain, so a revisit takes the worse
	// of the two samples: under-quoting costs a route, over-quoting hands the router a
	// fill the venue will not honour.
	if p.Extra.FeeStableInWad != nil && p.Extra.FeeVolatileInWad != nil {
		worse := p.Extra.FeeStableInWad
		if p.Extra.FeeVolatileInWad.Gt(worse) {
			worse = p.Extra.FeeVolatileInWad
		}
		worse = new(uint256.Int).Set(worse)
		p.Extra.FeeStableInWad = worse
		p.Extra.FeeVolatileInWad = worse
	}
}

// GetTotalReserves is the TOTAL ALM inventory delta domain (accounted + fees idled since
// this snapshot) — what the rebalancer's getTotalAmounts-based words move by.
func (p *PoolSimulator) GetTotalReserves() []*big.Int {
	return []*big.Int{
		new(big.Int).Add(p.reserveStable.ToBig(), p.idleAccStable.ToBig()),
		new(big.Int).Add(p.reserveVolatile.ToBig(), p.idleAccVolatile.ToBig()),
	}
}

// ApplyLiquidityDelta folds a pro-rata ALM mint/burn (a CollateralRebalancer fill's
// deposit/withdraw leg) into this sim. The curve is homogeneous of degree 1 in kappa and
// the legs are ratio-matched, so ONE scalar — the ALM share delta over the prior supply —
// scales reserves and kappa alike and leaves x, the anchor and the band fixed. x is
// unmoved to the wei; that part is exact.
//
// This matches the venue: its deposit path scales kappa by the same ratio and splits the
// deposit so the priced book takes its own share. What it cannot fix is an inexact SCALE
// FACTOR — the caller's predicted mint runs ~0.2% above the settled one, and since a mint
// is a fraction of supply that dilutes to ~2 bps on reserves and kappa alike (measured
// against the settled fill at block 24,863,882; the coupling test bounds it at 5).
//
// A quote reads those words only through price impact, so the drift is worth far less
// than it looks: 0 ppm up to a tenth of the book, 5 ppm at half, 82 ppm with the whole
// stable leg as input. It surfaces only in a chained quote on the pushed base and rides
// the route's slippage bound; a fill quoted off a fresh snapshot is unaffected.
//
// Returns the token deltas applied. Copy-on-write like UpdateBalance.
func (p *PoolSimulator) ApplyLiquidityDelta(sharesDelta, supplyBefore *big.Int) (dStable, dVolatile *big.Int) {
	if sharesDelta == nil || supplyBefore == nil || supplyBefore.Sign() <= 0 || p.Extra.Kappa == nil {
		return nil, nil
	}
	supplyAfter := new(big.Int).Add(supplyBefore, sharesDelta)
	if supplyAfter.Sign() < 0 {
		return nil, nil
	}
	scale := func(v *uint256.Int) *uint256.Int {
		out := new(big.Int).Div(new(big.Int).Mul(v.ToBig(), supplyAfter), supplyBefore)
		u, overflow := uint256.FromBig(out)
		if overflow {
			return nil
		}
		return u
	}
	rs, rv, kappa := scale(p.reserveStable), scale(p.reserveVolatile), scale(p.Extra.Kappa)
	if rs == nil || rv == nil || kappa == nil {
		return nil, nil
	}
	dStable = new(big.Int).Sub(rs.ToBig(), p.reserveStable.ToBig())
	dVolatile = new(big.Int).Sub(rv.ToBig(), p.reserveVolatile.ToBig())
	p.reserveStable, p.reserveVolatile = rs, rv
	p.Extra.Kappa = kappa
	p.Info.Reserves = []*big.Int{rs.ToBig(), rv.ToBig()}
	return dStable, dVolatile
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
