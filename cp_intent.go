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
	Instruction   *IntentInstruction
	TargetMint    solana.PublicKey
	CounterMint   solana.PublicKey
	Dir           SwapDir
	SwapKind      SwapKind
	SlippagePct   float64
	SlippageRatio *big.Rat

	KnownAmount  *big.Int // amount supplied by the user (in token they referenced)
	QuoteAmount  *big.Int // counter amount computed by the curve prior to slippage adjustments
	MinAmountOut *big.Int
	MaxAmountIn  *big.Int

	TokenInMint      solana.PublicKey
	TokenOutMint     solana.PublicKey
	TokenInVault     solana.PublicKey
	TokenOutVault    solana.PublicKey
	TokenInProgram   solana.PublicKey
	TokenOutProgram  solana.PublicKey
	TokenInDecimals  uint8
	TokenOutDecimals uint8

	PoolAddress solana.PublicKey
	AmmConfig   solana.PublicKey
	Observation solana.PublicKey
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
		return cloneInt(ci.KnownAmount)
	case SwapKindBaseOutput:
		return cloneInt(ci.MaxAmountIn)
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
		if ci.KnownAmount == nil || ci.MinAmountOut == nil {
			return nil, errors.New("swap input intent missing amounts")
		}
		if !ci.KnownAmount.IsUint64() || !ci.MinAmountOut.IsUint64() {
			return nil, errors.New("amounts exceed uint64 range required by the program")
		}
		return raydium_cp_swap.NewSwapBaseInputInstruction(
			ci.KnownAmount.Uint64(),
			ci.MinAmountOut.Uint64(),
			payer,
			authority,
			ci.AmmConfig,
			ci.PoolAddress,
			inputATA,
			outputATA,
			ci.TokenInVault,
			ci.TokenOutVault,
			ci.TokenInProgram,
			ci.TokenOutProgram,
			ci.TokenInMint,
			ci.TokenOutMint,
			ci.Observation,
		)
	case SwapKindBaseOutput:
		if ci.MaxAmountIn == nil || ci.KnownAmount == nil {
			return nil, errors.New("swap output intent missing amounts")
		}
		if !ci.MaxAmountIn.IsUint64() || !ci.KnownAmount.IsUint64() {
			return nil, errors.New("amounts exceed uint64 range required by the program")
		}
		return raydium_cp_swap.NewSwapBaseOutputInstruction(
			ci.MaxAmountIn.Uint64(),
			ci.KnownAmount.Uint64(),
			payer,
			authority,
			ci.AmmConfig,
			ci.PoolAddress,
			inputATA,
			outputATA,
			ci.TokenInVault,
			ci.TokenOutVault,
			ci.TokenInProgram,
			ci.TokenOutProgram,
			ci.TokenInMint,
			ci.TokenOutMint,
			ci.Observation,
		)
	default:
		return nil, errors.New("unsupported swap kind")
	}
}

