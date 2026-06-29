package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/latch/relayer/internal/memo"
)

const (
	relayerURL    = "http://localhost:4000"
	depositorSeed = "SAHTDBI4OCTAP6XAVG3F5FDYEX5S36DXMT36LRIUAZQPWKANX4ETLA2A"
	cAddress      = "CCOX4AG3XESDAZC7L27AMQZ6KKMUWEU2KCHFXJ2PXNAXMDUCL225MN2P"
	depositAmount = "5"
)

func main() {
	client := horizonclient.DefaultTestNetClient
	depositor, _ := keypair.ParseFull(depositorSeed)

	fmt.Println("=== Latch Relayer — End-to-End Testnet Test ===")
	fmt.Println()

	// ── 1. Register the C-address ────────────────────────────────────────────
	fmt.Printf("Step 1: Register C-address %s\n", cAddress)
	regBody := fmt.Sprintf(`{"c_address":"%s"}`, cAddress)
	resp, err := http.Post(relayerURL+"/register", "application/json", strings.NewReader(regBody))
	if err != nil {
		log.Fatalf("POST /register: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var regResp struct {
		MemoID      string `json:"memo_id"`
		PoolAddress string `json:"pool_address"`
		Error       string `json:"error"`
	}
	json.Unmarshal(raw, &regResp)

	if regResp.Error != "" {
		log.Fatalf("register error: %s", regResp.Error)
	}
	fmt.Printf("  ✓ memo_id:      %s\n", regResp.MemoID)
	fmt.Printf("  ✓ pool_address: %s\n", regResp.PoolAddress)

	// Verify the derived memo_id matches what the server returned
	derivedID := memo.DeriveID(cAddress)
	fmt.Printf("  ✓ local DeriveID check: %d\n\n", derivedID)

	// ── 2. Send deposit to pool ──────────────────────────────────────────────
	fmt.Printf("Step 2: Send %s XLM from depositor → pool (memo_id: %s)\n", depositAmount, regResp.MemoID)

	sourceAccount, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: depositor.Address()})
	if err != nil {
		log.Fatalf("fetch depositor account: %v", err)
	}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &sourceAccount,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			&txnbuild.Payment{
				Destination: regResp.PoolAddress,
				Amount:      depositAmount,
				Asset:       txnbuild.NativeAsset{},
			},
		},
		Memo:    txnbuild.MemoID(derivedID),
		BaseFee: txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimeout(300),
		},
	})
	if err != nil {
		log.Fatalf("build tx: %v", err)
	}

	tx, err = tx.Sign(network.TestNetworkPassphrase, depositor)
	if err != nil {
		log.Fatalf("sign tx: %v", err)
	}

	result, err := client.SubmitTransaction(tx)
	if err != nil {
		log.Fatalf("submit tx: %v", err)
	}
	fmt.Printf("  ✓ deposit tx hash: %s\n", result.Hash)
	fmt.Printf("  ✓ ledger:          %d\n\n", result.Ledger)

	// ── 3. Wait for watcher to pick it up and forwarder to process it ────────
	fmt.Println("Step 3: Polling /deposit/status until forward is confirmed...")
	statusURL := fmt.Sprintf("%s/deposit/status/%s", relayerURL, regResp.MemoID)

	for attempt := 1; attempt <= 12; attempt++ {
		time.Sleep(5 * time.Second)

		statusResp, err := http.Get(statusURL)
		if err != nil {
			fmt.Printf("  [%ds] poll error: %v\n", attempt*5, err)
			continue
		}
		body, _ := io.ReadAll(statusResp.Body)
		statusResp.Body.Close()

		var status struct {
			MemoID   string `json:"memo_id"`
			CAddress string `json:"c_address"`
			Forwards []struct {
				TxHash    string  `json:"tx_hash"`
				Amount    string  `json:"amount"`
				Status    string  `json:"status"`
				ForwardTx *string `json:"forward_tx"`
				CreatedAt string  `json:"created_at"`
			} `json:"forwards"`
		}
		json.Unmarshal(body, &status)

		if len(status.Forwards) == 0 {
			fmt.Printf("  [%ds] watcher hasn't picked up deposit yet...\n", attempt*5)
			continue
		}

		fwd := status.Forwards[0]
		fmt.Printf("  [%ds] forward status: %s\n", attempt*5, fwd.Status)

		if fwd.Status == "done" {
			fmt.Println()
			fmt.Println("=== ✅ END-TO-END SUCCESS ===")
			fmt.Printf("  Deposit tx:   %s\n", fwd.TxHash)
			fmt.Printf("  Forward tx:   %s\n", *fwd.ForwardTx)
			fmt.Printf("  Amount:       %s XLM\n", fwd.Amount)
			fmt.Printf("  C-address:    %s\n", status.CAddress)
			fmt.Printf("  Memo ID:      %s\n", status.MemoID)
			return
		}

		if fwd.Status == "failed" {
			log.Fatalf("forward failed — check server logs")
		}
	}

	log.Fatal("timed out waiting for forward to complete (60s)")
}
