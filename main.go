package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"

	"golang.org/x/sync/errgroup"

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

type ConstantProduct struct {
	Token0Vault  solana.PublicKey
	Token0Amount big.Int

	Token1Vault  solana.PublicKey
	Token1Amount big.Int
}

func humanAmount(raw *big.Int, decimals uint8, precision int) string {
	if raw == nil {
		return "0"
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	rat := new(big.Rat).SetFrac(raw, scale)
	return rat.FloatString(precision)
}

// NOTE(@hadydotai): This looks kind of weird but it's intentally that way to avoid side stepping
// the cache-line. We're not sharing ConstantProduct between go routines, each go routine potentially
// running on a single core now owns a copy of the data it needs, not writing into a shared space.
//
// Unnecessary today, but sets us up for a better future. It's also a tiny change really.
// Alternatively I would pad ConstantProduct for the L1 cache line, that's hairier than splitting
// ownership and then rendezvous at a later point to collate the data.
// It also consolidate the logic into a single closure, which means I can blow this up without
// worrying about cache, or size. Batch pulling vault information maybe? At the moment we're dealing with
// one hard coded pool with a pair of vaults.
// TODO(@hadydotai): Actually display in the output table which vault balance did we fail to fetch for and errored out
func poolBalances(ctx context.Context, client *rpc.Client, cp *ConstantProduct) error {
	queries := []solana.PublicKey{
		cp.Token0Vault,
		cp.Token1Vault,
	}
	results := make([]*big.Int, len(queries))

	errg, ctx := errgroup.WithContext(ctx)
	for i := range queries {
		i, vault := i, &queries[i] // NOTE(@hadydotai): order matters here, https://go.dev/ref/spec#For_clause
		errg.Go(func() error {
			resp, err := client.GetTokenAccountBalance(ctx, *vault, rpc.CommitmentFinalized)
			if err != nil {
				return err
			}
			if resp == nil {
				// NOTE(@hadydotai): To the naked eye, it seems unlikely that if we don't have an error then we must
				// have this, well, no. resp is a pointer, defend against the Martians attack and their null pointy fingers.
				// Anytime a pointer presents itself, it's subject to faulty memory. Best we can hope for here is a nil check.
				return fmt.Errorf("vault-%d rpc call getTokenAccountBalance failed, returning empty response", i)
			}
			if resp.Value == nil {
				// NOTE(@hadydotai): Really unsure about this, need to find some failure cases. Is it even possible
				// for a pool to drain completely on Raydium, if so what would that look like at this point here.
				// If not, what could land me here then, parsing error? error on the wire? I don't know. Maybe it's okay
				// to have no value and report a 0 balance. For now we'll error and gracefully show it to the user.
				return fmt.Errorf("vault-%d rpc call getTokenAccountBalance failed, returned no balance", i)
			}
			amount, ok := new(big.Int).SetString(resp.Value.Amount, 10)
			if !ok {
				return fmt.Errorf("vault-%d balance is an invalid amount %q", i, resp.Value.Amount)
			}
			results[i] = amount
			return nil
		})
	}
	if err := errg.Wait(); err != nil {
		return err
	}
	cp.Token0Amount = *results[0]
	cp.Token1Amount = *results[1]
	return nil
}

func main() {
	var (
		rpcEP    = flag.String("rpc", rpc.MainNetBeta_RPC, fmt.Sprintf("RPC to connect to, defaults to %s", rpc.MainNetBeta_RPC))
		poolAddr = flag.String("pool", ourCorePoolAddr, fmt.Sprintf("Pool to interact with, defaults to %s", ourCorePoolAddr))
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

	cp := ConstantProduct{
		Token0Vault: pool.Token0Vault,
		Token1Vault: pool.Token1Vault,
	}

	poolBalances(ctx, client, &cp)

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetTitle(*poolAddr)
	t.SetCaption("CPMM/CP-Swap Raydium Pool")
	t.AppendHeader(table.Row{"", "Token 0", "Token 1"})
	t.AppendRow(table.Row{"Addr", pool.Token0Mint.String(), pool.Token1Mint.String()})
	t.AppendRow(table.Row{"Decimals", pool.Mint0Decimals, pool.Mint1Decimals})
	t.AppendRow(table.Row{
		"Value",
		// Token0
		humanAmount(&cp.Token0Amount, pool.Mint0Decimals, int(pool.Mint0Decimals)),
		// Token1
		humanAmount(&cp.Token1Amount, pool.Mint1Decimals, int(pool.Mint1Decimals)),
	})
	t.Render()
}
