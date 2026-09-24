package forwarder

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/store"
)

// Once a forward's transfer may be in flight, nothing may build a second one
// until the first is known never to land — otherwise one deposit pays the
// C-address twice (#50).

func forwarderWith(t *testing.T, rpc *mockRPC) (*Forwarder, *mockStore) {
	t.Helper()
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	return &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: rpc}, st
}

func forward(f *Forwarder, inbound string) {
	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, inbound, 1, "GABC", "10.0000000", "native")
}

func TestForward_pollTimeoutDoesNotSendTwice(t *testing.T) {
	rpc := successRPC(t, "out-hash")
	rpc.pollErr = context.DeadlineExceeded
	f, st := forwarderWith(t, rpc)

	before := time.Now()
	forward(f, "in-poll")

	if rpc.sent != 1 {
		t.Fatalf("SendTransaction calls = %d, want 1: a second transfer can land alongside the first", rpc.sent)
	}
	if len(st.requeueCalls) != 1 || len(st.markFailedCalls) != 0 || len(st.doneCalls) != 0 {
		t.Fatalf("want one uncharged requeue; requeue=%d markFailed=%d done=%d",
			len(st.requeueCalls), len(st.markFailedCalls), len(st.doneCalls))
	}
	if len(st.recordCalls) != 1 || len(st.clearCalls) != 0 {
		t.Fatalf("want the transfer recorded and kept; record=%d clear=%d", len(st.recordCalls), len(st.clearCalls))
	}
	rec := st.recordCalls[0]
	if rec.txHash != "in-poll" || rec.submittedTx == "" {
		t.Errorf("record = %+v", rec)
	}
	if rec.until.Before(before) || rec.until.After(before.Add(txValidity+time.Second)) {
		t.Errorf("valid-until %s not within txValidity of %s", rec.until, before)
	}
}

func TestForward_sendErrorDoesNotSendTwice(t *testing.T) {
	rpc := successRPC(t, "out-hash")
	rpc.sendErr = errors.New("connection reset")
	f, st := forwarderWith(t, rpc)

	forward(f, "in-send")

	if rpc.sent != 1 {
		t.Fatalf("SendTransaction calls = %d, want 1: the first request may have reached the network", rpc.sent)
	}
	if len(st.requeueCalls) != 1 || len(st.clearCalls) != 0 {
		t.Fatalf("requeue=%d clear=%d, want 1 and 0", len(st.requeueCalls), len(st.clearCalls))
	}
}

func TestForward_nothingSentWithoutRecord(t *testing.T) {
	rpc := successRPC(t, "out-hash")
	f, st := forwarderWith(t, rpc)
	st.recordErr = errors.New("db down")

	forward(f, "in-norecord")

	if rpc.sent != 0 {
		t.Fatalf("sent %d transactions without recording them", rpc.sent)
	}
	if len(st.markFailedCalls) != 1 || st.markFailedCalls[0].status != store.StatusPendingRetry {
		t.Fatalf("want a transient pending_retry, got %+v", st.markFailedCalls)
	}
}

func TestForward_rejectionClearsSubmission(t *testing.T) {
	rpc := successRPC(t, "out-hash")
	rpc.sendResp = rpcprotocol.SendTransactionResponse{
		Status:         "ERROR",
		ErrorResultXDR: makeResultXDR(t, xdr.TransactionResultCodeTxBadAuth),
	}
	f, st := forwarderWith(t, rpc)

	forward(f, "in-reject")

	if len(st.clearCalls) != 1 || st.clearCalls[0] != "in-reject" {
		t.Fatalf("clear calls = %v, want the rejected transfer forgotten", st.clearCalls)
	}
	if len(st.markFailedCalls) != 1 || st.markFailedCalls[0].status != store.StatusFailed {
		t.Fatalf("want permanent failure, got %+v", st.markFailedCalls)
	}
}

func TestForward_failedOnChainClearsSubmission(t *testing.T) {
	rpc := successRPC(t, "out-hash")
	rpc.pollResp = rpcprotocol.GetTransactionResponse{TransactionDetails: rpcprotocol.TransactionDetails{
		Status:    rpcprotocol.TransactionStatusFailed,
		ResultXDR: makeResultXDR(t, xdr.TransactionResultCodeTxBadAuth),
	}}
	f, st := forwarderWith(t, rpc)

	forward(f, "in-failed")

	if len(st.clearCalls) != 1 {
		t.Fatalf("clear calls = %v, want the failed transfer forgotten", st.clearCalls)
	}
}

