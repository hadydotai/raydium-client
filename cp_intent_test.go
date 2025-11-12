package main

import (
	"bytes"
	"math/big"
	"testing"

	"hadydotai/raydium-client/raydium_cp_swap"

	solana "github.com/gagliardetto/solana-go"
)

func newTestPoolState() (*raydium_cp_swap.PoolState, solana.PublicKey) {
	pk := func() solana.PublicKey { return solana.NewWallet().PublicKey() }
	pool := &raydium_cp_swap.PoolState{
		AmmConfig:      pk(),
		PoolCreator:    pk(),
		Token0Vault:    pk(),
		Token1Vault:    pk(),
		Token0Mint:     pk(),
		Token1Mint:     pk(),
		Token0Program:  pk(),
		Token1Program:  pk(),
		ObservationKey: pk(),
	}
	return pool, pk()
}

func newPoolBalance(amount int64, decimals uint8) *PoolBalance {
	return &PoolBalance{Balance: big.NewInt(amount), Decimals: decimals}
}

func mustSlippageRatio(t *testing.T, pct float64) *big.Rat {
	t.Helper()
	ratio, err := makeSlippageRatio(pct)
	if err != nil {
		t.Fatalf("slippage ratio: %v", err)
	}
	return ratio
}

func newIntentFixture(t *testing.T, dir SwapDir) (*CPIntent, *raydium_cp_swap.PoolState, solana.PublicKey, []*PoolBalance, *IntentInstruction, *big.Rat) {
	t.Helper()
	pool, poolAddr := newTestPoolState()
	balances := []*PoolBalance{
		newPoolBalance(1000, 0),
		newPoolBalance(2000, 0),
	}
	slippage := mustSlippageRatio(t, 1)
	cp := ConstantProduct{TradeFeeRate: 0, SlippageRatio: slippage}
	var (
		targetMint  solana.PublicKey
		instruction *IntentInstruction
	)
	switch dir {
	case SwapDirSell:
		targetMint = pool.Token0Mint
		instruction = &IntentInstruction{Verb: "sell", AmountStr: "10", Dir: SwapDirSell, TargetSymbol: "AAA"}
	case SwapDirBuy:
		targetMint = pool.Token1Mint
		instruction = &IntentInstruction{Verb: "buy", AmountStr: "10", Dir: SwapDirBuy, TargetSymbol: "BBB"}
	default:
		t.Fatalf("unsupported dir %v", dir)
	}
	intent, err := NewCPIntent(cp, pool, poolAddr, instruction, targetMint, balances...)
	if err != nil {
		t.Fatalf("building cp intent: %v", err)
	}
	return intent, pool, poolAddr, balances, instruction, slippage
}

func TestNewCPIntentErrors(t *testing.T) {
	pool, poolAddr := newTestPoolState()
	balances := []*PoolBalance{newPoolBalance(1000, 0), newPoolBalance(1000, 0)}
	cp := ConstantProduct{TradeFeeRate: 0, SlippageRatio: mustSlippageRatio(t, 0.5)}
	validInstr := &IntentInstruction{Verb: "sell", AmountStr: "1", Dir: SwapDirSell, TargetSymbol: "AAA"}
	nilEntry := []*PoolBalance{nil, balances[1]}
	nilBalance := []*PoolBalance{
		{Balance: nil, Decimals: 0},
		balances[1],
	}

	cases := []struct {
		name     string
		instr    *IntentInstruction
		pool     *raydium_cp_swap.PoolState
		poolAddr solana.PublicKey
		balances []*PoolBalance
		target   solana.PublicKey
	}{
		{"missing balance entry", validInstr, pool, poolAddr, nilEntry, pool.Token0Mint},
		{"nil balance value", validInstr, pool, poolAddr, nilBalance, pool.Token0Mint},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("unexpected panic: %v", r)
				}
			}()
			if _, err := NewCPIntent(cp, tc.pool, tc.poolAddr, tc.instr, tc.target, tc.balances...); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestNewCPIntentSell(t *testing.T) {
	intent, pool, poolAddr, balances, instruction, slippage := newIntentFixture(t, SwapDirSell)

	if intent.SwapKind != SwapKindBaseInput {
		t.Fatalf("expected base input, got %v", intent.SwapKind)
	}
	if intent.TokenIn.Mint != pool.Token0Mint {
		t.Fatalf("expected token in mint %s", pool.Token0Mint)
	}
	if intent.TokenOut.Mint != pool.Token1Mint {
		t.Fatalf("expected token out mint %s", pool.Token1Mint)
	}
	if intent.Pool.Address != poolAddr {
		t.Fatalf("pool address mismatch")
	}
	if intent.Pool.AmmConfig != pool.AmmConfig {
		t.Fatalf("amm config mismatch")
	}

	expectedKnown, err := fmtForMath(instruction.AmountStr, balances[0].Decimals)
	if err != nil {
		t.Fatalf("fmtForMath: %v", err)
	}
	if intent.Amounts.KnownAmount.Cmp(expectedKnown) != 0 {
		t.Fatalf("known amount mismatch: got %s want %s", intent.Amounts.KnownAmount, expectedKnown)
	}

	cp := ConstantProduct{TokenInReserve: balances[0], TokenOutReserve: balances[1], TradeFeeRate: 0}
	quote, err := cp.QuoteOut(expectedKnown)
	if err != nil {
		t.Fatalf("quote out: %v", err)
	}
	if intent.Amounts.QuoteAmount.Cmp(quote) != 0 {
		t.Fatalf("quote amount mismatch: got %s want %s", intent.Amounts.QuoteAmount, quote)
	}
	minOut, err := applySlippageFloor(quote, slippage)
	if err != nil {
		t.Fatalf("apply slippage floor: %v", err)
	}
	if intent.Amounts.MinAmountOut.Cmp(minOut) != 0 {
		t.Fatalf("min amount out mismatch: got %s want %s", intent.Amounts.MinAmountOut, minOut)
	}
	if intent.Amounts.MaxAmountIn != nil {
		t.Fatalf("max amount in should be nil for sell intents")
	}
}

