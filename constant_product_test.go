package main

import (
	"math/big"
	"testing"
)

func newConstantProduct(reserveIn, reserveOut int64, tradeFee uint64) ConstantProduct {
	return ConstantProduct{
		TokenInReserve:  &PoolBalance{Balance: big.NewInt(reserveIn), Decimals: 0},
		TokenOutReserve: &PoolBalance{Balance: big.NewInt(reserveOut), Decimals: 0},
		TradeFeeRate:    tradeFee,
	}
}

func TestQuoteOut(t *testing.T) {
	cp := newConstantProduct(1000, 2000, 3000)
	amountIn := big.NewInt(100)
	got, err := cp.QuoteOut(amountIn)
	if err != nil {
		t.Fatalf("QuoteOut failed: %v", err)
	}
	want := big.NewInt(181)
	if got.Cmp(want) != 0 {
		t.Fatalf("QuoteOut mismatch: got %s want %s", got, want)
	}
}

func TestQuoteOutErrors(t *testing.T) {
	cp := newConstantProduct(1000, 2000, 0)
	if _, err := cp.QuoteOut(nil); err == nil {
		t.Fatalf("expected error for nil amount")
	}
	if _, err := cp.QuoteOut(big.NewInt(0)); err == nil {
		t.Fatalf("expected error for zero amount")
	}
	cp = newConstantProduct(10, 5, 0)
	if _, err := cp.QuoteOut(big.NewInt(10_000)); err == nil {
		t.Fatalf("expected error when requesting more than reserve")
	}
}

func TestQuoteIn(t *testing.T) {
	cp := newConstantProduct(1000, 2000, 3000)
	amountOut := big.NewInt(200)
	got, err := cp.QuoteIn(amountOut)
	if err != nil {
		t.Fatalf("QuoteIn failed: %v", err)
	}
	want := big.NewInt(112)
	if got.Cmp(want) != 0 {
		t.Fatalf("QuoteIn mismatch: got %s want %s", got, want)
	}
}

func TestQuoteInErrors(t *testing.T) {
	cp := newConstantProduct(1000, 2000, 0)
	if _, err := cp.QuoteIn(nil); err == nil {
		t.Fatalf("expected error for nil amount")
	}
	if _, err := cp.QuoteIn(big.NewInt(0)); err == nil {
		t.Fatalf("expected error for zero amount")
	}
	if _, err := cp.QuoteIn(big.NewInt(2000)); err == nil {
		t.Fatalf("expected error when draining reserve")
	}
}

func TestAmountAfterTradeFee(t *testing.T) {
	cp := ConstantProduct{TradeFeeRate: 2500}
	amount := big.NewInt(1_000_000)
	got, err := cp.amountAfterTradeFee(amount)
	if err != nil {
		t.Fatalf("amountAfterTradeFee failed: %v", err)
	}
	want := big.NewInt(997_500)
	if got.Cmp(want) != 0 {
		t.Fatalf("amountAfterTradeFee mismatch: got %s want %s", got, want)
	}
}

func TestAmountAfterTradeFeeErrors(t *testing.T) {
	cp := ConstantProduct{TradeFeeRate: 0}
	if _, err := cp.amountAfterTradeFee(nil); err == nil {
		t.Fatalf("expected error for nil amount")
	}
}

func TestAmountBeforeTradeFee(t *testing.T) {
	cp := ConstantProduct{TradeFeeRate: 2500}
	net := big.NewInt(997_500)
	got, err := cp.amountBeforeTradeFee(net)
	if err != nil {
		t.Fatalf("amountBeforeTradeFee failed: %v", err)
	}
	want := big.NewInt(1_000_000)
	if got.Cmp(want) != 0 {
		t.Fatalf("amountBeforeTradeFee mismatch: got %s want %s", got, want)
	}
}

func TestAmountBeforeTradeFeeErrors(t *testing.T) {
	cp := ConstantProduct{TradeFeeRate: 0}
	if _, err := cp.amountBeforeTradeFee(nil); err == nil {
		t.Fatalf("expected error for nil amount")
	}
	if _, err := cp.amountBeforeTradeFee(big.NewInt(0)); err == nil {
		t.Fatalf("expected error for zero net amount")
	}
}

func TestApplySlippageFloor(t *testing.T) {
	amount := big.NewInt(1000)
	ratio := big.NewRat(1, 100) // 1%
	got, err := applySlippageFloor(amount, ratio)
	if err != nil {
		t.Fatalf("applySlippageFloor failed: %v", err)
	}
	want := big.NewInt(990)
	if got.Cmp(want) != 0 {
		t.Fatalf("applySlippageFloor mismatch: got %s want %s", got, want)
	}
}

func TestApplySlippageFloorEdgeCases(t *testing.T) {
	amount := big.NewInt(1000)
	got, err := applySlippageFloor(amount, nil)
	if err != nil {
		t.Fatalf("unexpected error when ratio nil: %v", err)
	}
	if got.Cmp(amount) != 0 {
		t.Fatalf("expected unchanged amount when ratio nil")
	}
	if _, err := applySlippageFloor(nil, big.NewRat(1, 10)); err == nil {
		t.Fatalf("expected error for nil amount")
	}
}

func TestApplySlippageCeil(t *testing.T) {
	amount := big.NewInt(1000)
	ratio := big.NewRat(1, 100) // 1%
	got, err := applySlippageCeil(amount, ratio)
	if err != nil {
		t.Fatalf("applySlippageCeil failed: %v", err)
	}
	want := big.NewInt(1010)
	if got.Cmp(want) != 0 {
		t.Fatalf("applySlippageCeil mismatch: got %s want %s", got, want)
	}
}

func TestApplySlippageCeilEdgeCases(t *testing.T) {
	amount := big.NewInt(1000)
	got, err := applySlippageCeil(amount, nil)
	if err != nil {
		t.Fatalf("unexpected error when ratio nil: %v", err)
	}
	if got.Cmp(amount) != 0 {
		t.Fatalf("expected unchanged amount when ratio nil")
	}
	if _, err := applySlippageCeil(nil, big.NewRat(1, 10)); err == nil {
		t.Fatalf("expected error for nil amount")
	}
}

func TestMakeSlippageRatioBounds(t *testing.T) {
	if _, err := makeSlippageRatio(-0.1); err == nil {
		t.Fatalf("expected error for negative percent")
	}
	if _, err := makeSlippageRatio(100); err == nil {
		t.Fatalf("expected error for percent >= 100")
	}
	if ratio, err := makeSlippageRatio(0); err != nil || ratio.Sign() != 0 {
		t.Fatalf("expected zero ratio for zero percent")
	}
}

func TestTradeFeeNumeratorBounds(t *testing.T) {
	cp := ConstantProduct{TradeFeeRate: uint64(feeRateDenom)}
	if _, err := cp.tradeFeeNumerator(); err == nil {
		t.Fatalf("expected error when trade fee exceeds denom")
	}
}
