package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"strings"
	"sync"

	"hadydotai/raydium-client/raydium_cp_swap"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jedib0t/go-pretty/v6/table"
)

var CPMMProgramPubK = solana.MustPublicKeyFromBase58("CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C")

// Addr represents an address on the blockchain, which can render nicely truncated in the middle with ellipsis.
// This is my poor man's solution to fixing these long addresses until I figure out how to deal with and find ticker
// symbols/token metadata on Solana
type Addr string

func (addr Addr) String() string {
	const (
		ellipsis = "…"
		head     = 6
		tail     = 6
	)

	rs := []rune(addr)
	if len(rs) <= head+tail {
		return string(addr)
	}
	return string(rs[:head]) + ellipsis + string(rs[len(rs)-tail:])
}

const (
	wSOLMintAddr    Addr = "So11111111111111111111111111111111111111112"
	ourCorePoolAddr Addr = "3ELLbDZkimZSpnWoWVAfDzeG24yi2LC4sB35ttfNCoEi"
	feeRateDenom         = 1e6 // https://github.com/raydium-io/raydium-cp-swap/blob/master/programs/cp-swap/src/curve/fees.rs#L3
)

type SwapDir uint8

const (
	SwapDirUnknown SwapDir = iota
	SwapDirBuy
	SwapDirSell
)

// mapPtrSliceRetAny maps over a slice of pointers, passing each element to a projection function to pick any value out
// and collect that back into a new slice.
func mapPtrSliceRetAny[Slice ~[]*Elm, Elm any](s Slice, m func(elm *Elm) any) []any {
	mapped := []any{}
	for _, elm := range s {
		mapped = append(mapped, m(elm))
	}
	return mapped
}

func fixedPointScale(decimals uint8) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
}

func humanAmount(raw *big.Int, decimals uint8, precision int) string {
	if raw == nil {
		return "0"
	}
	scale := fixedPointScale(decimals)
	rat := new(big.Rat).SetFrac(raw, scale)
	return rat.FloatString(precision)
}

