// Command channels creates (or retires) channel accounts on-chain. Channels
// are derived from a seed, so this only needs a funding key and the seed; it's
// safe to re-run — existing channels are skipped — which is also how you
// recover after a testnet reset.
//
//	go run ./scripts/channels -n 10                 # ensure channels 0..9 exist
//	go run ./scripts/channels -n 10 -merge-to 25    # also merge channels 10..24 back into the funder
//	go run ./scripts/channels -deposit              # the deposit bridge's channels (make deposit-channels)
//
// By default it serves the gasless service and reads gasless.env (or the
// environment): NETWORK, RPC_URL, FUNDER_ADDRESS, FUNDER_PRIVATE_KEY,
// CHANNEL_SEED, CHANNEL_COUNT. With -deposit it reads .env instead and uses
// DEPOSIT_CHANNEL_SEED and DEPOSIT_CHANNEL_COUNT, funded by pool 1
// (POOL_ADDRESS_1 / POOL_PRIVATE_KEY_1). Deposit channels only need their
// base reserve: the pool pays every fee by fee-bump.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/amount"
	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/gasless/chain"
	"github.com/latch/relayer/internal/gasless/keys"
	"github.com/latch/relayer/internal/signer"
)

const (
	maxCreatesPerTx = 100 // Stellar's per-transaction operation limit
	maxMergesPerTx  = 19  // each merge needs the channel's signature; a tx carries ≤20
)

func main() {
	deposit := flag.Bool("deposit", false, "manage the deposit bridge's channels (.env, DEPOSIT_CHANNEL_*, funded by pool 1)")
	for _, a := range os.Args[1:] {
		if a == "-deposit" || a == "--deposit" || a == "-deposit=true" || a == "--deposit=true" {
			*deposit = true
		}
	}
	envFile, seedVar, countVar := config.GaslessEnvFile, "CHANNEL_SEED", "CHANNEL_COUNT"
	funderKeyVar, funderAddrVar := "FUNDER_PRIVATE_KEY", "FUNDER_ADDRESS"
	if *deposit {
		envFile, seedVar, countVar = ".env", "DEPOSIT_CHANNEL_SEED", "DEPOSIT_CHANNEL_COUNT"
		funderKeyVar, funderAddrVar = "POOL_PRIVATE_KEY_1", "POOL_ADDRESS_1"
	}
	godotenv.Load(envFile)

	n := flag.Int("n", envInt(countVar), "number of channels that should exist (indexes 0..n-1)")
	startXLM := flag.String("start-xlm", "1.5", "starting balance for each new channel (base reserve is 1 XLM)")
	mergeTo := flag.Int("merge-to", 0, "also merge channels n..merge-to-1 back into the funder")
	dryRun := flag.Bool("dry-run", false, "show what would change without submitting")
	flag.Parse()

	if *n <= 0 {
		log.Fatal("channel count required: -n N or CHANNEL_COUNT")
	}
	passphrase := network.TestNetworkPassphrase
	if v := os.Getenv("NETWORK"); v == "mainnet" || v == "pubnet" {
		passphrase = network.PublicNetworkPassphrase
	}
	rpcURL := os.Getenv("RPC_URL")
	if rpcURL == "" {
		rpcURL = "https://soroban-testnet.stellar.org"
	}
	funder := mustKeypair(funderKeyVar, funderAddrVar)
	seed, err := hex.DecodeString(os.Getenv(seedVar))
	if err != nil || len(seed) < keys.MinSeedBytes {
		log.Fatalf("%s must be hex, at least 16 bytes", seedVar)
	}
	if _, err := amount.ParseInt64(*startXLM); err != nil {
		log.Fatalf("-start-xlm: %v", err)
	}

	total := max(*n, *mergeTo)
	chans, err := keys.DeriveChannels(seed, total)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rpc := rpcclient.NewClient(rpcURL, &http.Client{Timeout: 30 * time.Second})
	defer rpc.Close()

	addrs := []string{funder.Address()}
	for _, ch := range chans {
		addrs = append(addrs, ch.Address())
	}
	accts, err := chain.Accounts(ctx, rpc, addrs)
	if err != nil {
		log.Fatal(err)
	}
	if !accts[funder.Address()].Exists {
		log.Fatalf("funder %s does not exist; fund it first (testnet: https://friendbot.stellar.org/?addr=%s)", funder.Address(), funder.Address())
	}

	var toCreate, toMerge []keys.Channel
	for _, ch := range chans {
		exists := accts[ch.Address()].Exists
		switch {
		case ch.Index < *n && !exists:
			toCreate = append(toCreate, ch)
		case ch.Index >= *n && exists:
			toMerge = append(toMerge, ch)
		}
	}
	log.Printf("channels 0..%d: %d exist, %d to create; %d to merge back", *n-1, *n-len(toCreate), len(toCreate), len(toMerge))
	if *dryRun || (len(toCreate) == 0 && len(toMerge) == 0) {
		return
	}

	fs := signer.FromKeypair(funder)
	for start := 0; start < len(toCreate); start += maxCreatesPerTx {
		batch := toCreate[start:min(start+maxCreatesPerTx, len(toCreate))]
		ops := make([]txnbuild.Operation, 0, len(batch))
		for _, ch := range batch {
			ops = append(ops, &txnbuild.CreateAccount{Destination: ch.Address(), Amount: *startXLM})
		}
		if err := submit(ctx, rpc, passphrase, fs, ops, nil); err != nil {
			log.Fatalf("create channels %d..%d: %v", batch[0].Index, batch[len(batch)-1].Index, err)
		}
		log.Printf("created channels %d..%d", batch[0].Index, batch[len(batch)-1].Index)
	}

	for start := 0; start < len(toMerge); start += maxMergesPerTx {
		batch := toMerge[start:min(start+maxMergesPerTx, len(toMerge))]
		ops := make([]txnbuild.Operation, 0, len(batch))
		var chanSigners []signer.Signer
		for _, ch := range batch {
			ops = append(ops, &txnbuild.AccountMerge{Destination: funder.Address(), SourceAccount: ch.Address()})
			chanSigners = append(chanSigners, signer.FromKeypair(ch.Keypair))
		}
		if err := submit(ctx, rpc, passphrase, fs, ops, chanSigners); err != nil {
			log.Fatalf("merge channels: %v", err)
		}
		log.Printf("merged channels %d..%d into the funder", batch[0].Index, batch[len(batch)-1].Index)
	}
}

