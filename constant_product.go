package main

import (
	"errors"
	"fmt"
	"math/big"
)

type SwapDir uint8

const (
	SwapDirUnknown SwapDir = iota
	SwapDirBuy
	SwapDirSell
)

type ConstantProduct struct {
	TokenInReserve  *PoolBalance
	TokenOutReserve *PoolBalance
	TradeFeeRate    uint64
	SlippageRatio   *big.Rat
	SlippagePct     float64
}

func (cp ConstantProduct) tradeFeeNumerator() (int64, error) {
	if cp.TradeFeeRate >= uint64(feeRateDenom) {
		// NOTE(@hadydotai): No other place to cover this issue of `feeRateDenom` potentially going out of sync
		// with the contract. It's a hard coded value after all. I wonder if they expose it on chain.
		return 0, fmt.Errorf("trade fee rate %d is invalid", cp.TradeFeeRate)
	}
	return feeRateDenom - int64(cp.TradeFeeRate), nil
}

func (cp ConstantProduct) amountAfterTradeFee(amount *big.Int) (*big.Int, error) {
	if amount == nil {
		return nil, errors.New("amount cannot be nil when applying trade fee")
	}
	numerator, err := cp.tradeFeeNumerator()
	if err != nil {
		return nil, err
	}
	net := new(big.Int).Mul(amount, big.NewInt(numerator))
	net.Quo(net, big.NewInt(feeRateDenom))
	if net.Sign() <= 0 {
		return nil, errors.New("amount becomes zero after applying trade fee")
	}
	return net, nil
}

func (cp ConstantProduct) amountBeforeTradeFee(net *big.Int) (*big.Int, error) {
	if net == nil {
		return nil, errors.New("amount cannot be nil when removing trade fee")
	}
	numerator, err := cp.tradeFeeNumerator()
	if err != nil {
		return nil, err
	}
	if net.Sign() <= 0 {
		return nil, errors.New("amount must be greater than zero when removing trade fee")
	}
	grossNumerator := new(big.Int).Mul(net, big.NewInt(feeRateDenom))
	divisor := big.NewInt(numerator)
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(grossNumerator, divisor, remainder)
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if quotient.Sign() <= 0 {
		return nil, errors.New("trade would not require a positive input amount")
	}
	return quotient, nil
}

