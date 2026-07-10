package main

import (
	"fmt"
	"log"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/latch/relayer/internal/memo"
)

const (
	horizonURL    = "https://horizon-testnet.stellar.org"
	depositorSeed = "SAHTDBI4OCTAP6XAVG3F5FDYEX5S36DXMT36LRIUAZQPWKANX4ETLA2A"
	poolAddress   = "GB3AETG6Q5SYNM36TPHULYXPP364EC77YJJBDMNKDA5CF4QYP3XHJ6I5"
)

func main() {
	client := horizonclient.DefaultTestNetClient

	// ── 1. Load depositor keypair ────────────────────────────────────────────
	depositor, err := keypair.ParseFull(depositorSeed)
	if err != nil {
		log.Fatalf("parse depositor seed: %v", err)
	}
	fmt.Println("Depositor:", depositor.Address())

	// ── 2. Use a fixed test memo_id ──────────────────────────────────────────
	// In production, memo_ids come from POST /intents. This script uses a
	// hardcoded value to test the send → confirm → ParseID round-trip only.
	const memoID uint64 = 1234567890
	fmt.Printf("Memo ID (test value): %d\n", memoID)

	// ── 3. Fetch depositor account (we need the sequence number) ────────────
	accountReq := horizonclient.AccountRequest{AccountID: depositor.Address()}
	sourceAccount, err := client.AccountDetail(accountReq)
	if err != nil {
		log.Fatalf("fetch account: %v", err)
	}
	fmt.Printf("Depositor sequence: %s\n", sourceAccount.Sequence)

	// ── 4. Build the transaction ─────────────────────────────────────────────
	tx, err := txnbuild.NewTransaction(
		txnbuild.TransactionParams{
			SourceAccount:        &sourceAccount,
			IncrementSequenceNum: true,
			Operations: []txnbuild.Operation{
				&txnbuild.Payment{
					Destination: poolAddress,
					Amount:      "10",
					Asset:       txnbuild.NativeAsset{},
				},
			},
			Memo:    txnbuild.MemoID(memoID),
			BaseFee: txnbuild.MinBaseFee,
			Preconditions: txnbuild.Preconditions{
				TimeBounds: txnbuild.NewTimeout(300),
			},
		},
	)
	if err != nil {
		log.Fatalf("build transaction: %v", err)
	}

	// ── 5. Sign ──────────────────────────────────────────────────────────────
	tx, err = tx.Sign(network.TestNetworkPassphrase, depositor)
	if err != nil {
		log.Fatalf("sign transaction: %v", err)
	}

	// ── 6. Submit ────────────────────────────────────────────────────────────
	fmt.Println("\nSubmitting transaction...")
	result, err := client.SubmitTransaction(tx)
	if err != nil {
		log.Fatalf("submit transaction: %v", err)
	}
	fmt.Println("✓ Transaction submitted!")
	fmt.Println("  Hash:       ", result.Hash)
	fmt.Println("  Ledger:     ", result.Ledger)
	fmt.Println("  Successful: ", result.Successful)

	// ── 7. Confirm the pool received it ──────────────────────────────────────
	fmt.Println("\nChecking pool account received the payment...")
	time.Sleep(6 * time.Second)

	paymentsReq := horizonclient.OperationRequest{
		ForAccount: poolAddress,
		Order:      horizonclient.OrderDesc,
		Limit:      1,
	}
	ops, err := client.Operations(paymentsReq)
	if err != nil {
		log.Fatalf("fetch operations: %v", err)
	}

	if len(ops.Embedded.Records) == 0 {
		log.Fatal("no operations found on pool account")
	}

	latest := ops.Embedded.Records[0]
	fmt.Printf("✓ Latest operation on pool account:\n")
	fmt.Printf("  Type:   %s\n", latest.GetType())
	fmt.Printf("  TxHash: %s\n", latest.GetTransactionHash())

	// ── 8. Fetch the transaction to verify the memo ───────────────────────────
	txDetail, err := client.TransactionDetail(result.Hash)
	if err != nil {
		log.Fatalf("fetch transaction: %v", err)
	}
	fmt.Printf("\n✓ Transaction memo:\n")
	fmt.Printf("  Type:  %s\n", txDetail.MemoType)
	fmt.Printf("  Value: %s\n", txDetail.Memo)

	// ── 9. Round-trip: parse the memo back ───────────────────────────────────
	parsed, err := memo.ParseID(txDetail.MemoType, txDetail.Memo)
	if err != nil {
		log.Fatalf("parse memo from confirmed tx: %v", err)
	}
	if parsed != memoID {
		log.Fatalf("memo mismatch: sent %d, got back %d", memoID, parsed)
	}
	fmt.Printf("\n✅ Round-trip success: send → confirm → ParseID = %d\n", parsed)
}
