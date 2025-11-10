package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"strings"
	"time"

	"hadydotai/raydium-client/raydium_cp_swap"

	solana "github.com/gagliardetto/solana-go"
	atapkg "github.com/gagliardetto/solana-go/programs/associated-token-account"
	computebudget "github.com/gagliardetto/solana-go/programs/compute-budget"
	"github.com/gagliardetto/solana-go/programs/system"
	tokenprog "github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	"github.com/jedib0t/go-pretty/v6/table"
)

const (
	RaydiumProgramID = iota
	DefaultRPC
)

const (
	wSOLMintAddr Addr = "So11111111111111111111111111111111111111112"
	feeRateDenom      = int64(1_000_000) // https://github.com/raydium-io/raydium-cp-swap/blob/master/programs/cp-swap/src/curve/fees.rs#L3
)

var (
	ATAProgramID     = atapkg.ProgramID
	SystemProgramID  = system.ProgramID
	DefaultUnitLimit = uint32(200_000_000) // rough ballpark, https://solana.com/docs/core/fees and from simulating a few transactions
	DefaultUnitPrice = uint64(5000)        // micro-lamports, 0.005 lamports

	wSOLMint = solana.MustPublicKeyFromBase58(string(wSOLMintAddr))

	networks = map[string]map[int]any{
		"devnet": {
			RaydiumProgramID: solana.MustPublicKeyFromBase58("DRaycpLY18LhpbydsBWbVJtxpNv9oXPgjRSfpF2bWpYb"),
			DefaultRPC:       rpc.DevNet_RPC,
		},
		"mainnet": {
			RaydiumProgramID: solana.MustPublicKeyFromBase58("CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C"),
			DefaultRPC:       rpc.MainNetBeta_RPC,
		},
	}
)

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

func promptSymbolMappingCLI(symbol string, mint string) (bool, error) {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Printf("Symbol %s is unknown. Map it to mint %s (%s)? [y/n]: ", symbol, mint, Addr(mint))
		resp, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return false, err
			}
			return false, err
		}
		resp = strings.ToLower(strings.TrimSpace(resp))
		switch resp {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Println("Please answer y or n.")
		}
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

type txSummaryData struct {
	Signature        solana.Signature
	Status           string
	FeeLamports      uint64
	PaidAmount       *big.Int
	PaidDecimals     uint8
	PaidSymbol       string
	ReceivedAmount   *big.Int
	ReceivedDecimals uint8
	ReceivedSymbol   string
}

func renderTxSummary(data txSummaryData) string {
	builder := &strings.Builder{}
	t := table.NewWriter()
	t.SetOutputMirror(builder)
	t.SetTitle("Swap Result")
	t.AppendHeader(table.Row{"Field", "Value"})
	t.AppendRow(table.Row{"Signature", data.Signature.String()})
	status := data.Status
	if status == "" {
		status = "pending"
	}
	t.AppendRow(table.Row{"Status", strings.ToUpper(status)})
	t.AppendRow(table.Row{"Paid", formatTokenAmount(data.PaidAmount, data.PaidDecimals, data.PaidSymbol)})
	t.AppendRow(table.Row{"Received", formatTokenAmount(data.ReceivedAmount, data.ReceivedDecimals, data.ReceivedSymbol)})
	feeStr := "n/a"
	if data.FeeLamports > 0 {
		feeStr = formatLamports(data.FeeLamports)
	}
	t.AppendRow(table.Row{"Fee", feeStr})
	t.Render()
	return builder.String()
}

func formatTokenAmount(amount *big.Int, decimals uint8, symbol string) string {
	if amount == nil {
		return "n/a"
	}
	precision := int(decimals)
	if precision > 8 {
		precision = 8
	}
	if precision < 2 {
		precision = 2
	}
	return fmt.Sprintf("%s %s", fmtForDisplay(amount, decimals, precision), symbol)
}

func formatLamports(lamports uint64) string {
	val := new(big.Int).SetUint64(lamports)
	return fmt.Sprintf("%s SOL", fmtForDisplay(val, 9, 9))
}

func chooseDecimals(primary uint8, fallback uint8) uint8 {
	if primary != 0 {
		return primary
	}
	return fallback
}

func tokenBalanceAmount(balances []rpc.TokenBalance, accountIndex int, mint solana.PublicKey) (*big.Int, uint8, bool) {
	for _, bal := range balances {
		if int(bal.AccountIndex) != accountIndex {
			continue
		}
		if !bal.Mint.Equals(mint) {
			continue
		}
		amount, ok := new(big.Int).SetString(bal.UiTokenAmount.Amount, 10)
		if !ok {
			return nil, 0, false
		}
		return amount, bal.UiTokenAmount.Decimals, true
	}
	return nil, 0, false
}

