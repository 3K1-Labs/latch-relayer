package forwarder

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/store"
)

func init() {
	// Zero-out backoffs so transient-error tests don't sleep in CI.
	backoffs = []time.Duration{0, 0, 0}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// makeResultXDR encodes a minimal TransactionResult XDR with the given result
// code. Only works for codes with no associated union data (not TxSuccess/TxFailed).
func makeResultXDR(t *testing.T, code xdr.TransactionResultCode) string {
	t.Helper()
	res, err := xdr.NewTransactionResultResult(code, nil)
	if err != nil {
		t.Fatalf("build TransactionResultResult for code %v: %v", code, err)
	}
	txResult := xdr.TransactionResult{Result: res}
	b, err := txResult.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal TransactionResult: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// validSorobanDataXDR returns a zero-value SorobanTransactionData encoded as
// base64. Sufficient for mocked simulation responses in unit tests.
func validSorobanDataXDR(t *testing.T) string {
	t.Helper()
	var d xdr.SorobanTransactionData
	b, err := d.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal SorobanTransactionData: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// testCAddress is a known-valid Stellar contract address (C-strkey) used across
// tests that need to reach txnbuild.NewPaymentToContract without a real network.
const testCAddress = "CCOX4AG3XESDAZC7L27AMQZ6KKMUWEU2KCHFXJ2PXNAXMDUCL225MN2P"

// ── mocks ─────────────────────────────────────────────────────────────────────

type markFailedCall struct{ txHash, status, errMsg string }
type permanentlyFailCall struct{ txHash, errMsg string }

type mockStore struct {
	insertErr  error
	insertDup  bool   // InsertForward reports the row already existed
	insertPool string // pool address InsertForward recorded
	intent     *store.Intent
	intentErr  error

	doneCalls           [][2]string
	completeIntentCalls []uint64
	markFailedCalls     []markFailedCall
	requeueCalls        []markFailedCall
	permFailCalls       []permanentlyFailCall
	failIntentCalls     []uint64
	recordCalls         []recordCall
	clearCalls          []string
	recordErr           error
	sweepCalls          [][2]string // {txHash, reason}
	sweptCalls          [][2]string // {txHash, sweepTx}
	sweepErr            error
}

type recordCall struct {
	txHash, submittedTx string
	until               time.Time
}

func (m *mockStore) InsertForward(_ context.Context, _ string, _ uint64, poolAddress, _, _, _ string) (bool, error) {
	m.insertPool = poolAddress
	if m.insertErr != nil {
		return false, m.insertErr
	}
	return !m.insertDup, nil
}
func (m *mockStore) GetIntentByMemoID(_ context.Context, _ uint64) (*store.Intent, error) {
	return m.intent, m.intentErr
}
func (m *mockStore) MarkForwardDone(_ context.Context, txHash, forwardTx string) error {
	m.doneCalls = append(m.doneCalls, [2]string{txHash, forwardTx})
	return nil
}
func (m *mockStore) CompleteIntent(_ context.Context, memoID uint64) error {
	m.completeIntentCalls = append(m.completeIntentCalls, memoID)
	return nil
}
func (m *mockStore) MarkForwardFailed(_ context.Context, txHash, status, errMsg string) error {
	m.markFailedCalls = append(m.markFailedCalls, markFailedCall{txHash, status, errMsg})
	return nil
}
func (m *mockStore) RequeueForContention(_ context.Context, txHash, errMsg string) error {
	m.requeueCalls = append(m.requeueCalls, markFailedCall{txHash, "pending_retry", errMsg})
	return nil
}
func (m *mockStore) PermanentlyFail(_ context.Context, txHash, errMsg string) error {
	m.permFailCalls = append(m.permFailCalls, permanentlyFailCall{txHash, errMsg})
	return nil
}
func (m *mockStore) FailIntent(_ context.Context, memoID uint64) error {
	m.failIntentCalls = append(m.failIntentCalls, memoID)
	return nil
}
func (m *mockStore) MarkSweep(_ context.Context, txHash, reason string) error {
	if m.sweepErr != nil {
		return m.sweepErr
	}
	m.sweepCalls = append(m.sweepCalls, [2]string{txHash, reason})
	return nil
}
func (m *mockStore) MarkSwept(_ context.Context, txHash, sweepTx string) error {
	m.sweptCalls = append(m.sweptCalls, [2]string{txHash, sweepTx})
	return nil
}
func (m *mockStore) RecordSubmission(_ context.Context, txHash, submittedTx string, until time.Time) error {
	if m.recordErr != nil {
		return m.recordErr
	}
	m.recordCalls = append(m.recordCalls, recordCall{txHash, submittedTx, until})
	return nil
}
func (m *mockStore) ClearSubmission(_ context.Context, txHash string) error {
	m.clearCalls = append(m.clearCalls, txHash)
	return nil
}

type mockHorizon struct {
	account    hProtocol.Account
	accountErr error
	accounts   map[string]hProtocol.Account // per-address overrides of account
}

func (m *mockHorizon) AccountDetail(req horizonclient.AccountRequest) (hProtocol.Account, error) {
	if a, ok := m.accounts[req.AccountID]; ok {
		return a, nil
	}
	return m.account, m.accountErr
}

type mockRPC struct {
	simResp  rpcprotocol.SimulateTransactionResponse
	simErr   error
	sendResp rpcprotocol.SendTransactionResponse
	sendErr  error
	pollResp rpcprotocol.GetTransactionResponse
	pollErr  error
	getResp  rpcprotocol.GetTransactionResponse
	getErr   error

	simulated []string // transaction XDR of each simulation request
	sent      int      // SendTransaction calls
	sentXDR   []string // transaction XDR of each send
	looked    []string // hashes passed to GetTransaction
}

func (m *mockRPC) SimulateTransaction(_ context.Context, req rpcprotocol.SimulateTransactionRequest) (rpcprotocol.SimulateTransactionResponse, error) {
	m.simulated = append(m.simulated, req.Transaction)
	return m.simResp, m.simErr
}
func (m *mockRPC) SendTransaction(_ context.Context, req rpcprotocol.SendTransactionRequest) (rpcprotocol.SendTransactionResponse, error) {
	m.sent++
	m.sentXDR = append(m.sentXDR, req.Transaction)
	return m.sendResp, m.sendErr
}
func (m *mockRPC) GetTransaction(_ context.Context, req rpcprotocol.GetTransactionRequest) (rpcprotocol.GetTransactionResponse, error) {
	m.looked = append(m.looked, req.Hash)
	return m.getResp, m.getErr
}
func (m *mockRPC) PollTransaction(_ context.Context, _ string) (rpcprotocol.GetTransactionResponse, error) {
	return m.pollResp, m.pollErr
}

// ── classifyResultXDR tests ───────────────────────────────────────────────────

func TestClassifyResultXDR(t *testing.T) {
	cases := []struct {
		name       string
		xdrB64     string
		wantPerm   bool
		wantFeeErr bool
	}{
		{
			name:       "empty string — transient (no result XDR)",
			xdrB64:     "",
			wantPerm:   false,
			wantFeeErr: false,
		},
		{
			name:       "garbage base64 — transient (decode fail)",
			xdrB64:     "!!!not-base64!!!",
			wantPerm:   false,
			wantFeeErr: false,
		},
		{
			name:       "valid base64 but not XDR — transient (unmarshal fail)",
			xdrB64:     base64.StdEncoding.EncodeToString([]byte("not valid xdr bytes")),
			wantPerm:   false,
			wantFeeErr: false,
		},
		{
			name:       "TxInsufficientFee — feeError sentinel",
			xdrB64:     makeResultXDR(t, xdr.TransactionResultCodeTxInsufficientFee),
			wantFeeErr: true,
		},
		{
			name:     "TxTooEarly — transient",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxTooEarly),
			wantPerm: false,
		},
		{
			name:     "TxTooLate — transient",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxTooLate),
			wantPerm: false,
		},
		{
			name:     "TxBadSeq — transient",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxBadSeq),
			wantPerm: false,
		},
		{
			name:     "TxInternalError — transient",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxInternalError),
			wantPerm: false,
		},
		{
			name:     "TxBadAuth — permanent",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxBadAuth),
			wantPerm: true,
		},
		{
			name:     "TxInsufficientBalance — permanent",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxInsufficientBalance),
			wantPerm: true,
		},
		{
			name:     "TxNoAccount — permanent",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxNoAccount),
			wantPerm: true,
		},
		{
			name:     "TxNotSupported — permanent",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxNotSupported),
			wantPerm: true,
		},
		{
			name:     "TxBadAuthExtra — permanent",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxBadAuthExtra),
			wantPerm: true,
		},
		{
			name:     "TxMalformed — permanent",
			xdrB64:   makeResultXDR(t, xdr.TransactionResultCodeTxMalformed),
			wantPerm: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyResultXDR(tc.xdrB64)
			if err == nil {
				t.Fatal("want non-nil error, got nil")
			}

			if tc.wantFeeErr {
				var fe *feeError
				if !errors.As(err, &fe) {
					t.Errorf("want feeError, got %T: %v", err, err)
				}
				return
			}

			if tc.wantPerm != isPermanent(err) {
				t.Errorf("isPermanent = %v, want %v (err: %v)", isPermanent(err), tc.wantPerm, err)
			}
		})
	}
}