// submit builds a classic tx from the funder, signs it with the funder plus
// extra signers, sends it via RPC and waits for the result.
func submit(ctx context.Context, rpc *rpcclient.Client, passphrase string, funder signer.Signer, ops []txnbuild.Operation, extra []signer.Signer) error {
	src, err := rpc.LoadAccount(ctx, funder.Address())
	if err != nil {
		return fmt.Errorf("load funder: %w", err)
	}
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        src,
		IncrementSequenceNum: true,
		Operations:           ops,
		BaseFee:              1000, // stroops per op; classic ops are cheap
		Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(120)},
	})
	if err != nil {
		return fmt.Errorf("build tx: %w", err)
	}
	for _, s := range append([]signer.Signer{funder}, extra...) {
		if tx, err = signer.SignTx(ctx, s, tx, passphrase); err != nil {
			return err
		}
	}
	b64, err := tx.Base64()
	if err != nil {
		return err
	}

	sent, err := rpc.SendTransaction(ctx, protocol.SendTransactionRequest{Transaction: b64})
	if err != nil {
		return fmt.Errorf("send: %w", err)
	}
	if sent.Status != "PENDING" && sent.Status != "DUPLICATE" {
		return fmt.Errorf("send status %s: %s", sent.Status, sent.ErrorResultXDR)
	}
	res, err := rpc.PollTransaction(ctx, sent.Hash)
	if err != nil {
		return fmt.Errorf("poll %s: %w", sent.Hash, err)
	}
	if res.Status != protocol.TransactionStatusSuccess {
		return fmt.Errorf("tx %s %s: %s", sent.Hash, res.Status, res.ResultXDR)
	}
	log.Printf("tx %s succeeded in ledger %d", sent.Hash, res.Ledger)
	return nil
}

func mustKeypair(keyVar, addrVar string) *keypair.Full {
	kp, err := keypair.ParseFull(os.Getenv(keyVar))
	if err != nil {
		log.Fatalf("%s: %v", keyVar, err)
	}
	if addr := os.Getenv(addrVar); addr != "" && addr != kp.Address() {
		log.Fatal(errors.New(addrVar + " does not match " + keyVar))
	}
	return kp
}

func envInt(key string) int {
	n, _ := strconv.Atoi(os.Getenv(key))
	return n
}
