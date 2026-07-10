package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

const (
	relayerURL    = "http://localhost:4000"
	depositorSeed = "SAHTDBI4OCTAP6XAVG3F5FDYEX5S36DXMT36LRIUAZQPWKANX4ETLA2A"
	cAddress      = "CCOX4AG3XESDAZC7L27AMQZ6KKMUWEU2KCHFXJ2PXNAXMDUCL225MN2P"
	depositAmount = "1"
)

func main() {
	_ = godotenv.Load()

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL not set")
	}

	ctx := context.Background()
	dbPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer dbPool.Close()

	client := horizonclient.DefaultTestNetClient
	depositor, _ := keypair.ParseFull(depositorSeed)

	fmt.Println("=== Latch Relayer — Sweep / Recovery Test ===")
	fmt.Println()

	// ── Get pool address from a throwaway intent ──────────────────────────────
	fmt.Println("Step 1: Fetching pool address from relayer...")
	body := fmt.Sprintf(`{"c_address":"%s","expires_in":60}`, cAddress)
	resp, err := http.Post(relayerURL+"/intents", "application/json", strings.NewReader(body))
	if err != nil {
		log.Fatalf("POST /intents: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var intentResp struct {
		PoolAddress string `json:"pool_address"`
		Error       string `json:"error"`
	}
	json.Unmarshal(raw, &intentResp)
	if intentResp.Error != "" {
		log.Fatalf("create intent error: %s", intentResp.Error)
	}
	poolAddress := intentResp.PoolAddress
	fmt.Printf("  ✓ pool_address: %s\n\n", poolAddress)

	// ── Case 1: MEMO_TEXT (wrong memo type) ───────────────────────────────────
	// The watcher calls ParseID → ErrNotMemoID → forwarder.Forward(..., memoID=0)
	// → GetIntentByMemoID(0) → no rows → sweep + mark forward failed.
	fmt.Println("Step 2: Sending deposit with MEMO_TEXT \"sweep-test\" (wrong memo type)...")
	acc1, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: depositor.Address()})
	if err != nil {
		log.Fatalf("fetch account: %v", err)
	}
	tx1, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &acc1,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			&txnbuild.Payment{Destination: poolAddress, Amount: depositAmount, Asset: txnbuild.NativeAsset{}},
		},
		Memo:    txnbuild.MemoText("sweep-test"),
		BaseFee: txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
	if err != nil {
		log.Fatalf("build tx1: %v", err)
	}
	tx1, err = tx1.Sign(network.TestNetworkPassphrase, depositor)
	if err != nil {
		log.Fatalf("sign tx1: %v", err)
	}
	r1, err := client.SubmitTransaction(tx1)
	if err != nil {
		log.Fatalf("submit tx1: %v", err)
	}
	txHash1 := r1.Hash
	fmt.Printf("  ✓ tx_hash: %s\n\n", txHash1)

	// ── Case 2: Unknown MEMO_ID (not in intents table) ────────────────────────
	// ParseID succeeds → forwarder.Forward(..., memoID=unknownMemoID)
	// → GetIntentByMemoID → pgx.ErrNoRows → sweep + mark forward failed.
	unknownMemoID := uint64(rand.Int64()) // non-negative, safe int64 cast
	fmt.Printf("Step 3: Sending deposit with unknown MEMO_ID (%d)...\n", unknownMemoID)
	acc2, err := client.AccountDetail(horizonclient.AccountRequest{AccountID: depositor.Address()})
	if err != nil {
		log.Fatalf("reload account: %v", err)
	}
	tx2, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &acc2,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			&txnbuild.Payment{Destination: poolAddress, Amount: depositAmount, Asset: txnbuild.NativeAsset{}},
		},
		Memo:    txnbuild.MemoID(unknownMemoID),
		BaseFee: txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewTimeout(300)},
	})
	if err != nil {
		log.Fatalf("build tx2: %v", err)
	}
	tx2, err = tx2.Sign(network.TestNetworkPassphrase, depositor)
	if err != nil {
		log.Fatalf("sign tx2: %v", err)
	}
	r2, err := client.SubmitTransaction(tx2)
	if err != nil {
		log.Fatalf("submit tx2: %v", err)
	}
	txHash2 := r2.Hash
	fmt.Printf("  ✓ tx_hash: %s\n\n", txHash2)

	// ── Wait for the watcher to pick up both deposits ─────────────────────────
	fmt.Println("Step 4: Waiting 20s for watcher to process both deposits...")
	time.Sleep(20 * time.Second)
	fmt.Println()

	// ── Check both forward records in the DB ──────────────────────────────────
	fmt.Println("Step 5: Verifying forward records in DB...")
	ok1 := checkForward(ctx, dbPool, txHash1, "MEMO_TEXT (wrong type)")
	ok2 := checkForward(ctx, dbPool, txHash2, fmt.Sprintf("unknown MEMO_ID %d", unknownMemoID))

	fmt.Println()
	if ok1 && ok2 {
		fmt.Println("=== ✅ SWEEP TEST PASSED ===")
	} else {
		log.Fatal("=== ❌ SWEEP TEST FAILED — see above ===")
	}
}

func checkForward(ctx context.Context, pool *pgxpool.Pool, txHash, label string) bool {
	var status string
	var errMsg *string
	err := pool.QueryRow(ctx,
		`SELECT status, error FROM forwards WHERE tx_hash = $1`, txHash,
	).Scan(&status, &errMsg)
	if err != nil {
		fmt.Printf("  ✗ [%s] not found in forwards table: %v\n", label, err)
		return false
	}
	errStr := "<nil>"
	if errMsg != nil {
		errStr = *errMsg
	}
	if status != "failed" {
		fmt.Printf("  ✗ [%s] expected status=failed, got %q (error: %q)\n", label, status, errStr)
		return false
	}
	fmt.Printf("  ✓ [%s] status=failed, error=%q\n", label, errStr)
	return true
}