// ── error helper tests ────────────────────────────────────────────────────────

func TestIsPermanent(t *testing.T) {
	if isPermanent(permanent(errors.New("bad"))) != true {
		t.Error("permanent() should be permanent")
	}
	if isPermanent(transient(errors.New("retry"))) != false {
		t.Error("transient() should not be permanent")
	}
	if isPermanent(errors.New("plain")) != false {
		t.Error("plain error should not be permanent")
	}
	if isPermanent(nil) != false {
		t.Error("nil should not be permanent")
	}
}

// ── Retry tests ────────────────────────────────────────────────────────────────

func TestRetry_ceiling(t *testing.T) {
	st := &mockStore{}
	f := &Forwarder{store: st, config: testConfig(t), horizon: &mockHorizon{}, rpc: &mockRPC{}}

	f.Retry(context.Background(), store.Forward{
		TxHash:  "txhash1",
		MemoID:  42,
		Retries: maxRetries, // at the ceiling
	})

	if len(st.permFailCalls) != 1 {
		t.Fatalf("want 1 PermanentlyFail call, got %d", len(st.permFailCalls))
	}
	if st.permFailCalls[0].txHash != "txhash1" {
		t.Errorf("PermanentlyFail txHash = %q, want %q", st.permFailCalls[0].txHash, "txhash1")
	}
	if len(st.failIntentCalls) != 1 {
		t.Errorf("want 1 FailIntent call, got %d", len(st.failIntentCalls))
	}
	// submit must not have been called — no outbound calls beyond store
	if len(st.doneCalls) != 0 {
		t.Errorf("want 0 MarkForwardDone calls, got %d", len(st.doneCalls))
	}
}