func tokenDeltaFromResult(result *rpc.GetTransactionResult, account solana.PublicKey, mint solana.PublicKey) (*big.Int, uint8, bool) {
	if result == nil || result.Meta == nil || result.Transaction == nil {
		return nil, 0, false
	}
	tx, err := result.Transaction.GetTransaction()
	if err != nil || tx == nil {
		return nil, 0, false
	}
	accountIndex := -1
	for i, key := range tx.Message.AccountKeys {
		if key.Equals(account) {
			accountIndex = i
			break
		}
	}
	if accountIndex == -1 {
		return nil, 0, false
	}
	pre, decs, hasPre := tokenBalanceAmount(result.Meta.PreTokenBalances, accountIndex, mint)
	post, _, hasPost := tokenBalanceAmount(result.Meta.PostTokenBalances, accountIndex, mint)
	if !hasPre && !hasPost {
		return nil, 0, false
	}
	if !hasPre {
		pre = big.NewInt(0)
	}
	if !hasPost {
		post = big.NewInt(0)
	}
	delta := new(big.Int).Sub(post, pre)
	return delta, decs, true
}

func waitForTransactionResult(ctx context.Context, client *rpc.Client, sig solana.Signature) (string, *rpc.GetTransactionResult, error) {
	status := "pending"
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		select {
		case <-waitCtx.Done():
			return status, nil, waitCtx.Err()
		default:
			resp, err := client.GetTransaction(waitCtx, sig, &rpc.GetTransactionOpts{
				Encoding:   solana.EncodingBase64,
				Commitment: rpc.CommitmentConfirmed,
			})
			if err != nil {
				if errors.Is(err, rpc.ErrNotFound) {
					time.Sleep(500 * time.Millisecond)
					continue
				}
				return status, nil, err
			}
			status = deriveSignatureStatus(waitCtx, client, sig, resp)
			return status, resp, nil
		}
	}
}

func deriveSignatureStatus(ctx context.Context, client *rpc.Client, sig solana.Signature, result *rpc.GetTransactionResult) string {
	if result != nil && result.Meta != nil && result.Meta.Err != nil {
		return "failed"
	}
	resp, err := client.GetSignatureStatuses(ctx, false, sig)
	if err != nil || resp == nil || len(resp.Value) == 0 || resp.Value[0] == nil {
		if result != nil && result.Meta != nil && result.Meta.Err == nil {
			return "confirmed"
		}
		return "pending"
	}
	val := resp.Value[0]
	if val.Err != nil {
		return "failed"
	}
	switch val.ConfirmationStatus {
	case rpc.ConfirmationStatusFinalized:
		return "finalized"
	case rpc.ConfirmationStatusConfirmed:
		return "confirmed"
	case rpc.ConfirmationStatusProcessed:
		return "processed"
	default:
		return string(val.ConfirmationStatus)
	}
}

