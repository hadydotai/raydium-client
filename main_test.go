package main

import (
	"encoding/json"
	"math/big"
	"testing"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func makeTxEnvelope(t *testing.T, keys []solana.PublicKey) *rpc.TransactionResultEnvelope {
	t.Helper()
	tx := &solana.Transaction{Message: solana.Message{AccountKeys: keys}}
	data, err := json.Marshal(tx)
	if err != nil {
		t.Fatalf("marshal tx: %v", err)
	}
	env := &rpc.TransactionResultEnvelope{}
	if err := env.UnmarshalJSON(data); err != nil {
		t.Fatalf("unmarshal tx json: %v", err)
	}
	return env
}

func makeTokenBalance(accountIndex int, mint solana.PublicKey, amount string, decimals uint8) rpc.TokenBalance {
	return rpc.TokenBalance{
		AccountIndex: uint16(accountIndex),
		Mint:         mint,
		UiTokenAmount: &rpc.UiTokenAmount{
			Amount:         amount,
			Decimals:       decimals,
			UiAmountString: amount,
		},
	}
}

func TestTokenDeltaFromResultMissingPreBalance(t *testing.T) {
	account := solana.NewWallet().PublicKey()
	mint := solana.NewWallet().PublicKey()
	keys := []solana.PublicKey{solana.NewWallet().PublicKey(), account}
	result := &rpc.GetTransactionResult{
		Transaction: makeTxEnvelope(t, keys),
		Meta: &rpc.TransactionMeta{
			PostTokenBalances: []rpc.TokenBalance{makeTokenBalance(1, mint, "500", 0)},
		},
	}
	delta, ok := tokenDeltaFromResult(result, account, mint)
	if !ok {
		t.Fatalf("expected delta to be computed")
	}
	if delta.Cmp(big.NewInt(500)) != 0 {
		t.Fatalf("unexpected delta %s", delta)
	}
}

func TestTokenDeltaFromResultNoMatchingAccount(t *testing.T) {
	account := solana.NewWallet().PublicKey()
	mint := solana.NewWallet().PublicKey()
	result := &rpc.GetTransactionResult{
		Transaction: makeTxEnvelope(t, []solana.PublicKey{solana.NewWallet().PublicKey()}),
		Meta:        &rpc.TransactionMeta{},
	}
	if _, ok := tokenDeltaFromResult(result, account, mint); ok {
		t.Fatalf("expected tokenDeltaFromResult to fail when account missing")
	}
}

func TestTokenDeltaFromResultRequiresBalances(t *testing.T) {
	account := solana.NewWallet().PublicKey()
	mint := solana.NewWallet().PublicKey()
	keys := []solana.PublicKey{solana.NewWallet().PublicKey(), account}
	result := &rpc.GetTransactionResult{
		Transaction: makeTxEnvelope(t, keys),
		Meta:        &rpc.TransactionMeta{},
	}
	if _, ok := tokenDeltaFromResult(result, account, mint); ok {
		t.Fatalf("expected no delta when balances missing")
	}
}
