package main

import (
	"context"
	"errors"
	"fmt"
	"hadydotai/raydium-client/raydium_cp_swap"
	"strings"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jedib0t/go-pretty/v6/table"
)

type TableBuilder struct {
	ctx           context.Context
	client        *rpc.Client
	pool          *raydium_cp_swap.PoolState
	poolAmmConfig *raydium_cp_swap.AmmConfig
	targetAddr    Addr
	poolAddress   string
}

func (tb *TableBuilder) Build(intentLine string) (string, *IntentInstruction, error) {
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
	t.AppendRow(table.Row{"Addr", Addr(tb.pool.Token0Mint.String()), Addr(tb.pool.Token1Mint.String())})

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
	tradeFeeRow := formatFeeRate(tb.poolAmmConfig.TradeFeeRate)
	t.AppendRow(table.Row{"Trade fee", tradeFeeRow, tradeFeeRow})

	cp := ConstantProduct{TradeFeeRate: tb.poolAmmConfig.TradeFeeRate}
	intentMeta, intentErr := cp.DoIntent(intentLine, tb.pool, tb.targetAddr, balances...)
	intentRow := table.Row{"Intent", "", ""}
	targetTokenCell := 0
	if tb.targetAddr == Addr(tb.pool.Token1Mint.String()) {
		targetTokenCell = 1
	}
	counterTokenCell := 1 - targetTokenCell

	if intentErr != nil {
		errMsg := fmt.Sprintf("intent failed: %s", intentErr)
		if intentMeta != nil {
			errMsg = fmt.Sprintf("%s %s failed: %s", intentMeta.Verb, intentMeta.AmountStr, intentErr)
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
	intentText := fmt.Sprintf("%s %s", intentMeta.Verb, intentMeta.AmountStr)

	switch intentMeta.Dir {
	case SwapDirSell:
		intentRow[targetTokenCell+1] = intentText
		intentRow[counterTokenCell+1] = fmt.Sprintf("receiving %s", counterTokenAmount)
	case SwapDirBuy:
		intentRow[targetTokenCell+1] = intentText
		intentRow[counterTokenCell+1] = fmt.Sprintf("paying %s", counterTokenAmount)
	default:
		panic("shouldn't be here, did we miss an early return checking for verbToSwapDir error value?")
	}
	t.AppendRow(intentRow)
	t.Render()
	return builder.String(), intentMeta, nil
}
