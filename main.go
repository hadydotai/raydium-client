package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"sync"

	"hadydotai/raydium-client/raydium_cp_swap"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jedib0t/go-pretty/v6/table"
)

const (
	wSOLMintAddr    = "So11111111111111111111111111111111111111112"
	ourCorePoolAddr = "3ELLbDZkimZSpnWoWVAfDzeG24yi2LC4sB35ttfNCoEi"
)

var (
	CPMMProgramPubK = solana.MustPublicKeyFromBase58("CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C")
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

func humanAmount(raw *big.Int, decimals uint8, precision int) string {
	if raw == nil {
		return "0"
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	rat := new(big.Rat).SetFrac(raw, scale)
	return rat.FloatString(precision)
}

type poolBalance struct {
	Balance  *big.Int
	Decimals uint8
}

// poolBalances will fetch balances from all vaults concurrently or in parallel depending on how you configure Go exec,
// it's also cpu cache friendly. We don#t side step the cache line, each Go routine owns and mutates its own data, no
// shared data contention resulting in cache evictions
//
// Returns two equal length slices (equals len(vaults)), balances and errors, so they can be indexed over in tandem.
func poolBalances(ctx context.Context, client *rpc.Client, vaults []solana.PublicKey) ([]*poolBalance, []error) {
	results := make([]*poolBalance, len(vaults))
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
			results[i] = &poolBalance{
				Balance:  amount,
				Decimals: resp.Value.Decimals,
			}
		}()
	}
	wg.Wait()
	return results, errs
}
	}
}

func main() {
	var (
		rpcEP    = flag.String("rpc", rpc.MainNetBeta_RPC, "RPC to connect to")
		poolAddr = flag.String("pool", ourCorePoolAddr, "Pool to interact with")
		// mintAddr = flag.String("token", wSOLMintAddr, fmt.Sprintf("Token address to buy/sell, defaults to %s", wSOLMintAddr))
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
	t.AppendRow(table.Row{"Addr", pool.Token0Mint.String(), pool.Token1Mint.String()})

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
	decimals := append([]any{"Decimals"}, mapPtrSliceRetAny(balances, func(elm *poolBalance) any { return elm.Decimals })...)
	t.AppendRow(decimals)
	t.Render()
}