// ── Retry resolves an in-flight transfer before anything else ─────────────────

func inFlight(retries int, until time.Time) store.Forward {
	hash := "submitted-hash"
	return store.Forward{
		TxHash: "in-flight", MemoID: 1, Amount: "10.0000000", Asset: "native",
		Retries: retries, SubmittedTx: &hash, SubmittedUntil: &until,
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now().Add(-time.Minute),
	}
}

func lookup(status, resultXDR string) rpcprotocol.GetTransactionResponse {
	return rpcprotocol.GetTransactionResponse{TransactionDetails: rpcprotocol.TransactionDetails{
		Status: status, ResultXDR: resultXDR,
	}}
}

func TestRetry_inFlightLandedIsDone(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusSuccess, "")
	f, st := forwarderWith(t, rpc)

	// At the ceiling: a transfer that landed must be recorded as done, not failed.
	f.Retry(context.Background(), inFlight(maxRetries, time.Now().Add(time.Minute)))

	if rpc.sent != 0 {
		t.Fatalf("sent %d new transfers for one that already landed", rpc.sent)
	}
	if len(st.doneCalls) != 1 || st.doneCalls[0] != [2]string{"in-flight", "submitted-hash"} {
		t.Fatalf("done calls = %v", st.doneCalls)
	}
	if len(st.permFailCalls) != 0 || len(st.completeIntentCalls) != 1 {
		t.Fatalf("permFail=%d completeIntent=%d", len(st.permFailCalls), len(st.completeIntentCalls))
	}
	if len(rpc.looked) != 1 || rpc.looked[0] != "submitted-hash" {
		t.Errorf("looked up %v", rpc.looked)
	}
}

func TestRetry_inFlightNotFoundWithinWindowWaits(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusNotFound, "")
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, time.Now().Add(time.Minute)))

	if rpc.sent != 0 || len(st.clearCalls) != 0 {
		t.Fatalf("sent=%d clear=%d while the first transfer can still land", rpc.sent, len(st.clearCalls))
	}
	if len(st.requeueCalls) != 1 || len(st.markFailedCalls) != 0 {
		t.Fatalf("want one uncharged requeue; requeue=%d markFailed=%d", len(st.requeueCalls), len(st.markFailedCalls))
	}
}

func TestRetry_inFlightWaitsOutLandingGrace(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusNotFound, "")
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, time.Now().Add(-landingGrace/2)))

	if rpc.sent != 0 || len(st.requeueCalls) != 1 {
		t.Fatalf("sent=%d requeue=%d: must wait out the grace after max time", rpc.sent, len(st.requeueCalls))
	}
}

func TestRetry_inFlightExpiredIsRebuilt(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusNotFound, "")
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, time.Now().Add(-landingGrace-time.Second)))

	if len(st.clearCalls) != 1 || rpc.sent != 1 {
		t.Fatalf("clear=%d sent=%d, want the expired transfer forgotten and one rebuilt", len(st.clearCalls), rpc.sent)
	}
	if len(st.doneCalls) != 1 || st.doneCalls[0][1] != "new-hash" {
		t.Fatalf("done calls = %v", st.doneCalls)
	}
}

func TestRetry_inFlightFallsBackToUpdatedAt(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusNotFound, "")
	f, st := forwarderWith(t, rpc)

	fwd := inFlight(0, time.Time{})
	fwd.SubmittedUntil = nil
	fwd.UpdatedAt = time.Now()
	f.Retry(context.Background(), fwd)

	if rpc.sent != 0 || len(st.requeueCalls) != 1 {
		t.Fatalf("sent=%d requeue=%d: a just-recorded transfer must be waited for", rpc.sent, len(st.requeueCalls))
	}
}

func TestRetry_inFlightFailedPermanently(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusFailed, makeResultXDR(t, xdr.TransactionResultCodeTxBadAuth))
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, time.Now().Add(time.Minute)))

	if rpc.sent != 0 {
		t.Fatalf("sent %d after a permanent on-chain failure", rpc.sent)
	}
	if len(st.permFailCalls) != 1 || len(st.failIntentCalls) != 1 {
		t.Fatalf("permFail=%d failIntent=%d", len(st.permFailCalls), len(st.failIntentCalls))
	}
}