// NewCPIntent derives a CPIntent by combining pool data, balances, and the user instruction.
func NewCPIntent(cp ConstantProduct, pool *raydium_cp_swap.PoolState, poolAddress solana.PublicKey, instruction *IntentInstruction, targetMint solana.PublicKey, balances ...*PoolBalance) (*CPIntent, error) {
	if instruction == nil {
		return nil, errors.New("intent instruction missing")
	}
	if pool == nil {
		return nil, errors.New("pool state missing")
	}
	if len(balances) != 2 {
		return nil, fmt.Errorf("intents are handled per pool which is a pair of tokens, expected 2 balances, got %d", len(balances))
	}
	for i, bal := range balances {
		if bal == nil || bal.Balance == nil {
			return nil, fmt.Errorf("missing balance information for token index %d", i)
		}
	}

	targetIsToken0 := targetMint.Equals(pool.Token0Mint)
	targetIsToken1 := targetMint.Equals(pool.Token1Mint)
	if !targetIsToken0 && !targetIsToken1 {
		return nil, fmt.Errorf("token %s not part of pool", targetMint.String())
	}

	intent := &CPIntent{
		Instruction:   instruction,
		TargetMint:    targetMint,
		Dir:           instruction.Dir,
		SwapKind:      SwapKindUnknown,
		SlippagePct:   cp.SlippagePct,
		PoolAddress:   poolAddress,
		AmmConfig:     pool.AmmConfig,
		Observation:   pool.ObservationKey,
		SlippageRatio: cloneRat(cp.SlippageRatio),
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
			intent.TokenInMint, intent.TokenOutMint = pool.Token1Mint, pool.Token0Mint
			intent.TokenInVault, intent.TokenOutVault = pool.Token1Vault, pool.Token0Vault
			intent.TokenInProgram, intent.TokenOutProgram = pool.Token1Program, pool.Token0Program
			intent.TokenInDecimals, intent.TokenOutDecimals = balances[1].Decimals, balances[0].Decimals
		} else {
			knownAmount, err = fmtForMath(instruction.AmountStr, balances[1].Decimals)
			if err != nil {
				return nil, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[0], balances[1]
			intent.TokenInMint, intent.TokenOutMint = pool.Token0Mint, pool.Token1Mint
			intent.TokenInVault, intent.TokenOutVault = pool.Token0Vault, pool.Token1Vault
			intent.TokenInProgram, intent.TokenOutProgram = pool.Token0Program, pool.Token1Program
			intent.TokenInDecimals, intent.TokenOutDecimals = balances[0].Decimals, balances[1].Decimals
		}
		quote, err = cp.QuoteIn(knownAmount)
		if err != nil {
			return nil, err
		}
		maxIn, err := applySlippageCeil(quote, intent.SlippageRatio)
		if err != nil {
			return nil, err
		}
		intent.MaxAmountIn = maxIn
	case SwapDirSell:
		intent.SwapKind = SwapKindBaseInput
		if targetIsToken0 {
			knownAmount, err = fmtForMath(instruction.AmountStr, balances[0].Decimals)
			if err != nil {
				return nil, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[0], balances[1]
			intent.TokenInMint, intent.TokenOutMint = pool.Token0Mint, pool.Token1Mint
			intent.TokenInVault, intent.TokenOutVault = pool.Token0Vault, pool.Token1Vault
			intent.TokenInProgram, intent.TokenOutProgram = pool.Token0Program, pool.Token1Program
			intent.TokenInDecimals, intent.TokenOutDecimals = balances[0].Decimals, balances[1].Decimals
		} else {
			knownAmount, err = fmtForMath(instruction.AmountStr, balances[1].Decimals)
			if err != nil {
				return nil, err
			}
			cp.TokenInReserve, cp.TokenOutReserve = balances[1], balances[0]
			intent.TokenInMint, intent.TokenOutMint = pool.Token1Mint, pool.Token0Mint
			intent.TokenInVault, intent.TokenOutVault = pool.Token1Vault, pool.Token0Vault
			intent.TokenInProgram, intent.TokenOutProgram = pool.Token1Program, pool.Token0Program
			intent.TokenInDecimals, intent.TokenOutDecimals = balances[1].Decimals, balances[0].Decimals
		}
		quote, err = cp.QuoteOut(knownAmount)
		if err != nil {
			return nil, err
		}
		minOut, err := applySlippageFloor(quote, intent.SlippageRatio)
		if err != nil {
			return nil, err
		}
		intent.MinAmountOut = minOut
	default:
		return nil, fmt.Errorf("swap direction unknown for verb %s", instruction.Verb)
	}

	intent.KnownAmount = cloneInt(knownAmount)
	intent.QuoteAmount = cloneInt(quote)

	switch instruction.Dir {
	case SwapDirSell:
		intent.CounterMint = intent.TokenOutMint
	case SwapDirBuy:
		intent.CounterMint = intent.TokenInMint
	}

	return intent, nil
}

func cloneInt(v *big.Int) *big.Int {
	if v == nil {
		return nil
	}
	return new(big.Int).Set(v)
}

func cloneRat(v *big.Rat) *big.Rat {
	if v == nil {
		return nil
	}
	return new(big.Rat).Set(v)
}
