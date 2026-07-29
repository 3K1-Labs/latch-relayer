package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
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

	// ── 1. Create a funding intent ───────────────────────────────────────────
	fmt.Printf("Step 1: Create intent for C-address %s\n", cAddress)
	intentBody := fmt.Sprintf(`{"c_address":"%s","expected_amt":"%s","expires_in":3600}`, cAddress, depositAmount)
	resp, err := relayerPost(relayerURL+"/intents", intentBody)
	if err != nil {
		log.Fatalf("POST /intents: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var intentResp struct {
		IntentID    string `json:"intent_id"`
		MemoID      string `json:"memo_id"`
		PoolAddress string `json:"pool_address"`
		ExpiresAt   string `json:"expires_at"`
		Error       string `json:"error"`
	}
	json.Unmarshal(raw, &intentResp)

	if intentResp.Error != "" {
		log.Fatalf("create intent error: %s", intentResp.Error)
	}
	fmt.Printf("  ✓ intent_id:    %s\n", intentResp.IntentID)
	fmt.Printf("  ✓ memo_id:      %s\n", intentResp.MemoID)
	fmt.Printf("  ✓ pool_address: %s\n", intentResp.PoolAddress)
	fmt.Printf("  ✓ expires_at:   %s\n\n", intentResp.ExpiresAt)

	// ── 2. Send deposit to pool ──────────────────────────────────────────────
	fmt.Printf("Step 2: Send %s XLM from depositor → pool (memo_id: %s)\n", depositAmount, intentResp.MemoID)

	sourceAccount, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: depositor.Address()})
	if err != nil {
		log.Fatalf("fetch depositor account: %v", err)
	}

	var memoIDInt uint64
	fmt.Sscanf(intentResp.MemoID, "%d", &memoIDInt)

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &sourceAccount,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			&txnbuild.Payment{
				Destination: intentResp.PoolAddress,
				Amount:      depositAmount,
				Asset:       txnbuild.NativeAsset{},
			},
		},
		Memo:    txnbuild.MemoID(memoIDInt),
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

	// ── 3. Poll until the intent is completed ────────────────────────────────
	fmt.Println("Step 3: Polling /deposit/status until intent is completed...")
	statusURL := fmt.Sprintf("%s/deposit/status/%s", relayerURL, intentResp.MemoID)

	for attempt := 1; attempt <= 12; attempt++ {
		time.Sleep(5 * time.Second)

		statusResp, err := relayerGet(statusURL)
		if err != nil {
			fmt.Printf("  [%ds] poll error: %v\n", attempt*5, err)
			continue
		}
		body, _ := io.ReadAll(statusResp.Body)
		statusResp.Body.Close()

		var status struct {
			IntentID string `json:"intent_id"`
			MemoID   string `json:"memo_id"`
			CAddress string `json:"c_address"`
			Status   string `json:"status"`
			Forwards []struct {
				TxHash    string  `json:"tx_hash"`
				Amount    string  `json:"amount"`
				Status    string  `json:"status"`
				ForwardTx *string `json:"forward_tx"`
			} `json:"forwards"`
		}
		json.Unmarshal(body, &status)

		if len(status.Forwards) == 0 {
			fmt.Printf("  [%ds] watcher hasn't picked up deposit yet...\n", attempt*5)
			continue
		}

		fwd := status.Forwards[0]
		fmt.Printf("  [%ds] intent: %-12s  forward: %s\n", attempt*5, status.Status, fwd.Status)

		if status.Status == "completed" {
			fmt.Println()
			fmt.Println("=== ✅ END-TO-END SUCCESS ===")
			fmt.Printf("  Intent ID:    %s\n", status.IntentID)
			fmt.Printf("  Deposit tx:   %s\n", fwd.TxHash)
			fmt.Printf("  Forward tx:   %s\n", *fwd.ForwardTx)
			fmt.Printf("  Amount:       %s XLM\n", fwd.Amount)
			fmt.Printf("  C-address:    %s\n", status.CAddress)
			fmt.Printf("  Memo ID:      %s\n", status.MemoID)
			return
		}

		if status.Status == "failed" {
			log.Fatalf("intent failed — check server logs")
		}
	}

	log.Fatal("timed out waiting for intent to complete (60s)")
}

// ── Relayer HTTP helpers ──────────────────────────────────────────────────────
//
// Every route but /health requires the shared secret, so this script needs
// RELAYER_API_KEY set to the same value the server was started with.

func relayerAuth(req *http.Request) *http.Request {
	key := os.Getenv("RELAYER_API_KEY")
	if key == "" {
		log.Fatal("RELAYER_API_KEY is not set — the relayer will reject every request")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

func relayerPost(url, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(relayerAuth(req))
}

func relayerGet(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(relayerAuth(req))
}