func TestRetry_intentNotFound(t *testing.T) {
	st := &mockStore{intentErr: pgx.ErrNoRows}
	f := &Forwarder{store: st, config: testConfig(t), horizon: &mockHorizon{}, rpc: &mockRPC{}}

	f.Retry(context.Background(), store.Forward{TxHash: "txhash2", MemoID: 99, Retries: 0})

	if len(st.permFailCalls) != 1 || st.permFailCalls[0].txHash != "txhash2" {
		t.Errorf("want PermanentlyFail(txhash2), got %+v", st.permFailCalls)
	}
	if len(st.doneCalls) != 0 {
		t.Errorf("want no MarkForwardDone, got %d", len(st.doneCalls))
	}
}

func TestRetry_permanentSubmitError(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	rpc := &mockRPC{simResp: rpcprotocol.SimulateTransactionResponse{Error: "contract not deployed"}}
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: rpc}

	f.Retry(context.Background(), store.Forward{TxHash: "txhash3", MemoID: 1, Amount: "10.0000000", Asset: "native", Retries: 0})

	if len(st.permFailCalls) != 1 {
		t.Fatalf("want 1 PermanentlyFail, got %d", len(st.permFailCalls))
	}
	if len(st.failIntentCalls) != 1 {
		t.Errorf("want 1 FailIntent, got %d", len(st.failIntentCalls))
	}
	if len(st.markFailedCalls) != 0 {
		t.Errorf("want no MarkForwardFailed on permanent error, got %d", len(st.markFailedCalls))
	}
}

