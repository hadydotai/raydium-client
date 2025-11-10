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
	"time"

	"hadydotai/raydium-client/raydium_cp_swap"

	solana "github.com/gagliardetto/solana-go"
	atapkg "github.com/gagliardetto/solana-go/programs/associated-token-account"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/programs/system"
	tokenprog "github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
)

var (
	ATAProgramID     = atapkg.ProgramID
	SystemProgramID  = system.ProgramID
	DefaultUnitLimit = uint32(200_000_000) // rough ballpark, https://solana.com/docs/core/fees and from simulating a few transactions
	DefaultUnitPrice = uint64(5000)        // micro-lamports, 0.005 lamports
	//TODO(@hadydotai): Figure out how to give the user the ability to adjust priority fees
)

func init() {
	raydium_cp_swap.ProgramID = solana.MustPublicKeyFromBase58("DRaycpLY18LhpbydsBWbVJtxpNv9oXPgjRSfpF2bWpYb")
}

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
	feeRateDenom         = int64(1_000_000) // https://github.com/raydium-io/raydium-cp-swap/blob/master/programs/cp-swap/src/curve/fees.rs#L3
)

var wSOLMint = solana.MustPublicKeyFromBase58(string(wSOLMintAddr))

type SwapDir uint8

const (
	SwapDirUnknown SwapDir = iota
	SwapDirBuy
	SwapDirSell
)

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

func isNativeSOL(mint solana.PublicKey) bool {
	return mint.Equals(wSOLMint)
}

func isAccountMissingErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, rpc.ErrNotFound) {
		return true
	}
	var rpcErr *jsonrpc.RPCError
	if errors.As(err, &rpcErr) {
		if rpcErr.Code == -32602 || strings.Contains(strings.ToLower(rpcErr.Message), "could not find account") {
			return true
		}
	}
	return false
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

func buildTokenSymbolMaps(ctx context.Context, client *rpc.Client, mints []solana.PublicKey) (map[string]string, map[string]solana.PublicKey) {
	mintToSymbol := make(map[string]string, len(mints))
	symbolToMint := make(map[string]solana.PublicKey, len(mints))
	for _, mint := range mints {
		symbol := fetchTokenSymbol(ctx, client, mint)
		if len(symbol) == 0 {
			symbol = Addr(mint.String()).String()
		}
		symbol = normalizeSymbol(symbol)
		mintToSymbol[mint.String()] = symbol
		symbolToMint[symbol] = mint
	}
	return mintToSymbol, symbolToMint
}

func fetchTokenSymbol(ctx context.Context, client *rpc.Client, mint solana.PublicKey) string {
	tokenMeta, err := tokenMetadata(ctx, client, mint)
	if err != nil {
		log.Printf("warning: failed to fetch metadata for mint %s: %v", Addr(mint.String()), err)
		return ""
	}
	return tokenMeta.Symbol
}

func normalizeSymbol(raw string) string {
	sym := strings.TrimSpace(raw)
	sym = strings.Trim(sym, "\x00")
	sym = strings.ReplaceAll(sym, " ", "")
	sym = strings.ReplaceAll(sym, "\t", "")
	sym = strings.ToUpper(sym)
	return sym
}

type PoolBalance struct {
	Balance  *big.Int
	Decimals uint8
}

type IntentInstruction struct {
	Verb      string
	AmountStr string
	Dir       SwapDir
	// TargetSymbol is the user-supplied ticker symbol resolved from metadata.
	TargetSymbol string

	// NOTE(@hadydotai):CLEAN: Everything we'll need for building a transaction sits here, I'd like to think of
	// something better than this but for now, it'll do fine.
	// The general issue is, not until we've worked out the intent, we have no clue which token is In and
	// Out of the pair of tokens we're looking at in a pool. By the time we're able to provide an amount resolving
	// for an In or an Out, we already have everything we actually need, which vault is In, which is Out, what the mint
	// addresses are, ...etc. Perhaps it's par for the course, and it's okay to keep it here.
	// -- EDIT+1: A random thought, perhaps I can build the transaction and store it here. I don't actually need anything
	// 	outside of DoIntent.
	Amount          *big.Int
	KnownAmount     *big.Int
	MinAmountOut    *big.Int
	MaxAmountIn     *big.Int
	SlippagePct     float64
	TokenInMint     solana.PublicKey
	TokenOutMint    solana.PublicKey
	TokenInVault    solana.PublicKey
	TokenOutVault   solana.PublicKey
	TokenInProgram  solana.PublicKey
	TokenOutProgram solana.PublicKey
}

