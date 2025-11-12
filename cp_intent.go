package main

import (
	"errors"
	"fmt"
	"math/big"

	"hadydotai/raydium-client/raydium_cp_swap"

	solana "github.com/gagliardetto/solana-go"
)

// SwapKind identifies which Raydium instruction flavor we must build.
type SwapKind uint8

const (
	SwapKindUnknown SwapKind = iota
	SwapKindBaseInput
	SwapKindBaseOutput
)

type PoolBalance struct {
	Balance  *big.Int
	Decimals uint8
}

// SwapLeg captures one side of the swap (input or output).
type SwapLeg struct {
	Mint     solana.PublicKey
	Vault    solana.PublicKey
	Program  solana.PublicKey
	Decimals uint8
}

// SwapAmounts centralizes the various numeric values produced while resolving an intent.
type SwapAmounts struct {
	KnownAmount  *big.Int // amount supplied by the user (in token they referenced)
	QuoteAmount  *big.Int // counter amount computed by the curve prior to slippage adjustments
	MinAmountOut *big.Int
	MaxAmountIn  *big.Int
}

// PoolAccounts references the on-chain accounts that tie this intent to a specific Raydium pool.
type PoolAccounts struct {
	Address     solana.PublicKey
	AmmConfig   solana.PublicKey
	Observation solana.PublicKey
}

type IntentInstruction struct {
	Verb         string
	AmountStr    string
	Dir          SwapDir
	TargetSymbol string
}

func (ii *IntentInstruction) String() string {
	if ii.TargetSymbol == "" {
		return fmt.Sprintf("%s %s", ii.Verb, ii.AmountStr)
	}
	return fmt.Sprintf("%s %s %s", ii.Verb, ii.AmountStr, ii.TargetSymbol)
}

// CPIntent captures the resolved swap details derived from the pool + user intent.
type CPIntent struct {
	Instruction *IntentInstruction
	SwapKind    SwapKind
	Amounts     SwapAmounts
	TokenIn     SwapLeg
	TokenOut    SwapLeg
	Pool        PoolAccounts
}

// String renders the original intent instruction for UI purposes.
func (ci *CPIntent) String() string {
	if ci == nil || ci.Instruction == nil {
		return ""
	}
	return ci.Instruction.String()
}

// RequiredInputAmount returns the amount that must be present in the payer's input ATA.
func (ci *CPIntent) RequiredInputAmount() *big.Int {
	if ci == nil {
		return nil
	}
	switch ci.SwapKind {
	case SwapKindBaseInput:
		return cloneInt(ci.Amounts.KnownAmount)
	case SwapKindBaseOutput:
		return cloneInt(ci.Amounts.MaxAmountIn)
	default:
		return nil
	}
}

// CounterLeg returns the swap leg that represents the counter token relative to the user's instruction.
func (ci *CPIntent) CounterLeg() *SwapLeg {
	if ci == nil {
		return nil
	}
	switch ci.SwapKind {
	case SwapKindBaseInput:
		return &ci.TokenOut
	case SwapKindBaseOutput:
		return &ci.TokenIn
	default:
		return nil
	}
}

// BuildSwapInstruction materializes the concrete Raydium instruction for the CPIntent.
func (ci *CPIntent) BuildSwapInstruction(payer solana.PublicKey, authority solana.PublicKey, inputATA solana.PublicKey, outputATA solana.PublicKey) (solana.Instruction, error) {
	if ci == nil {
		return nil, errors.New("cp intent missing")
	}
	switch ci.SwapKind {
	case SwapKindBaseInput:
		if ci.Amounts.KnownAmount == nil || ci.Amounts.MinAmountOut == nil {
			return nil, errors.New("swap input intent missing amounts")
		}
		if !ci.Amounts.KnownAmount.IsUint64() || !ci.Amounts.MinAmountOut.IsUint64() {
			return nil, errors.New("amounts exceed uint64 range required by the program")
		}
		return raydium_cp_swap.NewSwapBaseInputInstruction(
			ci.Amounts.KnownAmount.Uint64(),
			ci.Amounts.MinAmountOut.Uint64(),
			payer,
			authority,
			ci.Pool.AmmConfig,
			ci.Pool.Address,
			inputATA,
			outputATA,
			ci.TokenIn.Vault,
			ci.TokenOut.Vault,
			ci.TokenIn.Program,
			ci.TokenOut.Program,
			ci.TokenIn.Mint,
			ci.TokenOut.Mint,
			ci.Pool.Observation,
		)
	case SwapKindBaseOutput:
		if ci.Amounts.MaxAmountIn == nil || ci.Amounts.KnownAmount == nil {
			return nil, errors.New("swap output intent missing amounts")
		}
		if !ci.Amounts.MaxAmountIn.IsUint64() || !ci.Amounts.KnownAmount.IsUint64() {
			return nil, errors.New("amounts exceed uint64 range required by the program")
		}
		return raydium_cp_swap.NewSwapBaseOutputInstruction(
			ci.Amounts.MaxAmountIn.Uint64(),
			ci.Amounts.KnownAmount.Uint64(),
			payer,
			authority,
			ci.Pool.AmmConfig,
			ci.Pool.Address,
			inputATA,
			outputATA,
			ci.TokenIn.Vault,
			ci.TokenOut.Vault,
			ci.TokenIn.Program,
			ci.TokenOut.Program,
			ci.TokenIn.Mint,
			ci.TokenOut.Mint,
			ci.Pool.Observation,
		)
	default:
		return nil, errors.New("unsupported swap kind")
	}
}