func TestRetry_transientSubmitError(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{accountErr: errors.New("horizon timeout")}
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: &mockRPC{}}

	f.Retry(context.Background(), store.Forward{TxHash: "txhash4", MemoID: 1, Amount: "10.0000000", Asset: "native", Retries: 2})

	if len(st.markFailedCalls) != 1 {
		t.Fatalf("want 1 MarkForwardFailed, got %d", len(st.markFailedCalls))
	}
	if st.markFailedCalls[0].status != store.StatusPendingRetry {
		t.Errorf("status = %q, want %q", st.markFailedCalls[0].status, store.StatusPendingRetry)
	}
	if len(st.permFailCalls) != 0 {
		t.Errorf("want no PermanentlyFail on transient error, got %d", len(st.permFailCalls))
	}
}

func TestRetry_success(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	rpc := successRPC(t, "retry-out-hash")
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: rpc}

	f.Retry(context.Background(), store.Forward{
		TxHash:  "in-hash",
		MemoID:  1,
		Amount:  "10.0000000",
		Asset:   "native",
		Retries: 0,
	})

	if len(st.doneCalls) != 1 {
		t.Fatalf("want 1 MarkForwardDone, got %d", len(st.doneCalls))
	}
	if st.doneCalls[0][0] != "in-hash" || st.doneCalls[0][1] != "retry-out-hash" {
		t.Errorf("MarkForwardDone args = %v", st.doneCalls[0])
	}
	if len(st.completeIntentCalls) != 1 || st.completeIntentCalls[0] != 1 {
		t.Errorf("CompleteIntent calls = %v", st.completeIntentCalls)
	}
}

// ── Forward tests ─────────────────────────────────────────────────────────────

func TestForward_insertError(t *testing.T) {
	st := &mockStore{insertErr: errors.New("db down")}
	f := &Forwarder{store: st, config: testConfig(t), horizon: &mockHorizon{}, rpc: &mockRPC{}}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "txhash", 1, "GABC", "10.0", "native", time.Now())

	// InsertForward failed — nothing else should be called
	if len(st.markFailedCalls) != 0 || len(st.permFailCalls) != 0 {
		t.Error("unexpected store calls after InsertForward error")
	}
}

func TestForward_unknownMemoID(t *testing.T) {
	f, st := forwarderWith(t, successRPC(t, "sweep-hash"))
	st.intent, st.intentErr = nil, pgx.ErrNoRows

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "txhash-unk", 999, "GABC", "5.0", "native", time.Now())

	if len(st.sweepCalls) != 1 || st.sweepCalls[0] != [2]string{"txhash-unk", "unknown memo_id"} {
		t.Fatalf("sweep decision = %v", st.sweepCalls)
	}
	if len(st.sweptCalls) != 1 || st.sweptCalls[0] != [2]string{"txhash-unk", "sweep-hash"} {
		t.Fatalf("swept = %v, want the landed sweep recorded", st.sweptCalls)
	}
	if len(st.markFailedCalls) != 0 || len(st.doneCalls) != 0 {
		t.Errorf("markFailed=%v done=%v", st.markFailedCalls, st.doneCalls)
	}
}

func TestForward_expiredIntent(t *testing.T) {
	f, st := forwarderWith(t, successRPC(t, "sweep-hash"))
	st.intent = &store.Intent{MemoID: 55, Status: store.IntentExpired, CAddress: testCAddress}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "txhash-exp", 55, "GABC", "5.0", "native", time.Now())

	if len(st.sweepCalls) != 1 || st.sweepCalls[0][1] != "intent expired" {
		t.Fatalf("sweep decision = %v, want intent expired", st.sweepCalls)
	}
	if len(st.sweptCalls) != 1 || len(st.doneCalls) != 0 {
		t.Fatalf("swept=%v done=%v", st.sweptCalls, st.doneCalls)
	}
}

