package forwarder

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	"github.com/stellar/go-stellar-sdk/protocols/horizon/base"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/store"
)

// A sweep is recorded as swept only once the recovery payment has landed, and
// otherwise stays queued or fails with the funds still in the pool (#32).

const (
	usdcIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	usdc       = "USDC:" + usdcIssuer
)

// sweepForwarder returns a forwarder whose deposit carries an unknown memo.
func sweepForwarder(t *testing.T, rpc *mockRPC) (*Forwarder, *mockStore, *mockHorizon) {
	t.Helper()
	f, st := forwarderWith(t, rpc)
	st.intent, st.intentErr = nil, pgx.ErrNoRows
	return f, st, f.horizon.(*mockHorizon)
}

func sweepRow(f *Forwarder, asset string) store.Forward {
	return store.Forward{
		TxHash: "in-sweep", MemoID: 9, Amount: "5.0000000", Asset: asset,
		PoolAddress: f.config.PoolAccounts[0].Address, Sweep: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

func withTrustline(hz *mockHorizon, recovery string, authorized bool) {
	hz.accounts = map[string]hProtocol.Account{recovery: {
		AccountID: recovery,
		Balances: []hProtocol.Balance{{
			Asset:        base.Asset{Type: "credit_alphanum4", Code: "USDC", Issuer: usdcIssuer},
			IsAuthorized: &authorized,
		}},
	}}
}

func TestSweep_MissingTrustlineWaitsWithoutSending(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, st, hz := sweepForwarder(t, rpc)
	hz.accounts = map[string]hProtocol.Account{f.config.RecoveryAddress: {AccountID: f.config.RecoveryAddress}}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-sweep", 9, "GABC", "5.0000000", usdc, time.Now())

	if rpc.sent != 0 {
		t.Fatalf("sent %d payments the recovery account cannot receive", rpc.sent)
	}
	if len(st.sweptCalls) != 0 || len(st.permFailCalls) != 0 || len(st.markFailedCalls) != 0 {
		t.Fatalf("swept=%v permFail=%v markFailed=%v, want only an uncharged requeue",
			st.sweptCalls, st.permFailCalls, st.markFailedCalls)
	}
	if len(st.requeueCalls) != 1 || !strings.Contains(st.requeueCalls[0].errMsg, "trustline") {
		t.Fatalf("requeue = %+v, want it to name the missing trustline", st.requeueCalls)
	}

	// Adding the trustline is all it takes: the next retry sweeps.
	withTrustline(hz, f.config.RecoveryAddress, true)
	f.Retry(context.Background(), sweepRow(f, usdc))

	if rpc.sent != 1 || len(st.sweptCalls) != 1 {
		t.Fatalf("sent=%d swept=%v after the trustline was added", rpc.sent, st.sweptCalls)
	}
}

func TestSweep_UnauthorizedTrustlineWaits(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, st, hz := sweepForwarder(t, rpc)
	withTrustline(hz, f.config.RecoveryAddress, false)

	f.Retry(context.Background(), sweepRow(f, usdc))

	if rpc.sent != 0 || len(st.requeueCalls) != 1 {
		t.Fatalf("sent=%d requeue=%d for an unauthorized trustline", rpc.sent, len(st.requeueCalls))
	}
}

func TestSweep_TransientFailureThenSuccess(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	rpc.sendResp = rpcprotocol.SendTransactionResponse{
		Status:         "ERROR",
		ErrorResultXDR: makeResultXDR(t, xdr.TransactionResultCodeTxInternalError),
	}
	f, st, _ := sweepForwarder(t, rpc)

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-sweep", 9, "GABC", "5.0000000", "native", time.Now())

	if len(st.sweptCalls) != 0 {
		t.Fatalf("recorded as swept although the payment was rejected: %v", st.sweptCalls)
	}
	if len(st.markFailedCalls) != 1 || st.markFailedCalls[0].status != store.StatusPendingRetry ||
		!strings.Contains(st.markFailedCalls[0].errMsg, "not swept") {
		t.Fatalf("markFailed = %+v, want a charged pending_retry saying not swept", st.markFailedCalls)
	}
	if len(st.sweepCalls) != 1 {
		t.Fatalf("sweep decision not persisted: %v", st.sweepCalls)
	}

	rpc.sendResp = rpcprotocol.SendTransactionResponse{Status: "PENDING", Hash: "sweep-hash"}
	fwd := sweepRow(f, "native")
	fwd.Retries = 1
	f.Retry(context.Background(), fwd)

	if len(st.sweptCalls) != 1 || st.sweptCalls[0] != [2]string{"in-sweep", "sweep-hash"} {
		t.Fatalf("swept = %v after the retry landed", st.sweptCalls)
	}
	if len(st.doneCalls) != 0 || len(st.completeIntentCalls) != 0 {
		t.Fatalf("a sweep was recorded as a credit: done=%v complete=%v", st.doneCalls, st.completeIntentCalls)
	}
}

// Crash after the sweep was sent, before the database heard back: the retry
// finds the payment on-chain and records it, rather than paying again.
func TestSweep_CrashAfterSubmissionIsResolvedNotResent(t *testing.T) {
	rpc := successRPC(t, "new-hash")
	rpc.getResp = lookup(rpcprotocol.TransactionStatusSuccess, "")
	f, st, _ := sweepForwarder(t, rpc)

	fwd := sweepRow(f, "native")
	hash, until := "sweep-in-flight", time.Now().Add(time.Minute)
	fwd.SubmittedTx, fwd.SubmittedUntil = &hash, &until
	f.Retry(context.Background(), fwd)

	if rpc.sent != 0 {
		t.Fatalf("sent %d more sweeps for one that already landed", rpc.sent)
	}
	if len(st.sweptCalls) != 1 || st.sweptCalls[0] != [2]string{"in-sweep", "sweep-in-flight"} {
		t.Fatalf("swept = %v", st.sweptCalls)
	}
	if len(st.doneCalls) != 0 || len(st.completeIntentCalls) != 0 {
		t.Fatalf("a sweep was recorded as a credit: done=%v complete=%v", st.doneCalls, st.completeIntentCalls)
	}
}

func TestSweep_PollTimeoutDoesNotSendTwice(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	rpc.pollErr = context.DeadlineExceeded
	f, st, _ := sweepForwarder(t, rpc)

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-sweep", 9, "GABC", "5.0000000", "native", time.Now())

	if rpc.sent != 1 || len(st.recordCalls) != 1 || len(st.requeueCalls) != 1 {
		t.Fatalf("sent=%d record=%d requeue=%d, want one recorded send left to resolve",
			rpc.sent, len(st.recordCalls), len(st.requeueCalls))
	}
	if len(st.sweptCalls) != 0 {
		t.Fatalf("recorded as swept before it was seen to land")
	}
}

func TestSweep_FailedOnChainIsNotSwept(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	rpc.pollResp = lookup(rpcprotocol.TransactionStatusFailed, makeResultXDR(t, xdr.TransactionResultCodeTxBadAuth))
	f, st, _ := sweepForwarder(t, rpc)

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-sweep", 9, "GABC", "5.0000000", "native", time.Now())

	if len(st.sweptCalls) != 0 {
		t.Fatalf("recorded as swept although it failed on-chain")
	}
	if len(st.permFailCalls) != 1 || !strings.Contains(st.permFailCalls[0].errMsg, "funds remain in pool") {
		t.Fatalf("permFail = %+v, want an accurate not-swept failure", st.permFailCalls)
	}
}

func TestSweep_NoRecoveryAddressIsNotSwept(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, st, _ := sweepForwarder(t, rpc)
	f.config.RecoveryAddress = ""

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-sweep", 9, "GABC", "5.0000000", "native", time.Now())

	if rpc.sent != 0 || len(st.sweptCalls) != 0 {
		t.Fatalf("sent=%d swept=%v with no recovery address", rpc.sent, st.sweptCalls)
	}
	if len(st.permFailCalls) != 1 || !strings.Contains(st.permFailCalls[0].errMsg, "RECOVERY_ADDRESS") {
		t.Fatalf("permFail = %+v", st.permFailCalls)
	}
}

func TestSweep_DecisionNotPersistedSendsNothing(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, st, _ := sweepForwarder(t, rpc)
	st.sweepErr = errors.New("db down")

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-sweep", 9, "GABC", "5.0000000", "native", time.Now())

	if rpc.sent != 0 {
		t.Fatalf("swept without persisting the decision")
	}
	if len(st.markFailedCalls) != 1 || st.markFailedCalls[0].status != store.StatusPendingRetry {
		t.Fatalf("markFailed = %+v", st.markFailedCalls)
	}
}

// A database error is not "unknown memo": sweeping on it would send a valid
// deposit to the recovery account.
func TestForward_IntentLookupErrorDoesNotSweep(t *testing.T) {
	rpc := successRPC(t, "out")
	f, st := forwarderWith(t, rpc)
	st.intent, st.intentErr = nil, errors.New("connection refused")

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-dberr", 9, "GABC", "5.0000000", "native", time.Now())

	if rpc.sent != 0 || len(st.sweepCalls) != 0 {
		t.Fatalf("sent=%d sweep=%v on a lookup error", rpc.sent, st.sweepCalls)
	}
	if len(st.markFailedCalls) != 1 || st.markFailedCalls[0].status != store.StatusPendingRetry {
		t.Fatalf("markFailed = %+v", st.markFailedCalls)
	}
}

func TestRetry_IntentLookupErrorDoesNotSweepOrFail(t *testing.T) {
	rpc := successRPC(t, "out")
	f, st := forwarderWith(t, rpc)
	st.intent, st.intentErr = nil, errors.New("connection refused")

	f.Retry(context.Background(), store.Forward{TxHash: "in-dberr", MemoID: 9, Amount: "1.0000000", Asset: "native",
		PoolAddress: f.config.PoolAccounts[0].Address})

	if rpc.sent != 0 || len(st.sweepCalls) != 0 || len(st.permFailCalls) != 0 {
		t.Fatalf("sent=%d sweep=%v permFail=%v on a lookup error", rpc.sent, st.sweepCalls, st.permFailCalls)
	}
}

// A row whose first attempt died before deciding still carries an unknown memo:
// the retry returns it rather than failing it.
func TestRetry_UnknownMemoIsSwept(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, st, _ := sweepForwarder(t, rpc)

	f.Retry(context.Background(), store.Forward{TxHash: "in-undecided", MemoID: 9, Amount: "1.0000000",
		Asset: "native", PoolAddress: f.config.PoolAccounts[0].Address})

	if len(st.sweepCalls) != 1 || len(st.sweptCalls) != 1 || len(st.permFailCalls) != 0 {
		t.Fatalf("sweep=%v swept=%v permFail=%v", st.sweepCalls, st.sweptCalls, st.permFailCalls)
	}
}

func TestSweep_RetryCeilingSaysNotSwept(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, st, _ := sweepForwarder(t, rpc)

	fwd := sweepRow(f, "native")
	fwd.Retries = maxRetries
	f.Retry(context.Background(), fwd)

	if rpc.sent != 0 || len(st.failIntentCalls) != 0 {
		t.Fatalf("sent=%d failIntent=%v", rpc.sent, st.failIntentCalls)
	}
	if len(st.permFailCalls) != 1 || !strings.Contains(st.permFailCalls[0].errMsg, "not swept") {
		t.Fatalf("permFail = %+v", st.permFailCalls)
	}
}

// The recovery payment carries the inbound hash as its memo, so the recovery
// account can be reconciled against the deposits it received.
func TestSweep_MemoIsInboundHash(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, _, _ := sweepForwarder(t, rpc)
	inbound := strings.Repeat("ab", 32)

	if _, err := f.sweep(context.Background(), inbound, f.config.PoolAccounts[0].Address, "1.0000000", "native"); err != nil {
		t.Fatal(err)
	}

	tx := parseTx(t, rpc.sentXDR[0])
	memo, ok := tx.Memo().(txnbuild.MemoHash)
	if !ok || hex.EncodeToString(memo[:]) != inbound {
		t.Fatalf("memo = %#v, want MemoHash(%s)", tx.Memo(), inbound)
	}
	pay, ok := tx.Operations()[0].(*txnbuild.Payment)
	if !ok || pay.Destination != f.config.RecoveryAddress {
		t.Fatalf("operation = %#v, want a payment to the recovery account", tx.Operations()[0])
	}
}

// Only accepted assets are credited; anything else goes back to recovery.
func TestForward_UnsupportedAssetIsSwept(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, st := forwarderWith(t, rpc)
	f.config.AcceptedAssets = []string{"native"}
	withTrustline(f.horizon.(*mockHorizon), f.config.RecoveryAddress, true)

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-usdc", 1, "GABC", "5.0000000", usdc, time.Now())

	if len(st.doneCalls) != 0 {
		t.Fatalf("credited an asset that is not accepted: %v", st.doneCalls)
	}
	if len(st.sweepCalls) != 1 || st.sweepCalls[0][1] != "unsupported asset "+usdc {
		t.Fatalf("sweep decision = %v", st.sweepCalls)
	}
	if len(st.sweptCalls) != 1 {
		t.Fatalf("swept = %v", st.sweptCalls)
	}
}

func TestForward_AcceptedAssetIsCredited(t *testing.T) {
	rpc := successRPC(t, "out")
	f, st := forwarderWith(t, rpc)
	f.config.AcceptedAssets = []string{"native", usdc}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-usdc", 1, "GABC", "5.0000000", usdc, time.Now())

	if len(st.doneCalls) != 1 || len(st.sweepCalls) != 0 {
		t.Fatalf("done=%v sweep=%v, want an accepted asset credited", st.doneCalls, st.sweepCalls)
	}
}

func TestCheckTrustlines(t *testing.T) {
	f, _ := forwarderWith(t, successRPC(t, "x"))
	f.config.AcceptedAssets = []string{"native", usdc}
	hz := f.horizon.(*mockHorizon)
	withTrustline(hz, f.config.RecoveryAddress, true)
	pool := f.config.PoolAccounts[0].Address
	hz.accounts[pool] = hProtocol.Account{AccountID: pool} // no USDC trustline

	problems := f.CheckTrustlines()

	if len(problems) != 1 || !strings.HasPrefix(problems[0], pool) || !strings.Contains(problems[0], "USDC") {
		t.Fatalf("problems = %v, want only the pool's missing USDC trustline", problems)
	}
}

// A payment from a batched transaction is keyed "hash:position"; its sweep
// still carries the transaction hash as memo.
func TestSweep_MemoFromBatchedDepositKey(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, _, _ := sweepForwarder(t, rpc)
	inbound := strings.Repeat("cd", 32)

	if _, err := f.sweep(context.Background(), inbound+":2", f.config.PoolAccounts[0].Address, "1.0000000", "native"); err != nil {
		t.Fatal(err)
	}
	memo, ok := parseTx(t, rpc.sentXDR[0]).Memo().(txnbuild.MemoHash)
	if !ok || hex.EncodeToString(memo[:]) != inbound {
		t.Fatalf("memo = %#v, want MemoHash(%s)", memo, inbound)
	}
}

// Expiry is judged by when the deposit landed, not when it is processed: a
// payment made inside the window is credited even if the relayer reaches it
// late (a replay after restart, a backlog), and even if the retry worker has
// already flipped the intent to 'expired' by wall clock.
func TestForward_ExpiryJudgedByLandingTime(t *testing.T) {
	expires := time.Now().Add(-time.Minute)
	cases := []struct {
		name     string
		status   string
		landedAt time.Time
		credited bool
	}{
		{"landed in window, processed late", store.IntentPending, expires.Add(-time.Second), true},
		{"landed in window, already flipped to expired", store.IntentExpired, expires.Add(-time.Second), true},
		{"landed after the window", store.IntentPending, expires.Add(time.Second), false},
		{"landing time unknown falls back to now", store.IntentPending, time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, st := forwarderWith(t, successRPC(t, "out"))
			st.intent.Status, st.intent.ExpiresAt = c.status, expires

			f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-ttl", 1, "GABC", "5.0000000", "native", c.landedAt)

			if credited := len(st.doneCalls) == 1; credited != c.credited {
				t.Fatalf("credited = %v, want %v (done=%v sweep=%v)", credited, c.credited, st.doneCalls, st.sweepCalls)
			}
			if !c.credited && (len(st.sweepCalls) != 1 || st.sweepCalls[0][1] != "intent expired") {
				t.Fatalf("sweep = %v, want the expiry sweep", st.sweepCalls)
			}
		})
	}
}

// ── Review of #54 ─────────────────────────────────────────────────────────────

// A send that errored may still have reached the network, so its sequence
// number may be spent. Re-reading Horizon then would hand the same number to
// the next payment.
func TestTransfer_UnconfirmedSendDoesNotResyncSequence(t *testing.T) {
	rpc := successRPC(t, "out")
	rpc.sendErr = errors.New("connection reset")
	f, _ := forwarderWith(t, rpc)
	pool := f.config.PoolAccounts[0].Address

	f.Forward(context.Background(), pool, "in-a", 1, "GABC", "1.0000000", "native", time.Now())
	rpc.sendErr = nil
	f.Forward(context.Background(), pool, "in-b", 1, "GABC", "1.0000000", "native", time.Now())

	if len(rpc.sentXDR) != 2 {
		t.Fatalf("sent %d transactions, want 2", len(rpc.sentXDR))
	}
	first, second := parseTx(t, rpc.sentXDR[0]).SequenceNumber(), parseTx(t, rpc.sentXDR[1]).SequenceNumber()
	if second != first+1 {
		t.Fatalf("second payment reused sequence %d after an unconfirmed send (first %d)", second, first)
	}
}

// If the first attempt died before its sweep decision was saved, the retry
// must still return a deposit that landed after its intent expired.
func TestRetry_RechecksExpiryByLandingTime(t *testing.T) {
	expires := time.Now().Add(-time.Hour)
	late, early := expires.Add(time.Minute), expires.Add(-time.Minute)
	cases := []struct {
		name     string
		landedAt *time.Time
		credited bool
	}{
		{"landed after expiry", &late, false},
		{"landed in time, retried after expiry", &early, true},
		{"legacy row without landing time", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, st := forwarderWith(t, successRPC(t, "out"))
			st.intent.ExpiresAt, st.intent.Status = expires, store.IntentExpired

			f.Retry(context.Background(), store.Forward{
				TxHash: "in-retry", MemoID: 1, Amount: "1.0000000", Asset: "native",
				PoolAddress: f.config.PoolAccounts[0].Address, LandedAt: c.landedAt,
			})

			if credited := len(st.doneCalls) == 1; credited != c.credited {
				t.Fatalf("credited = %v, want %v (sweep=%v)", credited, c.credited, st.sweepCalls)
			}
			if !c.credited && (len(st.sweepCalls) != 1 || st.sweepCalls[0][1] != "intent expired") {
				t.Fatalf("sweep = %v, want the expiry sweep", st.sweepCalls)
			}
		})
	}
}

