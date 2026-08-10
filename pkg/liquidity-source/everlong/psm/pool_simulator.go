package everlongpsm

import (
	"math/big"

	"github.com/goccy/go-json"
	"github.com/samber/lo"

	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/entity"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/source/pool"
	"github.com/KyberNetwork/kyberswap-dex-lib/pkg/util/bignumber"
)

// PoolSimulator prices one (debtToken, stable) pair of an Everlong PermissionlessPSM:
// 1:1 with decimal scaling (wadOffset), a bp fee from the fee hook, a per-stable mint
// cap on the deposit direction and the PSM's physical stable reserve plus the per-stable
// minted total bounding the redeem direction. Pure-integer port of PermissionlessPSM
// (previewDeposit / previewRedeem plus the execution-path cap and accounting checks).
//
// Token order: [0] = debtToken (e.g. NECT/EverUSD), [1] = stable (e.g. USDC).
type PoolSimulator struct {
	pool.Pool
	StaticExtra StaticExtra
	Extra       Extra
}

var (
	bigBp = big.NewInt(10_000)

	_ = pool.RegisterFactory0(DexType, NewPoolSimulator)
)

func NewPoolSimulator(p entity.Pool) (*PoolSimulator, error) {
	var extra Extra
	if err := json.Unmarshal([]byte(p.Extra), &extra); err != nil {
		return nil, err
	}
	var staticExtra StaticExtra
	if err := json.Unmarshal([]byte(p.StaticExtra), &staticExtra); err != nil {
		return nil, err
	}
	return &PoolSimulator{
		Pool: pool.Pool{Info: pool.PoolInfo{
			Address:     p.Address,
			Exchange:    p.Exchange,
			Type:        p.Type,
			Tokens:      lo.Map(p.Tokens, func(e *entity.PoolToken, _ int) string { return e.Address }),
			Reserves:    lo.Map(p.Reserves, func(e string, _ int) *big.Int { return bignumber.NewBig(e) }),
			BlockNumber: p.BlockNumber,
		}},
		StaticExtra: staticExtra,
		Extra:       extra,
	}, nil
}

func (s *PoolSimulator) CalcAmountOut(params pool.CalcAmountOutParams) (*pool.CalcAmountOutResult, error) {
	indexIn, indexOut := s.GetTokenIndex(params.TokenAmountIn.Token), s.GetTokenIndex(params.TokenOut)
	if indexIn < 0 || indexOut < 0 || indexIn == indexOut {
		return nil, ErrInvalidToken
	}
	amountIn := params.TokenAmountIn.Amount
	if amountIn == nil || amountIn.Sign() <= 0 {
		return nil, ErrInvalidAmountIn
	}
	if s.Extra.Paused {
		return nil, ErrPaused
	}
	if s.StaticExtra.WadOffset == nil || s.StaticExtra.WadOffset.Sign() <= 0 {
		return nil, ErrNotListed
	}

	if indexIn == 1 { // stable -> debt: deposit
		return s.calcDeposit(params, amountIn)
	}
	return s.calcRedeem(params, amountIn) // debt -> stable: redeem
}

// calcDeposit ports previewDeposit + the execution cap check: gross = in * wadOffset,
// fee = ceil(gross * bp / 1e4) minted to the fee receiver, out = gross - fee, and
// minted + gross must stay within the cap — the input is clamped to the cap room so an
// oversized order partial-fills instead of reverting.
func (s *PoolSimulator) calcDeposit(params pool.CalcAmountOutParams,
	amountIn *big.Int) (*pool.CalcAmountOutResult, error) {
	e, w := &s.Extra, s.StaticExtra.WadOffset

	var capRoom big.Int
	capRoom.Sub(e.MintCap, e.DebtTokenMinted)
	if capRoom.Sign() <= 0 {
		return nil, ErrCapExhausted
	}
	// largest stable input whose gross fits the cap room
	maxIn := new(big.Int).Quo(&capRoom, w)
	if maxIn.Sign() == 0 {
		return nil, ErrCapExhausted
	}
	used := amountIn
	if amountIn.Cmp(maxIn) > 0 {
		used = maxIn
	}

	var gross big.Int
	gross.Mul(used, w)
	fee := feeOnRawUp(&gross, e.EntryFeeBp)
	out := new(big.Int).Sub(&gross, fee)
	if out.Sign() <= 0 {
		return nil, ErrZeroAmountOut
	}

	var remaining big.Int
	remaining.Sub(amountIn, used)
	return &pool.CalcAmountOutResult{
		TokenAmountOut:         &pool.TokenAmount{Token: params.TokenOut, Amount: out},
		Fee:                    &pool.TokenAmount{Token: params.TokenOut, Amount: fee},
		RemainingTokenAmountIn: &pool.TokenAmount{Token: params.TokenAmountIn.Token, Amount: &remaining},
		Gas:                    s.gasFor(true),
		SwapInfo: SwapInfo{
			IsDeposit:   true,
			GrossDebt:   &gross,
			GrossStable: used,
		},
	}, nil
}