// A replayed SSE event (crash before the cursor was saved, or a second instance
// on the same pool) must not pay the C-address twice for one deposit.
func TestForward_duplicateTxHash(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{insertDup: true, intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	f := &Forwarder{
		store:   st,
		config:  testConfigWithKeypair(t, kp),
		horizon: hz,
		rpc:     successRPC(t, "out-dup"),
	}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-dup", 1, "GABC", "10.0000000", "native", time.Now())

	if len(st.doneCalls) != 0 || len(st.completeIntentCalls) != 0 {
		t.Errorf("duplicate tx_hash was forwarded again: done=%v complete=%v",
			st.doneCalls, st.completeIntentCalls)
	}
	// Skipping is not a failure — the first dispatch owns this row's outcome.
	if len(st.markFailedCalls) != 0 || len(st.permFailCalls) != 0 {
		t.Errorf("duplicate tx_hash should be skipped silently: failed=%v perm=%v",
			st.markFailedCalls, st.permFailCalls)
	}
}

// The intent's TTL has elapsed but the retry worker has not ticked yet, so the
// status is still 'pending'. Crediting must not depend on that worker's timing.
func TestForward_intentPastExpiryButStillPending(t *testing.T) {
	st := &mockStore{intent: &store.Intent{
		MemoID:    77,
		CAddress:  testCAddress,
		Status:    store.IntentPending,
		ExpiresAt: time.Now().Add(-1 * time.Minute),
	}}
	f := &Forwarder{store: st, config: testConfig(t), horizon: &mockHorizon{}, rpc: &mockRPC{}}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-late", 77, "GABC", "5.0", "native", time.Now())

	if len(st.doneCalls) != 0 {
		t.Errorf("deposit past expires_at was credited: %v", st.doneCalls)
	}
	// Assert the reason, not just an outcome: falling through to submit() also
	// fails this forward, so an outcome-only check passes even without the guard.
	if len(st.sweepCalls) != 1 || st.sweepCalls[0][1] != "intent expired" {
		t.Errorf("sweep decision = %v, want the expiry sweep reason", st.sweepCalls)
	}
}

func TestMismatchesExpected(t *testing.T) {
	cases := []struct {
		name               string
		expected, received string
		want               bool
	}{
		{"exact match", "10.0000000", "10.0000000", false},
		{"within tolerance — provider fee", "10.0000000", "9.6000000", false},
		{"at the tolerance edge", "10.0000000", "9.5000000", false},
		{"short by half", "10.0000000", "5.0000000", true},
		{"double", "10.0000000", "20.0000000", true},
		// expected_amt is free-form; an unusable value is a caller bug, and must
		// not be reported as a suspicious deposit.
		{"unparseable expected", "fifty dollars", "10.0000000", false},
		{"empty expected", "", "10.0000000", false},
		{"zero expected", "0", "10.0000000", false},
		{"unparseable received", "10.0000000", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mismatchesExpected(tc.expected, tc.received); got != tc.want {
				t.Errorf("mismatchesExpected(%q, %q) = %v, want %v",
					tc.expected, tc.received, got, tc.want)
			}
		})
	}
}

// A deposit that misses expected_amt is still the user's money and still gets
// credited — the mismatch is logged, never enforced.
func TestForward_amountMismatchStillCredits(t *testing.T) {
	kp := newTestKeypair(t)
	intent := testIntent(kp.Address())
	expected := "100.0000000"
	intent.ExpectedAmt = &expected

	st := &mockStore{intent: intent}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	f := &Forwarder{
		store:   st,
		config:  testConfigWithKeypair(t, kp),
		horizon: hz,
		rpc:     successRPC(t, "out-mismatch"),
	}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-mismatch", 1, "GABC", "10.0000000", "native", time.Now())

	if len(st.doneCalls) != 1 {
		t.Fatalf("want the deposit credited despite the mismatch, got %d done calls", len(st.doneCalls))
	}
	if len(st.markFailedCalls) != 0 {
		t.Errorf("amount mismatch must not fail the forward: %v", st.markFailedCalls)
	}
}

func TestForward_permanentError(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	// Simulation returning an error → permanent failure in submit()
	rpc := &mockRPC{simResp: rpcprotocol.SimulateTransactionResponse{Error: "contract not deployed"}}
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: rpc}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-perm", 1, "GABC", "10.0000000", "native", time.Now())

	// Permanent error on first attempt → StatusFailed, no pending_retry
	if len(st.markFailedCalls) != 1 {
		t.Fatalf("want 1 MarkForwardFailed, got %d", len(st.markFailedCalls))
	}
	if st.markFailedCalls[0].status != store.StatusFailed {
		t.Errorf("status = %q, want StatusFailed", st.markFailedCalls[0].status)
	}
}