func (ii *IntentInstruction) String() string {
	if ii.TargetSymbol == "" {
		return fmt.Sprintf("%s %s", ii.Verb, ii.AmountStr)
	}
	return fmt.Sprintf("%s %s %s", ii.Verb, ii.AmountStr, ii.TargetSymbol)
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
		available := humanAmount(reserveOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
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
		requested := humanAmount(amountOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
		available := humanAmount(reserveOut, cp.TokenOutReserve.Decimals, int(cp.TokenOutReserve.Decimals))
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

func (cp ConstantProduct) DoIntent(instruction *IntentInstruction, pool *raydium_cp_swap.PoolState, targetMint solana.PublicKey, balances ...*PoolBalance) (*IntentInstruction, error) {
	if instruction == nil {
		return nil, errors.New("intent instruction missing")
	}
	if len(balances) != 2 {
		// NOTE(@hadydotai): Who's fault is this actually? Mine or the users? Possible location for a panic here as
		// I don't think we should actually be here at all.
		return instruction, fmt.Errorf("intents are handled per pool which is a pair of tokens, expected 2 balances, got %d", len(balances))
	}
	for i, bal := range balances {
		if bal == nil || bal.Balance == nil {
			return instruction, fmt.Errorf("missing balance information for token index %d", i)
		}
	}
	targetIsToken0 := targetMint.Equals(pool.Token0Mint)
	targetIsToken1 := targetMint.Equals(pool.Token1Mint)
	if !targetIsToken0 && !targetIsToken1 {
		return instruction, fmt.Errorf("token %s not part of pool", targetMint.String())
	}

	var (
		knownAmount *big.Int
		quote       *big.Int
		err         error
	)

	// and now for the tricky bit https://youtu.be/lKXe3HUG2l4?si=Tb6V5Pe0k9nKzcBh&t=628
	switch instruction.Dir {
	case SwapDirBuy:
		if targetIsToken0 {
			knownAmount, err = humanToFixed(instruction.AmountStr, balances[0].Decimals)
			if err != nil {
				return instruction, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[1], balances[0]
			instruction.TokenInMint, instruction.TokenOutMint = pool.Token1Mint, pool.Token0Mint
			instruction.TokenInVault, instruction.TokenOutVault = pool.Token1Vault, pool.Token0Vault
			instruction.TokenInProgram, instruction.TokenOutProgram = pool.Token1Program, pool.Token0Program
		} else {
			knownAmount, err = humanToFixed(instruction.AmountStr, balances[1].Decimals)
			if err != nil {
				return instruction, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[0], balances[1]
			instruction.TokenInMint, instruction.TokenOutMint = pool.Token0Mint, pool.Token1Mint
			instruction.TokenInVault, instruction.TokenOutVault = pool.Token0Vault, pool.Token1Vault
			instruction.TokenInProgram, instruction.TokenOutProgram = pool.Token0Program, pool.Token1Program
		}
		quote, err = cp.QuoteIn(knownAmount)
		if err != nil {
			return instruction, err
		}
	case SwapDirSell:
		if targetIsToken0 {
			knownAmount, err = humanToFixed(instruction.AmountStr, balances[0].Decimals)
			if err != nil {
				return instruction, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[0], balances[1]
			instruction.TokenInMint, instruction.TokenOutMint = pool.Token0Mint, pool.Token1Mint
			instruction.TokenInVault, instruction.TokenOutVault = pool.Token0Vault, pool.Token1Vault
			instruction.TokenInProgram, instruction.TokenOutProgram = pool.Token0Program, pool.Token1Program
		} else {
			knownAmount, err = humanToFixed(instruction.AmountStr, balances[1].Decimals)
			if err != nil {
				return instruction, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[1], balances[0]
			instruction.TokenInMint, instruction.TokenOutMint = pool.Token1Mint, pool.Token0Mint
			instruction.TokenInVault, instruction.TokenOutVault = pool.Token1Vault, pool.Token0Vault
			instruction.TokenInProgram, instruction.TokenOutProgram = pool.Token1Program, pool.Token0Program
		}
		quote, err = cp.QuoteOut(knownAmount)
		if err != nil {
			return instruction, err
		}

	default: // SwapDirUnknown
		panic("shouldn't be here, did we miss an early return checking for verbToSwapDir error value?")
	}

	instruction.Amount = new(big.Int).Set(quote)
	instruction.KnownAmount = new(big.Int).Set(knownAmount)
	instruction.SlippagePct = cp.SlippagePct

	switch instruction.Dir {
	case SwapDirSell:
		minOut, err := applySlippageFloor(quote, cp.SlippageRatio)
		if err != nil {
			return instruction, err
		}
		instruction.MinAmountOut = minOut
	case SwapDirBuy:
		maxIn, err := applySlippageCeil(quote, cp.SlippageRatio)
		if err != nil {
			return instruction, err
		}
		instruction.MaxAmountIn = maxIn
	}

	return instruction, nil
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

func makeATAIfMissing(ctx context.Context, c *rpc.Client, payer solana.PublicKey, owner solana.PublicKey, mint solana.PublicKey) (solana.PublicKey, []solana.Instruction, error) {
	ata, _, err := solana.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		return solana.PublicKey{}, nil, err
	}

	var ixs []solana.Instruction
	_, err = c.GetAccountInfoWithOpts(ctx, ata, &rpc.GetAccountInfoOpts{
		Commitment: rpc.CommitmentProcessed,
	})
	if err != nil {
		if !isAccountMissingErr(err) {
			return solana.PublicKey{}, nil, err
		}
	} else {
		// account exists, nothing to do
		return ata, nil, nil
	}

	ix := atapkg.NewCreateInstruction(
		payer,
		owner,
		mint,
	).Build()
	ixs = append(ixs, ix)

	return ata, ixs, nil
}

func wrapNativeIfNeeded(ctx context.Context, c *rpc.Client, owner solana.PublicKey, ata solana.PublicKey, mint solana.PublicKey, required *big.Int) ([]solana.Instruction, error) {
	if required == nil || required.Sign() <= 0 {
		return nil, nil
	}
	if !isNativeSOL(mint) {
		return nil, nil
	}
	if !required.IsUint64() {
		return nil, fmt.Errorf("required native amount exceeds uint64")
	}
	deficit := new(big.Int).Set(required)
	balance, err := c.GetTokenAccountBalance(ctx, ata, rpc.CommitmentProcessed)
	if err != nil {
		if !isAccountMissingErr(err) {
			return nil, err
		}
	} else if balance != nil && balance.Value != nil {
		if existing, ok := new(big.Int).SetString(balance.Value.Amount, 10); ok {
			deficit.Sub(deficit, existing)
			if deficit.Sign() <= 0 {
				return nil, nil
			}
		}
	}
	if !deficit.IsUint64() {
		return nil, fmt.Errorf("wrap deficit exceeds uint64")
	}
	lamports := deficit.Uint64()
	wrapIxs := []solana.Instruction{
		system.NewTransferInstruction(lamports, owner, ata).Build(),
		tokenprog.NewSyncNativeInstruction(ata).Build(),
	}
	return wrapIxs, nil
}

func main() {
	var (
		hotwalletPath  = flag.String("hotwallet", "", "Path to the hotwallet to use for signing transactions")
		rpcEP          = flag.String("rpc", rpc.DevNet_RPC, "RPC to connect to")
		poolAddr       = flag.String("pool", ourCorePoolAddr.String(), "Pool to interact with")
		intentLine     = flag.String("intent", "", "Intent (<verb> <amount> <token-symbol>), e.g. \"pay 1 SOL\"")
		slippagePct    = flag.Float64("slippage", 0.5, "Slippage tolerance percentage (e.g. 0.5 for 0.5%)")
		nonInteractive = flag.Bool("non-interactive", false, "Don't enter interactive mode (TUI), execute immediately")
	)
	flag.Parse()

	client := rpc.New(*rpcEP)

	// NOTE(@hadydotai): A latest blockhash transaction will likely invalidate in anycase after about a minute,
	// so this leaves us with about 2 minutes of working time, if our RPC node is that slow, then we've got a problem.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	payer, err := solana.PrivateKeyFromSolanaKeygenFile(*hotwalletPath)
	if err != nil {
		log.Fatalf("failed to load private key from hot wallet: %s\n", err)
	}

	poolPubK, err := solana.PublicKeyFromBase58(*poolAddr)
	if err != nil {
		log.Fatalf("deriving public key from pool address (base58) failed, make sure it's base58 encoded: %s\n", err)
	}
	accountInfo, err := client.GetAccountInfoWithOpts(ctx, poolPubK, &rpc.GetAccountInfoOpts{Encoding: solana.EncodingBase64})
	if err != nil {
		log.Fatalf("rpc call getAccountInfo for Pool failed, check if the RPC endpoint is valid, or if you're being limited: %s\n", err)
	}

	pool, err := raydium_cp_swap.ParseAccount_PoolState(accountInfo.Value.Data.GetBinary())
	if err != nil {
		log.Fatalf("parsing PoolState failed, make sure the pool address you passed is a Raydium CP-Swap/CPMM pool: %s\n", err)
	}

	poolAmm, err := client.GetAccountInfoWithOpts(ctx, pool.AmmConfig, &rpc.GetAccountInfoOpts{Encoding: solana.EncodingBase64})
	if err != nil {
		log.Fatalf("rpc call getAccountInfo for Pool's AmmConfig failed, check if the RPC endpoint is valid, or if you're being limited: %s\n", err)
	}
	if poolAmm == nil || poolAmm.Value == nil {
		log.Fatalf("amm config account %s returned no data\n", pool.AmmConfig)
	}
	poolAmmConfig, err := raydium_cp_swap.ParseAccount_AmmConfig(poolAmm.Value.Data.GetBinary())
	if err != nil {
		// NOTE(@hadydotai): Just occurred to me, if the pool is inactive, are we going to end up here?
		log.Fatalf("parsing pool's AmmConfig failed: %s\n", err)
	}

	tokenMints := []solana.PublicKey{pool.Token0Mint, pool.Token1Mint}
	mintToSymbol, symbolToMint := buildTokenSymbolMaps(ctx, client, tokenMints)
	defaultSymbol := mintToSymbol[pool.Token0Mint.String()]
	if defaultSymbol == "" {
		defaultSymbol = Addr(pool.Token0Mint.String()).String()
	}
	initialIntent := strings.TrimSpace(*intentLine)
	if initialIntent == "" {
		initialIntent = fmt.Sprintf("pay 1 %s", defaultSymbol)
	}

	builder := &TableBuilder{
		ctx:           ctx,
		client:        client,
		pool:          pool,
		poolAmmConfig: poolAmmConfig,
		poolAddress:   *poolAddr,
		tokenOrder:    tokenMints,
		mintToSymbol:  mintToSymbol,
		symbolToMint:  symbolToMint,
	}
	if err := builder.SetSlippagePct(*slippagePct); err != nil {
		log.Fatalf("invalid slippage: %s\n", err)
	}

	var intentMeta *IntentInstruction
	if *nonInteractive {
		var report string
		report, intentMeta, err = builder.Build(initialIntent)
		if err != nil {
			log.Fatalf("building intent report failed: %s\n", err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s", report)
	} else {
		ui := newTermUI(builder)
		intentMeta, err = ui.Run(initialIntent)
		if err != nil {
			log.Fatalf("interactive UI failed: %s\n", err)
		}
		if intentMeta == nil { // user has chosen to reject or bailout
			log.Println("Aborting...")
			os.Exit(0)
		}
	}
	// now we do the swap, finally.
	payerPub := payer.PublicKey()
	inATA, inATAixs, err := makeATAIfMissing(ctx, client, payerPub, payerPub, intentMeta.TokenInMint)
	if err != nil {
		log.Fatalf("attempts to get/make ATA for input token failed: %s\n", err)
	}
	outATA, outATAixs, err := makeATAIfMissing(ctx, client, payerPub, payerPub, intentMeta.TokenOutMint)
	if err != nil {
		log.Fatalf("attempts to get/make ATA for output token failed: %s\n", err)
	}
	inATACreated := len(inATAixs) > 0

	auth, _, err := solana.FindProgramAddress(
		[][]byte{[]byte("vault_and_lp_mint_auth_seed")}, // https://github.com/raydium-io/raydium-cp-swap/blob/master/programs/cp-swap/src/lib.rs#L43
		raydium_cp_swap.ProgramID,
	)
	if err != nil {
		log.Fatalf("rpc call findProgramAddress failed: %s \n", err)
	}
	// TODO(@hadydotai): Here I need to check for the direction because the entire instruction will likely change, so I guess let's guard against it now
	// and deal with it later.
	if intentMeta.Dir != SwapDirSell {
		log.Fatalln("unsupported direction, currently attempting a max out transaction (swap_base_output)")
	}
	if intentMeta.KnownAmount == nil {
		log.Fatalln("known amount missing for swap")
	}
	if intentMeta.MinAmountOut == nil {
		log.Fatalln("minimum output amount missing for swap")
	}
	if !intentMeta.KnownAmount.IsUint64() || !intentMeta.MinAmountOut.IsUint64() {
		log.Fatalln("amounts exceed uint64 range required by the program")
	}
	amountInU64 := intentMeta.KnownAmount.Uint64()
	minOutU64 := intentMeta.MinAmountOut.Uint64()
	swapIx, err := raydium_cp_swap.NewSwapBaseInputInstruction(amountInU64, minOutU64,
		solana.Meta(payerPub).WRITE().SIGNER().PublicKey,
		solana.Meta(auth).PublicKey,
		solana.Meta(pool.AmmConfig).PublicKey,
		solana.Meta(poolPubK).WRITE().PublicKey,
		solana.Meta(inATA).WRITE().PublicKey,
		solana.Meta(outATA).WRITE().PublicKey,
		solana.Meta(intentMeta.TokenInVault).WRITE().PublicKey,
		solana.Meta(intentMeta.TokenOutVault).WRITE().PublicKey,
		solana.Meta(intentMeta.TokenInProgram).PublicKey,
		solana.Meta(intentMeta.TokenOutProgram).PublicKey,
		solana.Meta(intentMeta.TokenInMint).PublicKey,
		solana.Meta(intentMeta.TokenOutMint).PublicKey,
		solana.Meta(pool.ObservationKey).WRITE().PublicKey,
	)
	if err != nil {
		log.Fatalf("failed to build swap instruction: %s\n", err)
	}

	// NOTE(@hadydotai): I guess we don't need this, but maybe we can expose it to the user
	cb1 := computebudget.NewSetComputeUnitLimitInstruction(uint32(DefaultUnitLimit)).Build()
	cb2 := computebudget.NewSetComputeUnitPriceInstruction(DefaultUnitPrice).Build()

	wrapIxs, err := wrapNativeIfNeeded(ctx, client, payerPub, inATA, intentMeta.TokenInMint, intentMeta.KnownAmount)
	if err != nil {
		log.Fatalf("wrapping native token failed: %s\n", err)
	}

	var ixs []solana.Instruction
	ixs = append(ixs, cb1, cb2)
	ixs = append(ixs, inATAixs...)
	ixs = append(ixs, outATAixs...)
	ixs = append(ixs, wrapIxs...)
	ixs = append(ixs, swapIx)
	// NOTE(@hadydotai): Was mulling over the transactions and realized I don't close the wSOL temporary ATA we create when
	// dealing with SOL. Then a thought struck me, if we accidently close the output ATA we'll burn the money we just received.
	// So this right here, is a very fucking critical. Any wrong state in any of these values, and we're cooking money.
	if isNativeSOL(intentMeta.TokenInMint) && inATACreated {
		closeIx := tokenprog.NewCloseAccountInstructionBuilder().
			SetAccount(inATA).
			SetDestinationAccount(payerPub).
			SetOwnerAccount(payerPub).
			Build()
		ixs = append(ixs, closeIx)
	}

	recent, err := client.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		log.Fatalf("rpc call getLatestBlockhash failed: %s\n", err)
	}
	tx, err := solana.NewTransaction(
		ixs,
		recent.Value.Blockhash,
		solana.TransactionPayer(payerPub),
	)
	if err != nil {
		log.Fatalf("building transaction failed: %s\n", err)
	}
	if _, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(payerPub) {
			return &payer
		}
		return nil
	}); err != nil {
		log.Fatalf("signing transaction failed: %s\n", err)
	}

	sig, err := client.SendTransaction(ctx, tx)
	if err != nil {
		log.Fatalf("sending transaction failed: %s\n", err.Error())
	}
	log.Println("Tx: ", sig.String())
}