// QuoteOut (selling) takes an amount in TokenIn and will produce an amount in TokenOut: amountIn -> amountOut
// In layman's terms, this figures out how much we get out of the pool, provided the amount we put into the pool.
// All amounts are in their respective tokens of course.
// NOTE: Fee is applied directly on the amountIn before being added to the reserve
func (cp ConstantProduct) QuoteOut(amountIn *big.Int) (*big.Int, error) {
	// Some defensive house keeping. Go, you're killing me.
	if amountIn == nil || amountIn.Sign() <= 0 {
		return nil, errors.New("amount in must be greater than zero")
	}
	if cp.TokenInReserve == nil || cp.TokenOutReserve == nil {
		return nil, errors.New("pool reserves unavailable for quote out")
	}
	if cp.TokenInReserve.Balance == nil || cp.TokenOutReserve.Balance == nil {
		return nil, errors.New("pool balances unavailable for quote out")
	}
	if cp.TokenInReserve.Balance.Sign() <= 0 || cp.TokenOutReserve.Balance.Sign() <= 0 {
		return nil, errors.New("pool reserves must be greater than zero for quote out")
	}

	// X, Y initial reserves
	// pre-swap:  	K = X*Y
	// post-swap: 	K = (X + dX) * (Y - dY)
	// 				X * Y = (X + dX) * (Y - dY)
	//						^
	//						|{updatedReserveIn}
	// 					=> we need to isolate dY, so we'll divide by (X + dX) on both sides
	//				(X * Y) / (X + dX) = ((X + dX) * (Y - dY)) / (X + dX)
	//					=> (X + dX) rhs will cancel each other
	//				(X * Y) / (X + dX) 	= (Y - dY)
	//				~~~~~~~~~~~~~~~~~~
	//				^
	//				|{newReserveOut}
	//					=> our target amountOut will be subtracting initial reserve from the new reserve
	//				Y - {newReserveOut} = dY
	//	science.
	reserveIn, reserveOut := cp.TokenInReserve.Balance, cp.TokenOutReserve.Balance
	netAmountIn, err := cp.amountAfterTradeFee(amountIn)
	if err != nil {
		return nil, err
	}
	constantProductK := new(big.Int).Mul(reserveIn, reserveOut)
	updatedReserveIn := new(big.Int).Add(reserveIn, netAmountIn)
	newReserveOut := new(big.Int).Quo(constantProductK, updatedReserveIn)
	amountOut := new(big.Int).Sub(reserveOut, newReserveOut)
	if amountOut.Sign() <= 0 {
		// NOTE(@hadydotai): we'd only end up here if we math our way into draining the pool on one side,
		// I think this should be a flat out error and yell at the user for it, maybe?
		return nil, errors.New("trade would not yield a positive output amount")
	}
	if amountOut.Cmp(reserveOut) >= 0 {
		available := fmtForDisplay(reserveOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
		return nil, fmt.Errorf("requested output would exceed available %s liquidity", available)
	}
	return amountOut, nil
}

// QuoteIn (buying) takes an amount in TokenOut and will produce an amount in TokenIn: amountOut -> amountIn
// In layman's terms, this figures out how much we need to give, to match the amount we want out of the pool.
// All amounts are in their respective tokens of course
// NOTE: To work out the fee here, we work backwards. First we get the net input we'd need to receive the asking amount
// then we apply the fee
func (cp ConstantProduct) QuoteIn(amountOut *big.Int) (*big.Int, error) {
	// Some defensive house keeping. Go, you're killing me, but a little reptition won't kill you.
	if amountOut == nil || amountOut.Sign() <= 0 {
		return nil, errors.New("amount out must be greater than zero")
	}
	if cp.TokenInReserve == nil || cp.TokenOutReserve == nil {
		return nil, errors.New("pool reserves unavailable for quote in")
	}
	if cp.TokenInReserve.Balance == nil || cp.TokenOutReserve.Balance == nil {
		return nil, errors.New("pool balances unavailable for quote in")
	}
	if cp.TokenInReserve.Balance.Sign() <= 0 || cp.TokenOutReserve.Balance.Sign() <= 0 {
		return nil, errors.New("pool reserves must be greater than zero for quote in")
	}
	// X, Y initial reserves
	// pre-swap:  	K = X*Y
	// post-swap: 	K = (X + dX) * (Y - dY)
	// 				X * Y = (X + dX) * (Y - dY)
	//									^
	//									|{updatedReserveOut}
	// 					=> we need to isolate dX, so we'll divide by (Y - dY) on both sides
	//				(X * Y) / (Y - dY) = ((X + dX) * (Y - dY)) / (Y - dY)
	//					=> (Y - dY) rhs will cancel each other
	//				(X * Y) / (Y - dY) 	= (X + dX)
	//				~~~~~~~~~~~~~~~~~~
	//				^
	//				|{newReserveIn}
	//					=> our target amountIn will
	//				{newReserveIn} - X = dX
	//	science.

	reserveIn, reserveOut := cp.TokenInReserve.Balance, cp.TokenOutReserve.Balance
	constantProductK := new(big.Int).Mul(reserveIn, reserveOut)

	if amountOut.Cmp(reserveOut) >= 0 {
		requested := fmtForDisplay(amountOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
		available := fmtForDisplay(reserveOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
		return nil, fmt.Errorf("requested %s exceeds available %s liquidity", requested, available)
	}
	updatedReserveOut := new(big.Int).Sub(reserveOut, amountOut)
	newReserveIn := new(big.Int).Quo(constantProductK, updatedReserveOut)
	netAmountIn := new(big.Int).Sub(newReserveIn, reserveIn)
	if netAmountIn.Sign() <= 0 {
		// NOTE(@hadydotai): we'd only end up here if we math our way into draining the pool on one side,
		// I think this should be a flat out error and yell at the user for it, maybe?
		return nil, errors.New("trade would not require a positive input amount")
	}
	grossAmountIn, err := cp.amountBeforeTradeFee(netAmountIn)
	if err != nil {
		return nil, err
	}
	return grossAmountIn, nil
}

func makeSlippageRatio(percent float64) (*big.Rat, error) {
	if percent < 0 {
		return nil, fmt.Errorf("slippage percent must be >= 0")
	}
	if percent >= 100 {
		return nil, fmt.Errorf("slippage percent must be less than 100")
	}
	if percent == 0 {
		return big.NewRat(0, 1), nil
	}
	ratio := new(big.Rat).SetFloat64(percent / 100)
	return ratio, nil
}

func applySlippageFloor(amount *big.Int, ratio *big.Rat) (*big.Int, error) {
	if amount == nil {
		return nil, errors.New("amount cannot be nil for slippage calculation")
	}
	if ratio == nil || ratio.Sign() == 0 {
		return new(big.Int).Set(amount), nil
	}
	factor := new(big.Rat).Sub(big.NewRat(1, 1), ratio)
	if factor.Sign() <= 0 {
		return nil, errors.New("slippage factor must be positive")
	}
	num := new(big.Int).Mul(amount, factor.Num())
	den := factor.Denom()
	result := new(big.Int).Quo(num, den)
	return result, nil
}

func applySlippageCeil(amount *big.Int, ratio *big.Rat) (*big.Int, error) {
	if amount == nil {
		return nil, errors.New("amount cannot be nil for slippage calculation")
	}
	if ratio == nil || ratio.Sign() == 0 {
		return new(big.Int).Set(amount), nil
	}
	factor := new(big.Rat).Add(big.NewRat(1, 1), ratio)
	num := new(big.Int).Mul(amount, factor.Num())
	den := factor.Denom()
	result, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() > 0 {
		result.Add(result, big.NewInt(1))
	}
	return result, nil
}
