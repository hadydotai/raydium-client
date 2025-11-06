package main

import (
	"context"
	"errors"
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

// NOTE(@hadydotai): boy will this hurt. This function will likely have high CPU cache contention.
// If we're calling this on a tight loop, we're going to end up with heavy
// CPU cache eviction as multiple cores contesting the shared ConstantProduct structure. Either I split the data,
// or pad to align on cache boundaries.
// For now it's fine, but I reckon when I start hitting this function for every pool, in a very long list of
// pools, getting balances for each pair of vaults, I'll be adding seconds to what otherwise could be a few milliseconds.
// TODO(@hadydotai): Actually display in the output table which vault balance did we fail to fetch for and errored out
func poolBalances(ctx context.Context, client *rpc.Client, cp *ConstantProduct) error {
	errg, ctx := errgroup.WithContext(ctx)
	errg.Go(func() error {
		resp, err := client.GetTokenAccountBalance(ctx, cp.Token0Vault, rpc.CommitmentFinalized)
		if err != nil {
			return err
		}
		if resp == nil {
			// NOTE(@hadydotai): To the naked eye, it seems unlikely that if we don't have an error then we must
			// have this, well, no. resp is a pointer, defend against the Martians attack and their null pointy fingers.
			// Anytime a pointer presents itself, it's subject to faulty memory. Best we can hope for here is a nil check.
			return errors.New("rpc call getTokenAccountBalance failed, returning empty response")
		}
		if resp.Value == nil {
			// NOTE(@hadydotai): Really unsure about this, need to find some failure cases. Is it even possible
			// for a pool to drain completely on Raydium, if so what would that look like at this point here.
			// If not, what could land me here then, parsing error? error on the wire? I don't know. Maybe it's okay
			// to have no value and report a 0 balance. For now we'll error and gracefully show it to the user.
			return errors.New("rpc call getTokenAccountBalance failed, returned no balance")
		}
		cp.Token0Amount.SetString(resp.Value.Amount, 10)
		return nil
	})
	errg.Go(func() error {
		resp, err := client.GetTokenAccountBalance(ctx, cp.Token1Vault, rpc.CommitmentFinalized)
		if err != nil {
			return err
		}
		cp.Token1Amount.SetString(resp.Value.Amount, 10)
		return nil
	})
	if err := errg.Wait(); err != nil {
		return err
	}
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
		cp.Token0Amount.String(),
		// Token1
		cp.Token1Amount.String(),
	})
	t.Render()
}