func TestForward_transientAllAttempts(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	// AccountDetail error is transient — all 3 attempts will fail this way
	hz := &mockHorizon{accountErr: errors.New("horizon timeout")}
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: &mockRPC{}}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-trans", 1, "GABC", "10.0000000", "native", time.Now())

	// All 3 attempts transient → StatusPendingRetry
	if len(st.markFailedCalls) != 1 {
		t.Fatalf("want 1 MarkForwardFailed, got %d", len(st.markFailedCalls))
	}
	if st.markFailedCalls[0].status != store.StatusPendingRetry {
		t.Errorf("status = %q, want StatusPendingRetry", st.markFailedCalls[0].status)
	}
}

func TestForward_success(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	rpc := successRPC(t, "out-hash-xyz")
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: rpc}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-hash-abc", 1, "GABC", "10.0000000", "native", time.Now())

	if len(st.doneCalls) != 1 {
		t.Fatalf("want 1 MarkForwardDone, got %d", len(st.doneCalls))
	}
	if st.doneCalls[0][0] != "in-hash-abc" || st.doneCalls[0][1] != "out-hash-xyz" {
		t.Errorf("MarkForwardDone args = %v", st.doneCalls[0])
	}
	if len(st.completeIntentCalls) != 1 {
		t.Errorf("want 1 CompleteIntent, got %d", len(st.completeIntentCalls))
	}
	if len(st.markFailedCalls) != 0 {
		t.Errorf("want no MarkForwardFailed on success, got %d", len(st.markFailedCalls))
	}
}

// ── issued assets ─────────────────────────────────────────────────────────────

// An on-ramp delivering anything but XLM lands an issued asset in the pool.
// Refusing it here strands real funds: the forward fails permanently and the
// sweep cannot move them either.
func TestParseAsset(t *testing.T) {
	usdcIssuer := "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

	cases := []struct {
		name    string
		asset   string
		want    txnbuild.Asset
		wantErr bool
	}{
		{"native", "native", txnbuild.NativeAsset{}, false},
		{"usdc", "USDC:" + usdcIssuer, txnbuild.CreditAsset{Code: "USDC", Issuer: usdcIssuer}, false},
		{"four-char code", "yXLM:" + usdcIssuer, txnbuild.CreditAsset{Code: "yXLM", Issuer: usdcIssuer}, false},
		{"no separator", "USDC", nil, true},
		{"empty code", ":" + usdcIssuer, nil, true},
		{"empty issuer", "USDC:", nil, true},
		{"empty string", "", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAsset(tc.asset)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseAsset(%q) = %v, want error", tc.asset, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseAsset(%q): %v", tc.asset, err)
			}
			if got != tc.want {
				t.Errorf("parseAsset(%q) = %v, want %v", tc.asset, got, tc.want)
			}
		})
	}
}

// A USDC deposit against a live intent must forward, not fail permanently.
func TestForward_issuedAssetSucceeds(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	rpc := successRPC(t, "out-hash-usdc")
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: rpc}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-hash-usdc", 1, "GABC", "25.0000000",
		"USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN", time.Now())

	if len(st.doneCalls) != 1 {
		t.Fatalf("want 1 MarkForwardDone for an issued asset, got %d (markFailed: %v)",
			len(st.doneCalls), st.markFailedCalls)
	}
	if len(st.completeIntentCalls) != 1 {
		t.Errorf("want 1 CompleteIntent, got %d", len(st.completeIntentCalls))
	}
}

// An asset identifier the watcher could not have produced is still permanent —
// retrying cannot make it parse.
func TestForward_unparseableAssetIsPermanent(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	rpc := successRPC(t, "unused")
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: rpc}

	f.Forward(context.Background(), f.config.PoolAccounts[0].Address, "in-hash-bad", 1, "GABC", "1.0000000", "NOTANASSET", time.Now())

	if len(st.doneCalls) != 0 {
		t.Fatalf("want no MarkForwardDone for an unparseable asset, got %d", len(st.doneCalls))
	}
	if len(st.markFailedCalls) != 1 {
		t.Fatalf("want 1 MarkForwardFailed, got %d", len(st.markFailedCalls))
	}
	if st.markFailedCalls[0].status != store.StatusFailed {
		t.Errorf("status = %q, want %q (permanent, not queued for retry)",
			st.markFailedCalls[0].status, store.StatusFailed)
	}
}