func main() {
	var (
		hotwalletPath = flag.String("hotwallet", "", "Path to the hotwallet to use for signing transactions")
		rpcEP         = flag.String("rpc", rpc.DevNet_RPC, "RPC to connect to")
		network       = flag.String("network", "devnet", "Network to connect to, accepted values are 'mainnet', or 'devnet'")
		poolAddr      = flag.String("pool", "", "Pool to interact with")
		intentLine    = flag.String("intent", "", "Intent (<verb> <amount> <token-symbol>), e.g. \"pay 1 SOL\"")
		slippagePct   = flag.Float64("slippage", 0.5, "Slippage tolerance percentage (e.g. 0.5 for 0.5%)")
		noTUI         = flag.Bool("no-tui", false, "Don't enter TUI")
	)
	flag.Parse()

	validations := []FlagSpec{
		{Name: "rpc", Value: rpcEP, Rules: []FlagRule{Requires("network")}},
		{Name: "network", Value: network, Rules: []FlagRule{NotEmpty(), OneOf("mainnet", "devnet")}},
		{Name: "hotwallet", Value: hotwalletPath, Rules: []FlagRule{NotEmpty()}},
		{Name: "pool", Value: poolAddr, Rules: []FlagRule{NotEmpty()}},
	}
	if *noTUI {
		validations = append(validations, FlagSpec{Name: "intent", Value: intentLine, Rules: []FlagRule{NotEmpty()}})
	}
	ValidateConfigOrExit(flag.CommandLine, validations)

	raydium_cp_swap.ProgramID = networks[*network][RaydiumProgramID].(solana.PublicKey)
	if len(*rpcEP) == 0 {
		*rpcEP = networks[*network][DefaultRPC].(string)
	}
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
	symm := makeSymbolMapping(ctx, client, tokenMints)

	builder := &TableBuilder{
		ctx:               ctx,
		client:            client,
		pool:              pool,
		poolAmmConfig:     poolAmmConfig,
		poolAddress:       *poolAddr,
		poolPubKey:        poolPubK,
		tokenOrder:        tokenMints,
		symm:              symm,
		userSymbolAliases: make(map[string]solana.PublicKey),
	}
	if err := builder.SetSlippagePct(*slippagePct); err != nil {
		log.Fatalf("invalid slippage: %s\n", err)
	}

	var (
		report     string
		intentMeta *CPIntent
	)

	if *noTUI {
		for {
			report, intentMeta, err = builder.Build(*intentLine)
			if err == nil {
				break
			}
			var mapErr *MissingSymbolMappingError
			if errors.As(err, &mapErr) {
				mapped, promptErr := promptSymbolMappingCLI(mapErr.Symbol, mapErr.Mint)
				if promptErr != nil {
					log.Fatalf("failed to prompt for symbol mapping: %s\n", promptErr)
				}
				if !mapped {
					log.Fatalf("symbol %s remains unmapped; aborting\n", mapErr.Symbol)
				}
				symm.MapSymToMint(mapErr.Symbol, mapErr.Mint)
				continue
			}
			log.Fatalf("building intent report failed: %s\n", err)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s", report)
	} else {
		ui := newTermUI(builder)
		intentMeta, report, err = ui.Run(*intentLine)
		if err != nil {
			log.Fatalf("interactive UI failed: %s\n", err)
		}
		if intentMeta == nil { // user has chosen to reject or bailout
			log.Println("Aborting...")
			os.Exit(0)
		}
		if report != "" {
			fmt.Fprintln(os.Stdout, report)
		}
	}
	if intentMeta == nil {
		log.Fatalln("intent resolution failed, no transaction to build")
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
	swapIx, err := intentMeta.BuildSwapInstruction(
		payerPub,
		auth,
		inATA,
		outATA,
	)
	if err != nil {
		log.Fatalf("failed to build swap instruction: %s\n", err)
	}

	// NOTE(@hadydotai): I guess we don't need this, but maybe we can expose it to the user
	cb1 := computebudget.NewSetComputeUnitLimitInstruction(uint32(DefaultUnitLimit)).Build()
	cb2 := computebudget.NewSetComputeUnitPriceInstruction(DefaultUnitPrice).Build()

	requiredInput := intentMeta.RequiredInputAmount()
	if requiredInput == nil {
		log.Fatalln("required input amount missing for swap")
	}
	wrapIxs, err := wrapNativeIfNeeded(ctx, client, payerPub, inATA, intentMeta.TokenInMint, requiredInput)
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
	status, txResult, waitErr := waitForTransactionResult(ctx, client, sig)
	if waitErr != nil && !errors.Is(waitErr, context.DeadlineExceeded) && !errors.Is(waitErr, context.Canceled) {
		log.Printf("warning: waiting for transaction confirmation failed: %v", waitErr)
	}
	var txMeta *rpc.TransactionMeta
	if txResult != nil {
		txMeta = txResult.Meta
	}
	feeLamports := uint64(0)
	if txMeta != nil {
		feeLamports = txMeta.Fee
	}
	var paidDelta, receivedDelta *big.Int
	var paidDecimals, receivedDecimals uint8
	if delta, decs, ok := tokenDeltaFromResult(txResult, intentMeta.TokenInVault, intentMeta.TokenInMint); ok {
		if delta.Sign() < 0 {
			delta.Neg(delta)
		}
		paidDelta = delta
		paidDecimals = decs
	}
	if delta, decs, ok := tokenDeltaFromResult(txResult, intentMeta.TokenOutVault, intentMeta.TokenOutMint); ok {
		if delta.Sign() > 0 {
			delta = new(big.Int).Neg(delta)
		}
		receivedDelta = new(big.Int).Abs(delta)
		receivedDecimals = decs
	}
	summary := renderTxSummary(txSummaryData{
		Signature:        sig,
		Status:           status,
		FeeLamports:      feeLamports,
		PaidAmount:       paidDelta,
		PaidDecimals:     chooseDecimals(paidDecimals, intentMeta.TokenInDecimals),
		PaidSymbol:       symm.SymFrom(intentMeta.TokenInMint),
		ReceivedAmount:   receivedDelta,
		ReceivedDecimals: chooseDecimals(receivedDecimals, intentMeta.TokenOutDecimals),
		ReceivedSymbol:   symm.SymFrom(intentMeta.TokenOutMint),
	})
	fmt.Fprintln(os.Stdout, summary)
}
