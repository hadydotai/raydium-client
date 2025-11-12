package main

import (
	"context"
	"errors"
	"fmt"
	"hadydotai/raydium-client/raydium_cp_swap"
	"math/big"
	"strings"
	"sync"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/jedib0t/go-pretty/v6/text"
)

type TableBuilder struct {
	ctx               context.Context
	client            *rpc.Client
	pool              *raydium_cp_swap.PoolState
	poolAmmConfig     *raydium_cp_swap.AmmConfig
	poolAddress       string
	poolPubKey        solana.PublicKey
	slippagePct       float64
	slippageRat       *big.Rat
	tokenOrder        []solana.PublicKey
	symm              SymbolMapping
	userSymbolAliases map[string]solana.PublicKey
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

func (tb *TableBuilder) Build(intentLine string) (string, *CPIntent, error) {
	instruction, err := parseIntent(intentLine)
	if err != nil {
		return "", nil, err
	}
	targetMint, ok := tb.symm.MaybeMintFromSym(instruction.TargetSymbol)
	if !ok {
		candidate, ok := tb.symm.UnresolvedCandidate()
		if ok {
			return "", nil, &MissingSymbolMappingError{Symbol: instruction.TargetSymbol, Mint: candidate}
		}
		return "", nil, fmt.Errorf("the ticker symbol you provided is either missing from our mapping or isn't part of the pool's pair: %s", instruction.TargetSymbol)
	}

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
	t.AppendRow(table.Row{"Symbol", tb.symm.SymFrom(tb.pool.Token0Mint), tb.symm.SymFrom(tb.pool.Token1Mint)})

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
		balancesDisplay[i+1] = fmtForDisplay(balances[i].Balance, balances[i].Decimals, int(balances[i].Decimals))
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
	cp := ConstantProduct{TradeFeeRate: tb.poolAmmConfig.TradeFeeRate, SlippageRatio: slippageRat}
	intentMeta, intentErr := NewCPIntent(cp, tb.pool, tb.poolPubKey, instruction, targetMint, balances...)
	intentRow := table.Row{"Intent", "", ""}
	targetTokenCell := 0
	if targetMint.Equals(tb.pool.Token1Mint) {
		targetTokenCell = 1
	}
	counterTokenCell := 1 - targetTokenCell
	if intentErr != nil {
		errMsg := fmt.Sprintf("intent failed: %s", intentErr)
		if instruction != nil {
			errMsg = fmt.Sprintf("%s %s %s failed: %s", instruction.Verb, instruction.AmountStr, instruction.TargetSymbol, intentErr)
		}
		intentRow[targetTokenCell+1] = errMsg
		intentRow[counterTokenCell+1] = errMsg
		t.AppendRow(intentRow, table.RowConfig{AutoMerge: true})
		t.Render()
		return builder.String(), intentMeta, nil
	}

	counterDecimals := intentMeta.CounterLeg().Decimals
	counterTokenAmount := fmtForDisplay(cloneInt(intentMeta.Amounts.QuoteAmount), counterDecimals, int(counterDecimals))
	intentText := intentMeta.String()
	counterSymbol := tb.symm.SymFrom(intentMeta.CounterLeg().Mint)

	switch intentMeta.SwapKind {
	case SwapKindBaseInput:
		intentRow[targetTokenCell+1] = intentText
		intentRow[counterTokenCell+1] = fmt.Sprintf("receiving %s %s", counterTokenAmount, counterSymbol)
	case SwapKindBaseOutput:
		intentRow[targetTokenCell+1] = intentText
		intentRow[counterTokenCell+1] = fmt.Sprintf("paying %s %s", counterTokenAmount, counterSymbol)
	default:
		panic("shouldn't be here, did we miss an early return checking for verbToSwapDir error value?")
	}
	t.AppendRow(intentRow)
	t.Render()
	return builder.String(), intentMeta, nil
}

// poolBalances will fetch balances from all vaults concurrently or in parallel depending on how you configure Go exec env,
// it's also cpu cache friendly. We don't side step the cache line, each Go routine owns and mutates its own data, no
// shared data contention resulting in cache evictions
//
// Returns two equal length slices (equals len(vaults)), balances and errors, so they can be indexed over in tandem.
func poolBalances(ctx context.Context, client *rpc.Client, vaults []solana.PublicKey) ([]*PoolBalance, []error) {
	results := make([]*PoolBalance, len(vaults))
	errs := make([]error, len(vaults))
	wg := sync.WaitGroup{}
	for i := range vaults {
		i, vault := i, &vaults[i] // NOTE(@hadydotai): order matters here, https://go.dev/ref/spec#For_clause
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.GetTokenAccountBalance(ctx, *vault, rpc.CommitmentFinalized)
			if err != nil {
				errs[i] = fmt.Errorf("rpc call getTokenAccountBalance failed: %w", err)
				return
			}
			if resp == nil {
				errs[i] = errors.New("rpc call getTokenAccountBalance failed, returning empty response")
				return
			}

			if resp.Value == nil {
				// NOTE(@hadydotai): Really unsure about this, need to find some failure cases. Is it even possible
				// for a pool to drain completely on Raydium, if so what would that look like at this point here.
				// If not, what could land me here then, parsing error? error on the wire? I don't know. Maybe it's okay
				// to have no value and report a 0 balance. For now we'll error and gracefully show it to the user.
				errs[i] = errors.New("rpc call getTokenAccountBalance failed, returned no balance")
				return
			}
			amount, ok := new(big.Int).SetString(resp.Value.Amount, 10)
			if !ok {
				errs[i] = fmt.Errorf("balance is an invalid amount %q", resp.Value.Amount)
				return
			}
			results[i] = &PoolBalance{
				Balance:  amount,
				Decimals: resp.Value.Decimals,
			}
		}()
	}
	wg.Wait()
	return results, errs
}

func parseIntent(intentLine string) (*IntentInstruction, error) {
	intentParts := strings.Fields(intentLine)
	if len(intentParts) != 3 {
		return nil, errors.New("intent instructions must be <verb> <amount> <token-symbol>")
	}
	verb := intentParts[0]
	knownAmountStr := intentParts[1]
	tokenSymbol := intentParts[2]
	dir, err := verbToSwapDir(verb)
	if err != nil {
		return nil, err
	}
	return &IntentInstruction{
		Verb:         verb,
		AmountStr:    knownAmountStr,
		Dir:          dir,
		TargetSymbol: strings.ToUpper(tokenSymbol),
	}, nil
}

func verbToSwapDir(verb string) (SwapDir, error) {
	switch verb {
	case "pay", "sell", "swap":
		return SwapDirSell, nil
	case "buy", "get":
		return SwapDirBuy, nil
	default:
		return SwapDirUnknown, fmt.Errorf("verb(%s) has no clear swap direction", verb)
	}
}