func TestNewCPIntentBuy(t *testing.T) {
	intent, pool, _, balances, instruction, slippage := newIntentFixture(t, SwapDirBuy)

	if intent.SwapKind != SwapKindBaseOutput {
		t.Fatalf("expected base output, got %v", intent.SwapKind)
	}
	if intent.TokenIn.Mint != pool.Token0Mint {
		t.Fatalf("expected to pay token0")
	}
	if intent.TokenOut.Mint != pool.Token1Mint {
		t.Fatalf("expected to receive token1")
	}

	expectedKnown, err := fmtForMath(instruction.AmountStr, balances[1].Decimals)
	if err != nil {
		t.Fatalf("fmtForMath: %v", err)
	}
	if intent.Amounts.KnownAmount.Cmp(expectedKnown) != 0 {
		t.Fatalf("known amount mismatch: got %s want %s", intent.Amounts.KnownAmount, expectedKnown)
	}

	cp := ConstantProduct{TokenInReserve: balances[0], TokenOutReserve: balances[1], TradeFeeRate: 0}
	quote, err := cp.QuoteIn(expectedKnown)
	if err != nil {
		t.Fatalf("quote in: %v", err)
	}
	if intent.Amounts.QuoteAmount.Cmp(quote) != 0 {
		t.Fatalf("quote amount mismatch: got %s want %s", intent.Amounts.QuoteAmount, quote)
	}
	maxIn, err := applySlippageCeil(quote, slippage)
	if err != nil {
		t.Fatalf("apply slippage ceil: %v", err)
	}
	if intent.Amounts.MaxAmountIn.Cmp(maxIn) != 0 {
		t.Fatalf("max amount in mismatch: got %s want %s", intent.Amounts.MaxAmountIn, maxIn)
	}
	if intent.Amounts.MinAmountOut != nil {
		t.Fatalf("min amount out should be nil for buy intents")
	}
}

func TestCloneIntProducesCopy(t *testing.T) {
	original := big.NewInt(42)
	cloned := cloneInt(original)
	if cloned == original {
		t.Fatalf("clone returned same pointer")
	}
	original.SetInt64(100)
	if cloned.Int64() != 42 {
		t.Fatalf("clone changed when original mutated")
	}
}

func TestBuildSwapInstructionDiscriminators(t *testing.T) {
	sellIntent, _, _, _, _, _ := newIntentFixture(t, SwapDirSell)
	buyIntent, _, _, _, _, _ := newIntentFixture(t, SwapDirBuy)

	payer := solana.NewWallet().PublicKey()
	authority := solana.NewWallet().PublicKey()
	inputATA := solana.NewWallet().PublicKey()
	outputATA := solana.NewWallet().PublicKey()

	ix, err := sellIntent.BuildSwapInstruction(payer, authority, inputATA, outputATA)
	if err != nil {
		t.Fatalf("build base input: %v", err)
	}
	data, err := ix.Data()
	if err != nil {
		t.Fatalf("data fetch: %v", err)
	}
	if len(data) < len(raydium_cp_swap.Instruction_SwapBaseInput) || !bytes.Equal(data[:len(raydium_cp_swap.Instruction_SwapBaseInput)], raydium_cp_swap.Instruction_SwapBaseInput[:]) {
		t.Fatalf("expected swap base input discriminator")
	}

	ix, err = buyIntent.BuildSwapInstruction(payer, authority, inputATA, outputATA)
	if err != nil {
		t.Fatalf("build base output: %v", err)
	}
	data, err = ix.Data()
	if err != nil {
		t.Fatalf("data fetch: %v", err)
	}
	if len(data) < len(raydium_cp_swap.Instruction_SwapBaseOutput) || !bytes.Equal(data[:len(raydium_cp_swap.Instruction_SwapBaseOutput)], raydium_cp_swap.Instruction_SwapBaseOutput[:]) {
		t.Fatalf("expected swap base output discriminator")
	}
}

