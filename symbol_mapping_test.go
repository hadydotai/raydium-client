package main

import (
	"testing"

	solana "github.com/gagliardetto/solana-go"
)

func TestMissingSymbolMappingErrorFormatting(t *testing.T) {
	err := &MissingSymbolMappingError{Symbol: "SOL", Mint: solana.NewWallet().PublicKey().String()}
	if err.Error() == "" {
		t.Fatalf("expected non-empty error message")
	}
	if err.MintDisplay() == "" {
		t.Fatalf("expected formatted mint display")
	}
}

func TestNormalizeSymbol(t *testing.T) {
	raw := "  so\x00 \t"
	if got := normalizeSymbol(raw); got != "SO" {
		t.Fatalf("normalizeSymbol = %q, want SO", got)
	}
}

func TestSymbolMappingUnresolvedCandidate(t *testing.T) {
	symm := SymbolMapping{
		mintToSymbol: map[string]string{},
		symbolToMint: map[string]solana.PublicKey{},
		unresolved:   map[string]struct{}{"mintA": {}, "mintB": {}},
	}
	if _, ok := symm.UnresolvedCandidate(); ok {
		t.Fatalf("expected false when more than one unresolved")
	}
	symm.unresolved = map[string]struct{}{"mintA": {}}
	candidate, ok := symm.UnresolvedCandidate()
	if !ok || candidate != "mintA" {
		t.Fatalf("unexpected candidate %q ok=%v", candidate, ok)
	}
}

func TestSymbolMappingMapSymToMint(t *testing.T) {
	mint := solana.NewWallet().PublicKey()
	symm := SymbolMapping{
		mintToSymbol: make(map[string]string),
		symbolToMint: make(map[string]solana.PublicKey),
		unresolved:   map[string]struct{}{mint.String(): {}},
	}
	symm.MapSymToMint("SOL", mint.String())
	if _, ok := symm.unresolved[mint.String()]; ok {
		t.Fatalf("mint should have been removed from unresolved")
	}
	if got := symm.SymFrom(mint); got != "SOL" {
		t.Fatalf("SymFrom returned %s", got)
	}
	if got, ok := symm.MaybeMintFromSym("SOL"); !ok || !got.Equals(mint) {
		t.Fatalf("MaybeMintFromSym mismatch")
	}
}

func TestSymbolMappingUnknownSymbol(t *testing.T) {
	symm := SymbolMapping{
		mintToSymbol: make(map[string]string),
		symbolToMint: make(map[string]solana.PublicKey),
		unresolved:   make(map[string]struct{}),
	}
	mint, ok := symm.MaybeMintFromSym("FOO")
	if ok || mint != (solana.PublicKey{}) {
		t.Fatalf("expected empty mint when symbol missing")
	}
}