// ── test fixtures ─────────────────────────────────────────────────────────────

func newTestKeypair(t *testing.T) *keypair.Full {
	t.Helper()
	kp, err := keypair.Random()
	if err != nil {
		t.Fatalf("random keypair: %v", err)
	}
	return kp
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	kp := newTestKeypair(t)
	return testConfigWithKeypair(t, kp)
}

func testConfigWithKeypair(t *testing.T, kp *keypair.Full) *config.Config {
	t.Helper()
	return &config.Config{
		Common: config.Common{NetworkPassphrase: network.TestNetworkPassphrase},
		PoolAccounts: []config.PoolAccount{
			{Address: kp.Address(), Keypair: kp},
		},
		RecoveryAddress: "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF",
	}
}

func testIntent(poolAddress string) *store.Intent {
	return &store.Intent{
		MemoID:      1,
		CAddress:    testCAddress,
		PoolAddress: poolAddress,
		Status:      store.IntentPending,
		ExpiresAt:   time.Now().Add(24 * time.Hour),
	}
}

// successRPC returns a mockRPC pre-configured with valid simulation XDR,
// a PENDING send response, and a SUCCESS poll response.
func successRPC(t *testing.T, outHash string) *mockRPC {
	t.Helper()
	return &mockRPC{
		simResp: rpcprotocol.SimulateTransactionResponse{
			TransactionDataXDR: validSorobanDataXDR(t),
			MinResourceFee:     100,
		},
		sendResp: rpcprotocol.SendTransactionResponse{
			Status: "PENDING",
			Hash:   outHash,
		},
		pollResp: rpcprotocol.GetTransactionResponse{
			TransactionDetails: rpcprotocol.TransactionDetails{
				Status: rpcprotocol.TransactionStatusSuccess,
			},
		},
	}
}

// ── receiving pool: money leaves the pool it arrived at ───────────────────────

// twoPools builds a config with two pool accounts, as round-robin intent
// assignment produces in production.
func twoPools(t *testing.T) (*config.Config, *keypair.Full, *keypair.Full) {
	t.Helper()
	p1, p2 := newTestKeypair(t), newTestKeypair(t)
	cfg := testConfigWithKeypair(t, p1)
	cfg.PoolAccounts = append(cfg.PoolAccounts, config.PoolAccount{Address: p2.Address(), Keypair: p2})
	return cfg, p1, p2
}

func txSource(t *testing.T, txB64 string) string {
	t.Helper()
	return parseTx(t, txB64).SourceAccount().AccountID
}

func parseTx(t *testing.T, txB64 string) *txnbuild.Transaction {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(txB64)
	if err != nil {
		t.Fatal(err)
	}
	tx, ok := parsed.Transaction()
	if !ok {
		t.Fatal("not a simple transaction")
	}
	return tx
}

// Before this fix every sweep paid out of PoolAccounts[0]. With intents spread
// across pools, an unroutable deposit landing in pool 2 was refunded out of
// pool 1's balance — other customers' in-transit money — while pool 2 kept it.
func TestSweep_PaysOutOfReceivingPool(t *testing.T) {
	cases := map[string]*mockStore{
		"unknown memo":   {intentErr: pgx.ErrNoRows},
		"expired intent": {intent: &store.Intent{MemoID: 55, Status: store.IntentExpired, CAddress: testCAddress}},
	}
	for name, st := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, p1, p2 := twoPools(t)
			if st.intent != nil {
				st.intent.PoolAddress = p1.Address()
			}
			hz := &mockHorizon{account: hProtocol.Account{AccountID: p2.Address(), Sequence: 100}}
			rpc := successRPC(t, "sweep-hash")
			f := &Forwarder{store: st, config: cfg, horizon: hz, rpc: rpc}

			f.Forward(context.Background(), p2.Address(), "in-"+name, 55, "GABC", "5.0000000", "native", time.Now())

			if st.insertPool != p2.Address() {
				t.Errorf("forward recorded pool %s, want receiving pool %s", st.insertPool, p2.Address())
			}
			if len(rpc.sentXDR) != 1 {
				t.Fatalf("want 1 sweep, got %d", len(rpc.sentXDR))
			}
			sweep := parseTx(t, rpc.sentXDR[0])
			if src := sweep.SourceAccount().AccountID; src != p2.Address() {
				t.Fatalf("sweep paid out of %s, want receiving pool %s", src, p2.Address())
			}
			if sigs := sweep.Signatures(); len(sigs) != 1 || sigs[0].Hint != xdr.SignatureHint(p2.Hint()) {
				t.Fatal("sweep not signed by the receiving pool's key")
			}
		})
	}
}