func TestBuildSwapInstructionValidation(t *testing.T) {
	sellIntent, _, _, _, _, _ := newIntentFixture(t, SwapDirSell)
	buyIntent, _, _, _, _, _ := newIntentFixture(t, SwapDirBuy)
	payer := solana.NewWallet().PublicKey()
	authority := solana.NewWallet().PublicKey()
	inputATA := solana.NewWallet().PublicKey()
	outputATA := solana.NewWallet().PublicKey()

	// Remove required amounts
	sellIntent.Amounts.MinAmountOut = nil
	if _, err := sellIntent.BuildSwapInstruction(payer, authority, inputATA, outputATA); err == nil {
		t.Fatalf("expected error when min amount missing for sell")
	}

	// Overflow amounts
	overflow := new(big.Int).Lsh(big.NewInt(1), 70)
	buyIntent.Amounts.MaxAmountIn = overflow
	if _, err := buyIntent.BuildSwapInstruction(payer, authority, inputATA, outputATA); err == nil {
		t.Fatalf("expected error when max amount exceeds uint64")
	}
}

func TestCounterLegSelection(t *testing.T) {
	sellIntent, _, _, _, _, _ := newIntentFixture(t, SwapDirSell)
	leg := sellIntent.CounterLeg()
	if leg == nil || leg != &sellIntent.TokenOut {
		t.Fatalf("counter leg for sell should be token out")
	}

	buyIntent, _, _, _, _, _ := newIntentFixture(t, SwapDirBuy)
	leg = buyIntent.CounterLeg()
	if leg == nil || leg != &buyIntent.TokenIn {
		t.Fatalf("counter leg for buy should be token in")
	}
}

func TestCounterLegUnknownSwapKind(t *testing.T) {
	intent := &CPIntent{}
	if leg := intent.CounterLeg(); leg != nil {
		t.Fatalf("expected nil counter leg for unknown swap kind")
	}
	if intent.Amounts.QuoteAmount != nil {
		t.Fatalf("expected nil quote amount")
	}
}

func TestRequiredInputAmount(t *testing.T) {
	sellIntent, _, _, _, _, _ := newIntentFixture(t, SwapDirSell)
	wantSell := sellIntent.Amounts.KnownAmount
	if got := sellIntent.RequiredInputAmount(); got.Cmp(wantSell) != 0 {
		t.Fatalf("sell required amount mismatch: got %s want %s", got, wantSell)
	}

	buyIntent, _, _, _, _, _ := newIntentFixture(t, SwapDirBuy)
	wantBuy := buyIntent.Amounts.MaxAmountIn
	if got := buyIntent.RequiredInputAmount(); got.Cmp(wantBuy) != 0 {
		t.Fatalf("buy required amount mismatch: got %s want %s", got, wantBuy)
	}
}

func TestParseIntent(t *testing.T) {
	intent, err := parseIntent("pay 1 sol")
	if err != nil {
		t.Fatalf("parse intent: %v", err)
	}
	if intent.Verb != "pay" || intent.AmountStr != "1" {
		t.Fatalf("unexpected intent fields: %#v", intent)
	}
	if intent.Dir != SwapDirSell {
		t.Fatalf("expected SwapDirSell, got %v", intent.Dir)
	}
	if intent.TargetSymbol != "SOL" {
		t.Fatalf("symbol should be uppercased, got %s", intent.TargetSymbol)
	}

	if _, err := parseIntent("pay onlytwo"); err == nil {
		t.Fatalf("expected error for invalid instruction format")
	}
	if _, err := parseIntent("foo 1 sol"); err == nil {
		t.Fatalf("expected error for unknown verb")
	}
	intent, err = parseIntent("\t buy   2    usdc   ")
	if err != nil {
		t.Fatalf("parse intent with whitespace: %v", err)
	}
	if intent.Verb != "buy" || intent.TargetSymbol != "USDC" {
		t.Fatalf("unexpected intent parsing for whitespace case: %#v", intent)
	}
}

func TestVerbToSwapDir(t *testing.T) {
	tests := []struct {
		verb string
		want SwapDir
		err  bool
	}{
		{"pay", SwapDirSell, false},
		{"sell", SwapDirSell, false},
		{"swap", SwapDirSell, false},
		{"buy", SwapDirBuy, false},
		{"get", SwapDirBuy, false},
		{"hold", SwapDirUnknown, true},
	}
	for _, tt := range tests {
		got, err := verbToSwapDir(tt.verb)
		if tt.err {
			if err == nil {
				t.Fatalf("expected error for verb %s", tt.verb)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error for verb %s: %v", tt.verb, err)
		}
		if got != tt.want {
			t.Fatalf("verb %s: got %v want %v", tt.verb, got, tt.want)
		}
	}
}
