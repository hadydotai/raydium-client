package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

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

	poolData, err := raydium_cp_swap.ParseAccount_PoolState(accountInfo.Value.Data.GetBinary())
	if err != nil {
		log.Fatalf("parsing PoolState failed, make sure the pool address you passed is a Raydium CP-Swap/CPMM pool: %s\n", err)
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetTitle(*poolAddr)
	t.SetCaption("CPMM/CP-Swap Raydium Pool")
	t.AppendHeader(table.Row{"", "Token 0", "Token 1"})
	t.AppendRow(table.Row{"Addr", poolData.Token0Mint.String(), poolData.Token1Mint.String()})
	t.AppendRow(table.Row{"Decimals", poolData.Mint0Decimals, poolData.Mint1Decimals})

	t.Render()
}