// A depositor who pays a different pool than their intent named still gets
// credited — out of the pool that actually holds the money.
func TestForward_WrongPoolForwardsFromReceivingPool(t *testing.T) {
	cfg, p1, p2 := twoPools(t)
	st := &mockStore{intent: testIntent(p1.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: p2.Address(), Sequence: 100}}
	rpc := successRPC(t, "out-wrong-pool")
	f := &Forwarder{store: st, config: cfg, horizon: hz, rpc: rpc}

	f.Forward(context.Background(), p2.Address(), "in-wrong-pool", 1, "GABC", "10.0000000", "native", time.Now())

	if len(st.doneCalls) != 1 {
		t.Fatalf("forward not completed: done=%v failed=%v", st.doneCalls, st.markFailedCalls)
	}
	if len(rpc.simulated) == 0 || txSource(t, rpc.simulated[0]) != p2.Address() {
		t.Fatalf("forward built from %v, want receiving pool %s", rpc.simulated, p2.Address())
	}
}

func TestRetry_UsesStoredReceivingPool(t *testing.T) {
	cases := map[string]struct {
		stored   func(p1, p2 string) string
		wantPool func(p1, p2 string) string
	}{
		"receiving pool recorded": {func(_, p2 string) string { return p2 }, func(_, p2 string) string { return p2 }},
		"legacy row (no pool)":    {func(_, _ string) string { return "" }, func(p1, _ string) string { return p1 }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, p1, p2 := twoPools(t)
			st := &mockStore{intent: testIntent(p1.Address())}
			hz := &mockHorizon{account: hProtocol.Account{AccountID: p1.Address(), Sequence: 100}}
			rpc := successRPC(t, "out-retry")
			f := &Forwarder{store: st, config: cfg, horizon: hz, rpc: rpc}

			f.Retry(context.Background(), store.Forward{
				TxHash: "in-retry", MemoID: 1, Amount: "10.0000000", Asset: "native",
				PoolAddress: tc.stored(p1.Address(), p2.Address()),
			})

			want := tc.wantPool(p1.Address(), p2.Address())
			if len(rpc.simulated) == 0 || txSource(t, rpc.simulated[0]) != want {
				t.Fatalf("retry built from %v, want %s", rpc.simulated, want)
			}
		})
	}
}

// The sweep draws from the pool's sequencer, so it must also hold the pool's
// send lock: otherwise it can take N+1 and reach the network while a forward
// holding N is still between draw and send.
func TestSweep_WaitsForPoolSendLock(t *testing.T) {
	cfg, _, p2 := twoPools(t)
	hz := &mockHorizon{account: hProtocol.Account{AccountID: p2.Address(), Sequence: 100}}
	rpc := successRPC(t, "sweep-hash")
	f := &Forwarder{store: &mockStore{}, config: cfg, horizon: hz, rpc: rpc}

	unlock := f.seq().lockSend(p2.Address()) // a forward mid-send on this pool
	done := make(chan struct{})
	go func() {
		_, _ = f.sweep(context.Background(), "in-locked", p2.Address(), "1.0000000", "native")
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("sweep submitted while another send held the pool's lock")
	default:
	}
	unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweep did not proceed after the lock was released")
	}
	if rpc.sent != 1 {
		t.Fatalf("want 1 sweep after unlock, got %d", rpc.sent)
	}
}
