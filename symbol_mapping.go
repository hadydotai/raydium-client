package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// MissingSymbolMappingError is returned when the user references a symbol that we
// cannot resolve, but there is exactly one pool token lacking metadata.
type MissingSymbolMappingError struct {
	Symbol string
	Mint   string
}

func (e *MissingSymbolMappingError) Error() string {
	return fmt.Sprintf("unknown token symbol %s; map to mint %s?", e.Symbol, Addr(e.Mint))
}

func (e *MissingSymbolMappingError) MintDisplay() string {
	return Addr(e.Mint).String()
}

type SymbolMapping struct {
	mintToSymbol map[string]string
	symbolToMint map[string]solana.PublicKey
	// NOTE(@hadydotai): We'll keep track of unresolved mappings, while we will still fallback on putting whatever we can
	// in the symbol, it's still not exactly the correct mapping if we had otherwise managed to fetch something. But we still
	// need a flag to tell us which of the mappings didn't resolve
	unresolved map[string]struct{} // mint -> void
}

func (symm SymbolMapping) MapSymToMint(sym, mint string) {
	delete(symm.unresolved, mint)
	symm.mintToSymbol[mint] = sym

	mintPubK, _ := solana.PublicKeyFromBase58(mint)
	symm.symbolToMint[sym] = mintPubK
}

func (symm SymbolMapping) MaybeSymFrom(mint solana.PublicKey) (string, bool) {
	sym, ok := symm.mintToSymbol[mint.String()]
	return sym, ok
}

func (symm SymbolMapping) SymFrom(mint solana.PublicKey) string {
	sym, _ := symm.MaybeSymFrom(mint)
	return sym
}

func (symm SymbolMapping) MaybeMintFromSym(sym string) (solana.PublicKey, bool) {
	mint, ok := symm.symbolToMint[sym]
	return mint, ok
}

func (symm SymbolMapping) MintFromSym(sym string) solana.PublicKey {
	mint, _ := symm.MaybeMintFromSym(sym)
	return mint
}

func (symm SymbolMapping) UnresolvedCandidate() (string, bool) {
	res := []string{}
	for k := range symm.unresolved {
		res = append(res, k)
	}
	if len(res) == 1 {
		// NOTE(@hadydotai): we only have a pair, or should only have a pair, if we got both unresolved then we shouldn't be here.
		// while in theory I can show the user both options and ask them to map their symbol to the correct one, we're just asking
		// for trouble at this point. Mistakes are bound to happen. I'd rather just force the user to now explicitly copy and paste
		// the fallback symbols as we display them (which should be managable since we truncate it)
		return res[0], true
	}
	return "", false
}

func normalizeSymbol(raw string) string {
	sym := strings.TrimSpace(raw)
	sym = strings.Trim(sym, "\x00")
	sym = strings.ReplaceAll(sym, " ", "")
	sym = strings.ReplaceAll(sym, "\t", "")
	sym = strings.ToUpper(sym)
	return sym
}

func makeSymbolMapping(ctx context.Context, client *rpc.Client, mints []solana.PublicKey) SymbolMapping {
	symm := SymbolMapping{
		mintToSymbol: make(map[string]string, len(mints)),
		symbolToMint: make(map[string]solana.PublicKey, len(mints)),
		unresolved:   make(map[string]struct{}),
	}
	for _, mint := range mints {
		tokenMeta, err := tokenMetadata(ctx, client, mint)
		if err != nil {
			log.Printf("warning: failed to fetch metadata for mint %s: %v", Addr(mint.String()), err)
		}
		symbol := normalizeSymbol(tokenMeta.Symbol)
		if len(symbol) == 0 {
			symm.unresolved[mint.String()] = struct{}{}
			symbol = normalizeSymbol(mint.String()[:4])
		}
		symm.mintToSymbol[mint.String()] = symbol
		symm.symbolToMint[symbol] = mint
	}
	return symm
}
