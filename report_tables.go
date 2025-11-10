package main

import (
	"context"
	"errors"
	"fmt"
	"hadydotai/raydium-client/raydium_cp_swap"
	"math/big"
	"sort"
	"strings"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

type TableBuilder struct {
	ctx           context.Context
	client        *rpc.Client
	pool          *raydium_cp_swap.PoolState
	poolAmmConfig *raydium_cp_swap.AmmConfig
	poolAddress   string
	slippagePct   float64
	slippageRat   *big.Rat
	tokenOrder    []solana.PublicKey
	mintToSymbol  map[string]string
	symbolToMint  map[string]solana.PublicKey
}

func (tb *TableBuilder) SetSlippagePct(pct float64) error {
	rat, err := makeSlippageRatio(pct)
	if err != nil {
		return err
	}
	tb.slippagePct = pct
	tb.slippageRat = rat
	return nil
}

func (tb *TableBuilder) slippage() (float64, *big.Rat) {
	var ratioCopy *big.Rat
	if tb.slippageRat != nil {
		ratioCopy = new(big.Rat).Set(tb.slippageRat)
	} else {
		ratioCopy = big.NewRat(0, 1)
	}
	return tb.slippagePct, ratioCopy
}

func (tb *TableBuilder) symbolForMint(m solana.PublicKey) string {
	if tb.mintToSymbol == nil {
		return Addr(m.String()).String()
	}
	if sym, ok := tb.mintToSymbol[m.String()]; ok && sym != "" {
		return sym
	}
	return Addr(m.String()).String()
}

func (tb *TableBuilder) availableSymbols() []string {
	syms := make([]string, 0, len(tb.symbolToMint))
	for sym := range tb.symbolToMint {
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	return syms
}

func (tb *TableBuilder) resolveMintForSymbol(symbol string) (solana.PublicKey, error) {
	if tb.symbolToMint == nil {
		return solana.PublicKey{}, errors.New("token directory unavailable")
	}
	symKey := strings.ToUpper(strings.TrimSpace(symbol))
	if symKey == "" {
		return solana.PublicKey{}, errors.New("token symbol cannot be empty")
	}
	if mint, ok := tb.symbolToMint[symKey]; ok {
		return mint, nil
	}
	return solana.PublicKey{}, fmt.Errorf("unknown token symbol %s (available: %s)", symbol, strings.Join(tb.availableSymbols(), ", "))
}

func (tb *TableBuilder) Build(intentLine string) (string, *IntentInstruction, error) {
	instruction, err := parseIntent(intentLine)
	if err != nil {
		return "", nil, err
	}
	targetMint, err := tb.resolveMintForSymbol(instruction.TargetSymbol)
	if err != nil {
		return "", nil, err
	}
	instruction.TargetSymbol = tb.symbolForMint(targetMint)

	// TODO(@hadydotai):BUG: This is a problem, poolBalances always creates slices of an exact size, so len(balances) will
	// actually never be zero, it's not a signal for errors. In fact, neither are, balances and errs have a length.
	// They might be zeroed out, but they're preallocated and have a length.
	balances, errs := poolBalances(tb.ctx, tb.client, []solana.PublicKey{tb.pool.Token0Vault, tb.pool.Token1Vault})
	if len(balances) == 0 {
		return "", nil, errors.New("no balances available for pool")
	}
	builder := &strings.Builder{}
	t := table.NewWriter()
	t.SetOutputMirror(builder)
	t.SetTitle(tb.poolAddress)
	t.SetCaption("CPMM/CP-Swap Raydium Pool")
	t.Style().Size.WidthMax = 120
	t.AppendHeader(table.Row{"", "Token 0", "Token 1"})
	t.AppendRow(table.Row{"Symbol", tb.symbolForMint(tb.pool.Token0Mint), tb.symbolForMint(tb.pool.Token1Mint)})

	balancesDisplay := make([]any, len(balances)+1)
	balancesDisplay[0] = "Balances"
	for i := range balances {
		if i < len(errs) && errs[i] != nil {
			balancesDisplay[i+1] = errs[i].Error()
			continue
		}
		if balances[i] == nil || balances[i].Balance == nil {
			balancesDisplay[i+1] = "n/a"
			continue
		}
		balancesDisplay[i+1] = humanAmount(balances[i].Balance, balances[i].Decimals, int(balances[i].Decimals))
	}
	t.AppendRow(balancesDisplay)

	decimals := []any{"Decimals"}
	for _, bal := range balances {
		if bal == nil {
			decimals = append(decimals, "n/a")
			continue
		}
		decimals = append(decimals, bal.Decimals)
	}
	t.AppendRow(decimals)
	t.AppendSeparator()
	tradeFeeRow := formatFeeRate(tb.poolAmmConfig.TradeFeeRate)
	t.AppendRow(table.Row{"Trade fee", tradeFeeRow, tradeFeeRow})
	slippagePct, slippageRat := tb.slippage()
	slippageRow := formatPercent(slippagePct)
	t.AppendRow(table.Row{"Slippage", slippageRow, slippageRow}, table.RowConfig{AutoMerge: true, AutoMergeAlign: text.AlignLeft})

	t.AppendSeparator()
	cp := ConstantProduct{TradeFeeRate: tb.poolAmmConfig.TradeFeeRate, SlippageRatio: slippageRat, SlippagePct: slippagePct}
	intentMeta, intentErr := cp.DoIntent(instruction, tb.pool, targetMint, balances...)
	intentRow := table.Row{"Intent", "", ""}
	targetTokenCell := 0
	if targetMint.Equals(tb.pool.Token1Mint) {
		targetTokenCell = 1
	}
	counterTokenCell := 1 - targetTokenCell
	counterMint := tb.pool.Token1Mint
	if targetTokenCell == 1 {
		counterMint = tb.pool.Token0Mint
	}

	if intentErr != nil {
		errMsg := fmt.Sprintf("intent failed: %s", intentErr)
		if intentMeta != nil {
			errMsg = fmt.Sprintf("%s %s %s failed: %s", intentMeta.Verb, intentMeta.AmountStr, instruction.TargetSymbol, intentErr)
		}
		intentRow[targetTokenCell+1] = errMsg
		intentRow[counterTokenCell+1] = errMsg
		t.AppendRow(intentRow, table.RowConfig{AutoMerge: true})
		t.Render()
		return builder.String(), intentMeta, nil
	}

	var counterTokenDecimals uint8
	if counterTokenCell < len(balances) && balances[counterTokenCell] != nil {
		counterTokenDecimals = balances[counterTokenCell].Decimals
	}
	counterTokenAmount := humanAmount(intentMeta.Amount, counterTokenDecimals, int(counterTokenDecimals))
	intentText := fmt.Sprintf("%s %s %s", intentMeta.Verb, intentMeta.AmountStr, instruction.TargetSymbol)
	counterSymbol := tb.symbolForMint(counterMint)

	switch intentMeta.Dir {
	case SwapDirSell:
		intentRow[targetTokenCell+1] = intentText
		intentRow[counterTokenCell+1] = fmt.Sprintf("receiving %s %s", counterTokenAmount, counterSymbol)
	case SwapDirBuy:
		intentRow[targetTokenCell+1] = intentText
		intentRow[counterTokenCell+1] = fmt.Sprintf("paying %s %s", counterTokenAmount, counterSymbol)
	default:
		panic("shouldn't be here, did we miss an early return checking for verbToSwapDir error value?")
	}
	t.AppendRow(intentRow)
	t.Render()
	return builder.String(), intentMeta, nil
}