// Failing to load the recovery account is a Horizon blip, not the sweep
// failing: it must not use up the retry budget.
func TestSweep_TrustlineLookupFailureIsNotCharged(t *testing.T) {
	rpc := successRPC(t, "sweep-hash")
	f, st, hz := sweepForwarder(t, rpc)
	pool := f.config.PoolAccounts[0].Address
	hz.accounts = map[string]hProtocol.Account{pool: {AccountID: pool, Sequence: 100}}
	hz.accountErr = errors.New("horizon: 503")

	f.Retry(context.Background(), sweepRow(f, usdc))

	if rpc.sent != 0 || len(st.markFailedCalls) != 0 || len(st.permFailCalls) != 0 {
		t.Fatalf("sent=%d markFailed=%v permFail=%v, want an uncharged wait",
			rpc.sent, st.markFailedCalls, st.permFailCalls)
	}
	if len(st.requeueCalls) != 1 {
		t.Fatalf("requeue = %v", st.requeueCalls)
	}
}

func TestForward_RecordsLandingTime(t *testing.T) {
	f, st := forwarderWith(t, successRPC(t, "out"))
	landed := time.Now().Add(-time.Minute).Truncate(time.Second)

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-landed", 1, "GABC", "1.0000000", "native", landed)

	if !st.insertLanded.Equal(landed) {
		t.Fatalf("recorded landing time %s, want %s", st.insertLanded, landed)
	}
}