// calcRedeem ports previewRedeem + the execution accounting: gross = in / wadOffset
// (floor — sub-offset dust would burn for nothing, so the unusable remainder is
// returned instead), fee = ceil(gross * bp / 1e4) paid in stable, out = gross - fee.
// The burn cannot exceed debtTokenMinted[stable] and the stable leaving cannot exceed
// the PSM's reserve; the input is clamped to both so oversized orders partial-fill.
func (s *PoolSimulator) calcRedeem(params pool.CalcAmountOutParams,
	amountIn *big.Int) (*pool.CalcAmountOutResult, error) {
	e, w := &s.Extra, s.StaticExtra.WadOffset

	if e.DebtTokenMinted.Sign() == 0 || e.StableReserve.Sign() == 0 {
		return nil, ErrNothingToRedeem
	}
	// burn bound and reserve bound (gross stable leaving = out + fee = in/w floor)
	maxIn := new(big.Int).Set(e.DebtTokenMinted)
	var reserveBound big.Int
	reserveBound.Mul(e.StableReserve, w) // any in <= reserve*w has gross <= reserve
	if reserveBound.Cmp(maxIn) < 0 {
		maxIn = &reserveBound
	}
	used := new(big.Int).Set(amountIn)
	if used.Cmp(maxIn) > 0 {
		used.Set(maxIn)
	}
	// floor to the offset so no debt burns for zero stable
	var dust big.Int
	dust.Mod(used, w)
	used.Sub(used, &dust)
	if used.Sign() == 0 {
		return nil, ErrZeroAmountOut
	}

	gross := new(big.Int).Quo(used, w)
	fee := feeOnRawUp(gross, e.ExitFeeBp)
	out := new(big.Int).Sub(gross, fee)
	if out.Sign() <= 0 {
		return nil, ErrZeroAmountOut
	}

	var remaining big.Int
	remaining.Sub(amountIn, used)
	return &pool.CalcAmountOutResult{
		TokenAmountOut:         &pool.TokenAmount{Token: params.TokenOut, Amount: out},
		Fee:                    &pool.TokenAmount{Token: params.TokenOut, Amount: fee},
		RemainingTokenAmountIn: &pool.TokenAmount{Token: params.TokenAmountIn.Token, Amount: &remaining},
		Gas:                    s.gasFor(false),
		SwapInfo: SwapInfo{
			IsDeposit:   false,
			GrossDebt:   used,
			GrossStable: gross,
		},
	}, nil
}

// feeOnRawUp = FeeLib.feeOnRaw: ceil(amount * feeBp / 1e4).
func feeOnRawUp(amount, feeBp *big.Int) *big.Int {
	if feeBp == nil || feeBp.Sign() == 0 {
		return new(big.Int)
	}
	var p, m big.Int
	p.Mul(amount, feeBp)
	p.QuoRem(&p, bigBp, &m)
	if m.Sign() != 0 {
		p.Add(&p, big.NewInt(1))
	}
	return &p
}

// UpdateBalance replays the exact fill from SwapInfo — never recomputes swap results.
func (s *PoolSimulator) UpdateBalance(params pool.UpdateBalanceParams) {
	si, ok := params.SwapInfo.(SwapInfo)
	if !ok {
		return
	}
	e := &s.Extra
	if si.IsDeposit {
		e.DebtTokenMinted = new(big.Int).Add(e.DebtTokenMinted, si.GrossDebt)
		e.StableReserve = new(big.Int).Add(e.StableReserve, si.GrossStable)
	} else {
		e.DebtTokenMinted = new(big.Int).Sub(e.DebtTokenMinted, si.GrossDebt)
		e.StableReserve = new(big.Int).Sub(e.StableReserve, si.GrossStable)
	}
	capRoom := new(big.Int).Sub(e.MintCap, e.DebtTokenMinted)
	if capRoom.Sign() < 0 {
		capRoom = new(big.Int)
	}
	s.Info.Reserves = []*big.Int{capRoom, new(big.Int).Set(e.StableReserve)}
}

func (s *PoolSimulator) CloneState() pool.IPoolSimulator {
	cloned := *s
	cloned.Info.Reserves = lo.Map(s.Info.Reserves, func(r *big.Int, _ int) *big.Int {
		return new(big.Int).Set(r)
	})
	// UpdateBalance reassigns Extra's pointers wholesale (copy-on-write), so the value
	// copy of Extra above already insulates the clone.
	return &cloned
}

func (s *PoolSimulator) GetMetaInfo(_, _ string) any {
	return PoolMeta{PSM: s.StaticExtra.PSM}
}

// GetApprovalAddress: deposits transferFrom the stable; redeems burn straight from the
// caller (the PSM holds burn rights on the debt token), needing no approval.
func (s *PoolSimulator) GetApprovalAddress(tokenIn, _ string) string {
	if s.GetTokenIndex(tokenIn) == 1 {
		return s.StaticExtra.PSM
	}
	return ""
}

func (s *PoolSimulator) gasFor(deposit bool) int64 {
	if deposit {
		if s.StaticExtra.GasDeposit != 0 {
			return s.StaticExtra.GasDeposit
		}
		return defaultGasDeposit
	}
	if s.StaticExtra.GasRedeem != 0 {
		return s.StaticExtra.GasRedeem
	}
	return defaultGasRedeem
}