func TestRetry_inFlightFailedTransientlyIsRebuilt(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusFailed, makeResultXDR(t, xdr.TransactionResultCodeTxInternalError))
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, time.Now().Add(time.Minute)))

	if len(st.clearCalls) != 1 || rpc.sent != 1 || len(st.doneCalls) != 1 {
		t.Fatalf("clear=%d sent=%d done=%d, want one rebuild", len(st.clearCalls), rpc.sent, len(st.doneCalls))
	}
}

func TestRetry_inFlightLookupErrorWaits(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getErr = errors.New("rpc unavailable")
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, time.Now().Add(-time.Hour)))

	if rpc.sent != 0 || len(st.clearCalls) != 0 || len(st.requeueCalls) != 1 {
		t.Fatalf("sent=%d clear=%d requeue=%d: an unanswered lookup proves nothing",
			rpc.sent, len(st.clearCalls), len(st.requeueCalls))
	}
}

func TestRetry_pollTimeoutDoesNotSendTwice(t *testing.T) {
	rpc := successRPC(t, "out-hash")
	rpc.pollErr = context.DeadlineExceeded
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), store.Forward{TxHash: "in-r", MemoID: 1, Amount: "10.0000000", Asset: "native"})

	if rpc.sent != 1 || len(st.requeueCalls) != 1 || len(st.markFailedCalls) != 0 {
		t.Fatalf("sent=%d requeue=%d markFailed=%d, want one send and an uncharged requeue",
			rpc.sent, len(st.requeueCalls), len(st.markFailedCalls))
	}
}

// The network's clock decides expiry: a lagging RPC that has not yet ingested a
// ledger past the max time cannot rule out that the transfer lands.
func TestRetry_inFlightWaitsForLedgerPastMaxTime(t *testing.T) {
	until := time.Now().Add(-time.Hour)
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusNotFound, "")
	rpc.getResp.LatestLedgerCloseTime = until.Add(-time.Second).Unix()
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, until))

	if rpc.sent != 0 || len(st.clearCalls) != 0 || len(st.requeueCalls) != 1 {
		t.Fatalf("sent=%d clear=%d requeue=%d: RPC has not passed the max time yet",
			rpc.sent, len(st.clearCalls), len(st.requeueCalls))
	}
}

func TestRetry_inFlightExpiredPerLedgerIsRebuilt(t *testing.T) {
	until := time.Now().Add(-time.Minute).Truncate(time.Second)
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusNotFound, "")
	rpc.getResp.LatestLedgerCloseTime = until.Add(5 * time.Second).Unix()
	rpc.getResp.OldestLedgerCloseTime = until.Add(-24 * time.Hour).Unix()
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, until))

	if len(st.clearCalls) != 1 || rpc.sent != 1 || len(st.doneCalls) != 1 {
		t.Fatalf("clear=%d sent=%d done=%d, want one rebuild", len(st.clearCalls), rpc.sent, len(st.doneCalls))
	}
}

// After an outage longer than RPC's retention, "not found" no longer proves the
// transfer never landed. Rebuilding could pay twice, so it goes to a human.
func TestRetry_inFlightOutsideRPCHistoryIsNotRebuilt(t *testing.T) {
	until := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusNotFound, "")
	rpc.getResp.LatestLedgerCloseTime = time.Now().Unix()
	rpc.getResp.OldestLedgerCloseTime = time.Now().Add(-24 * time.Hour).Unix()
	f, st := forwarderWith(t, rpc)

	f.Retry(context.Background(), inFlight(0, until))

	if rpc.sent != 0 || len(st.clearCalls) != 0 {
		t.Fatalf("sent=%d clear=%d for a transfer RPC can no longer vouch for", rpc.sent, len(st.clearCalls))
	}
	if len(st.permFailCalls) != 1 || !strings.Contains(st.permFailCalls[0].errMsg, "submitted-hash") {
		t.Fatalf("want a permanent failure naming the transfer, got %+v", st.permFailCalls)
	}
	if len(st.failIntentCalls) != 0 {
		t.Fatalf("intent failed although the deposit may have been credited")
	}
}