func humanToFixed(amountStr string, decimals uint8) (*big.Int, error) {
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

type PoolBalance struct {
	Balance  *big.Int
	Decimals uint8
}

type IntentInstruction struct {
	Verb      string
	AmountStr string
	Dir       SwapDir
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

type ConstantProduct struct {
	TokenInReserve  *PoolBalance
	TokenOutReserve *PoolBalance
}

// QuoteOut takes an amount in TokenIn and will produce an amount in TokenOut: amountIn -> amountOut
// In layman's terms, this figures out how much we get out of the pool, provided the amount we put into the pool.
// All amounts are in their respective tokens of course
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
	constantProductK := new(big.Int).Mul(reserveIn, reserveOut)
	updatedReserveIn := new(big.Int).Add(reserveIn, amountIn)
	newReserveOut := new(big.Int).Quo(constantProductK, updatedReserveIn)
	amountOut := new(big.Int).Sub(reserveOut, newReserveOut)
	if amountOut.Sign() <= 0 {
		// NOTE(@hadydotai): we'd only end up here if we math our way into draining the pool on one side,
		// I think this should be a flat out error and yell at the user for it, maybe?
		return nil, errors.New("trade would not yield a positive output amount")
	}
	if amountOut.Cmp(reserveOut) >= 0 {
		available := humanAmount(reserveOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
		return nil, fmt.Errorf("requested output would exceed available %s liquidity", available)
	}
	return amountOut, nil
}

// QuoteIn takes an amount in TokenOut and will produce an amount in TokenIn: amountOut -> amountIn
// In layman's terms, this figures out how much we need to give, to match the amount we want out of the pool.
// All amounts are in their respective tokens of course
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
		requested := humanAmount(amountOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
		available := humanAmount(reserveOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
		return nil, fmt.Errorf("requested %s exceeds available %s liquidity", requested, available)
	}
	updatedReserveOut := new(big.Int).Sub(reserveOut, amountOut)
	newReserveIn := new(big.Int).Quo(constantProductK, updatedReserveOut)
	amountIn := new(big.Int).Sub(newReserveIn, reserveIn)
	if amountIn.Sign() <= 0 {
		// NOTE(@hadydotai): we'd only end up here if we math our way into draining the pool on one side,
		// I think this should be a flat out error and yell at the user for it, maybe?
		return nil, errors.New("trade would not require a positive input amount")
	}
	return amountIn, nil
}

func (cp ConstantProduct) DoIntent(intentLine string, pool *raydium_cp_swap.PoolState, targetTokenAddr Addr, balances ...*PoolBalance) (*big.Int, *IntentInstruction, error) {
	instruction, err := parseIntent(intentLine)
	if err != nil {
		return nil, nil, err
	}
	if len(balances) != 2 {
		// NOTE(@hadydotai): Who's fault is this actually? Mine or the users? Possible location for a panic here as
		// I don't think we should actually be here at all.
		return nil, instruction, fmt.Errorf("intents are handled per pool which is a pair of tokens, expected 2 balances, got %d", len(balances))
	}
	for i, bal := range balances {
		if bal == nil || bal.Balance == nil {
			return nil, instruction, fmt.Errorf("missing balance information for token index %d", i)
		}
	}
	var knownAmount *big.Int
	var quote *big.Int

	switch instruction.Dir {
	case SwapDirBuy:
		if targetTokenAddr == Addr(pool.Token0Mint.String()) {
			knownAmount, err = humanToFixed(instruction.AmountStr, balances[0].Decimals)
			if err != nil {
				return nil, instruction, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[1], balances[0]
		} else {
			knownAmount, err = humanToFixed(instruction.AmountStr, balances[1].Decimals)
			if err != nil {
				return nil, instruction, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[0], balances[1]
		}
		quote, err = cp.QuoteIn(knownAmount)
		if err != nil {
			return nil, instruction, err
		}
	case SwapDirSell:
		if targetTokenAddr == Addr(pool.Token0Mint.String()) {
			knownAmount, err = humanToFixed(instruction.AmountStr, balances[0].Decimals)
			if err != nil {
				return nil, instruction, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[0], balances[1]
		} else {
			knownAmount, err = humanToFixed(instruction.AmountStr, balances[1].Decimals)
			if err != nil {
				return nil, instruction, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[1], balances[0]
		}
		quote, err = cp.QuoteOut(knownAmount)
		if err != nil {
			return nil, instruction, err
		}

	default: // SwapDirUnknown
		panic("shouldn't be here, did we miss an early return checking for verbToSwapDir error value?")
	}

	return quote, instruction, nil
}

func parseIntent(intentLine string) (*IntentInstruction, error) {
	intentParts := strings.Fields(intentLine)
	if len(intentParts) != 2 {
		return nil, errors.New("intent instructions must be a <verb> <amount>, and one pair per line")
	}
	verb := intentParts[0]
	knownAmountStr := intentParts[1]
	dir, err := verbToSwapDir(verb)
	if err != nil {
		return nil, err
	}
	return &IntentInstruction{
		Verb:      verb,
		AmountStr: knownAmountStr,
		Dir:       dir,
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

func main() {
	var (
		rpcEP      = flag.String("rpc", rpc.DevNet_RPC, "RPC to connect to")
		poolAddr   = flag.String("pool", ourCorePoolAddr.String(), "Pool to interact with")
		intentLine = flag.String("intent", "pay 100", "Intent and direction of the trade")
		tokenAddr  = flag.String("token", wSOLMintAddr.String(), "Token address to trade")
	)
	flag.Parse()

	poolPubK, err := solana.PublicKeyFromBase58(*poolAddr)
	if err != nil {
		log.Fatalf("deriving public key from pool address (base58) failed, make sure it's b58 encoded: %s\n", err)
	}

	client := rpc.New(*rpcEP)

	// NOTE(@hadydotai): We'll time this out in the future
	ctx := context.Background()
	accountInfo, err := client.GetAccountInfoWithOpts(ctx, poolPubK, &rpc.GetAccountInfoOpts{Encoding: solana.EncodingBase64})
	if err != nil {
		log.Fatalf("rpc call getAccountInfo failed, check if the RPC endpoint is valid, or if you're being limited: %s\n", err)
	}

	pool, err := raydium_cp_swap.ParseAccount_PoolState(accountInfo.Value.Data.GetBinary())
	if err != nil {
		log.Fatalf("parsing PoolState failed, make sure the pool address you passed is a Raydium CP-Swap/CPMM pool: %s\n", err)
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetTitle(*poolAddr)
	t.SetCaption("CPMM/CP-Swap Raydium Pool")
	t.AppendHeader(table.Row{"", "Token 0", "Token 1"})
	t.AppendRow(table.Row{"Addr", Addr(pool.Token0Mint.String()), Addr(pool.Token1Mint.String())})

	balances, errs := poolBalances(ctx, client, []solana.PublicKey{pool.Token0Vault, pool.Token1Vault})
	balancesDisplay := make([]any, len(balances)+1)
	balancesDisplay[0] = "Balances"
	for i := range balances {
		if errs[i] != nil {
			balancesDisplay[i+1] = err
			continue
		}
		balancesDisplay[i+1] = humanAmount(balances[i].Balance, balances[i].Decimals, int(balances[i].Decimals))
	}
	t.AppendRow(balancesDisplay)

	// NOTE(@hadydotai): Here's a little false-positive quirk with static analysis, uncomment the next line
	// and change the decl+assign operator `:=` before the append to `=`, reassigning decimals. Can you figure out why
	// go-static analysis complains about this? Hint: SSA.
	// decimals := make([]any, len(balances)+1)
	decimals := append([]any{"Decimals"}, mapPtrSliceRetAny(balances, func(elm *PoolBalance) any { return elm.Decimals })...)
	t.AppendRow(decimals)

	targetAddr := Addr(*tokenAddr)
	cp := ConstantProduct{}
	unknownAmount, intentMeta, intentErr := cp.DoIntent(*intentLine, pool, targetAddr, balances...)
	intentRow := table.Row{"Intent", "", ""}
	targetTokenCell := 0
	if targetAddr == Addr(pool.Token1Mint.String()) {
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
		t.AppendRow(intentRow)
		t.Render()
		return
	}

	var counterTokenDecimals uint8
	if counterTokenCell < len(balances) && balances[counterTokenCell] != nil {
		counterTokenDecimals = balances[counterTokenCell].Decimals
	}
	counterTokenAmount := humanAmount(unknownAmount, counterTokenDecimals, int(counterTokenDecimals))
	intentText := fmt.Sprintf("%s %s", intentMeta.Verb, intentMeta.AmountStr)

	switch intentMeta.Dir {
	case SwapDirSell:
		intentRow[targetTokenCell+1] = intentText
		intentRow[counterTokenCell+1] = fmt.Sprintf("receiving %s", counterTokenAmount)
	case SwapDirBuy:
		intentRow[targetTokenCell+1] = intentText
		intentRow[counterTokenCell+1] = fmt.Sprintf("paying %s", counterTokenAmount)
	default: // SwapDirUnknown
		panic("shouldn't be here, did we miss an early return checking for verbToSwapDir error value?")
	}
	t.AppendRow(intentRow)
	t.Render()
}
