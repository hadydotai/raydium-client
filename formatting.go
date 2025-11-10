package main

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

func fixedPointScale(decimals uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
}

func fmtForDisplay(raw *big.Int, decimals uint8, precision int) string {
	if raw == nil {
		return "0"
	}
	scale := fixedPointScale(decimals)
	rat := new(big.Rat).SetFrac(raw, scale)
	return rat.FloatString(precision)
}

func fmtForMath(amountStr string, decimals uint8) (*big.Int, error) {
	rat, ok := new(big.Rat).SetString(amountStr)
	if !ok {
		return nil, fmt.Errorf("the amount provided is an invalid decimal number: %q", amountStr)
	}
	if rat.Sign() <= 0 {
		return nil, errors.New("amount must be greater than zero")
	}
	scale := fixedPointScale(decimals)
	rat.Mul(rat, new(big.Rat).SetInt(scale))
	if !rat.IsInt() {
		return nil, fmt.Errorf("amount %s exceeds decimal precision of %d", amountStr, decimals)
	}
	return new(big.Int).Set(rat.Num()), nil
}

func formatFeeRate(ppm uint64) string {
	ratePct := new(big.Rat).SetFrac(big.NewInt(int64(ppm)), big.NewInt(feeRateDenom))
	ratePct.Mul(ratePct, big.NewRat(100, 1))
	formatted := ratePct.FloatString(6)
	formatted = strings.TrimRight(formatted, "0")
	formatted = strings.TrimSuffix(formatted, ".")
	if formatted == "" {
		formatted = "0"
	}
	return fmt.Sprintf("%s%%", formatted)
}

func formatPercent(p float64) string {
	str := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", p), "0"), ".")
	if str == "" {
		str = "0"
	}
	return fmt.Sprintf("%s%%", str)
}