// NewCPIntent derives a CPIntent by combining pool data, balances, and the user instruction.
func NewCPIntent(cp ConstantProduct, pool *raydium_cp_swap.PoolState, poolAddress solana.PublicKey, instruction *IntentInstruction, targetMint solana.PublicKey, balances ...*PoolBalance) (*CPIntent, error) {
	for i, bal := range balances {
		if bal == nil || bal.Balance == nil {
			return nil, fmt.Errorf("missing balance information for token index %d", i)
		}
	}

	targetIsToken0 := targetMint.Equals(pool.Token0Mint)

	makeLeg := func(mint, vault, program solana.PublicKey, decimals uint8) SwapLeg {
		return SwapLeg{Mint: mint, Vault: vault, Program: program, Decimals: decimals}
	}

	intent := &CPIntent{
		Instruction: instruction,
		SwapKind:    SwapKindUnknown,
		Pool: PoolAccounts{
			Address:     poolAddress,
			AmmConfig:   pool.AmmConfig,
			Observation: pool.ObservationKey,
		},
	}

	var (
		knownAmount *big.Int
		quote       *big.Int
		err         error
	)

	switch instruction.Dir {
	case SwapDirBuy:
		intent.SwapKind = SwapKindBaseOutput
		if targetIsToken0 {
			knownAmount, err = fmtForMath(instruction.AmountStr, balances[0].Decimals)
			if err != nil {
				return nil, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[1], balances[0]
			intent.TokenIn = makeLeg(pool.Token1Mint, pool.Token1Vault, pool.Token1Program, balances[1].Decimals)
			intent.TokenOut = makeLeg(pool.Token0Mint, pool.Token0Vault, pool.Token0Program, balances[0].Decimals)
		} else {
			knownAmount, err = fmtForMath(instruction.AmountStr, balances[1].Decimals)
			if err != nil {
				return nil, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[0], balances[1]
			intent.TokenIn = makeLeg(pool.Token0Mint, pool.Token0Vault, pool.Token0Program, balances[0].Decimals)
			intent.TokenOut = makeLeg(pool.Token1Mint, pool.Token1Vault, pool.Token1Program, balances[1].Decimals)
		}
		quote, err = cp.QuoteIn(knownAmount)
		if err != nil {
			return nil, err
		}
		maxIn, err := applySlippageCeil(quote, cp.SlippageRatio)
		if err != nil {
			return nil, err
		}
		intent.Amounts.MaxAmountIn = maxIn
	case SwapDirSell:
		intent.SwapKind = SwapKindBaseInput
		if targetIsToken0 {
			knownAmount, err = fmtForMath(instruction.AmountStr, balances[0].Decimals)
			if err != nil {
				return nil, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[0], balances[1]
			intent.TokenIn = makeLeg(pool.Token0Mint, pool.Token0Vault, pool.Token0Program, balances[0].Decimals)
			intent.TokenOut = makeLeg(pool.Token1Mint, pool.Token1Vault, pool.Token1Program, balances[1].Decimals)
		} else {
			knownAmount, err = fmtForMath(instruction.AmountStr, balances[1].Decimals)
			if err != nil {
				return nil, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[1], balances[0]
			intent.TokenIn = makeLeg(pool.Token1Mint, pool.Token1Vault, pool.Token1Program, balances[1].Decimals)
			intent.TokenOut = makeLeg(pool.Token0Mint, pool.Token0Vault, pool.Token0Program, balances[0].Decimals)
		}
		quote, err = cp.QuoteOut(knownAmount)
		if err != nil {
			return nil, err
		}
		minOut, err := applySlippageFloor(quote, cp.SlippageRatio)
		if err != nil {
			return nil, err
		}
		intent.Amounts.MinAmountOut = minOut
	default:
		return nil, fmt.Errorf("swap direction unknown for verb %s", instruction.Verb)
	}

	intent.Amounts.KnownAmount = cloneInt(knownAmount)
	intent.Amounts.QuoteAmount = cloneInt(quote)

	return intent, nil
}

func cloneInt(v *big.Int) *big.Int {
	if v == nil {
		return nil
	}
	return new(big.Int).Set(v)
}
