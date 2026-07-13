package forwarder

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
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
	insertErr    error
	intent       *store.Intent
	intentErr    error

	doneCalls           [][2]string
	completeIntentCalls []uint64
	markFailedCalls     []markFailedCall
	permFailCalls       []permanentlyFailCall
	failIntentCalls     []uint64
}

func (m *mockStore) InsertForward(_ context.Context, _ string, _ uint64, _, _, _ string) error {
	return m.insertErr
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
func (m *mockStore) PermanentlyFail(_ context.Context, txHash, errMsg string) error {
	m.permFailCalls = append(m.permFailCalls, permanentlyFailCall{txHash, errMsg})
	return nil
}
func (m *mockStore) FailIntent(_ context.Context, memoID uint64) error {
	m.failIntentCalls = append(m.failIntentCalls, memoID)
	return nil
}

type mockHorizon struct {
	account    hProtocol.Account
	accountErr error
}

func (m *mockHorizon) AccountDetail(_ horizonclient.AccountRequest) (hProtocol.Account, error) {
	return m.account, m.accountErr
}
func (m *mockHorizon) SubmitTransaction(_ *txnbuild.Transaction) (hProtocol.Transaction, error) {
	return hProtocol.Transaction{}, nil
}

type mockRPC struct {
	simResp  rpcprotocol.SimulateTransactionResponse
	simErr   error
	sendResp rpcprotocol.SendTransactionResponse
	sendErr  error
	pollResp rpcprotocol.GetTransactionResponse
	pollErr  error
}

func (m *mockRPC) SimulateTransaction(_ context.Context, _ rpcprotocol.SimulateTransactionRequest) (rpcprotocol.SimulateTransactionResponse, error) {
	return m.simResp, m.simErr
}
func (m *mockRPC) SendTransaction(_ context.Context, _ rpcprotocol.SendTransactionRequest) (rpcprotocol.SendTransactionResponse, error) {
	return m.sendResp, m.sendErr
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
	st := &mockStore{intentErr: errors.New("no rows")}
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

	f.Forward(context.Background(), "txhash", 1, "GABC", "10.0", "native")

	// InsertForward failed — nothing else should be called
	if len(st.markFailedCalls) != 0 || len(st.permFailCalls) != 0 {
		t.Error("unexpected store calls after InsertForward error")
	}
}

func TestForward_unknownMemoID(t *testing.T) {
	st := &mockStore{intentErr: errors.New("no rows")}
	f := &Forwarder{store: st, config: testConfig(t), horizon: &mockHorizon{}, rpc: &mockRPC{}}

	f.Forward(context.Background(), "txhash-unk", 999, "GABC", "5.0", "native")

	if len(st.markFailedCalls) != 1 {
		t.Fatalf("want 1 MarkForwardFailed, got %d", len(st.markFailedCalls))
	}
	c := st.markFailedCalls[0]
	if c.txHash != "txhash-unk" || c.status != store.StatusFailed {
		t.Errorf("unexpected MarkForwardFailed call: %+v", c)
	}
}

func TestForward_expiredIntent(t *testing.T) {
	st := &mockStore{intent: &store.Intent{
		MemoID:  55,
		Status:  store.IntentExpired,
		CAddress: testCAddress,
	}}
	f := &Forwarder{store: st, config: testConfig(t), horizon: &mockHorizon{}, rpc: &mockRPC{}}

	f.Forward(context.Background(), "txhash-exp", 55, "GABC", "5.0", "native")

	if len(st.markFailedCalls) != 1 {
		t.Fatalf("want 1 MarkForwardFailed, got %d", len(st.markFailedCalls))
	}
	c := st.markFailedCalls[0]
	if c.status != store.StatusFailed {
		t.Errorf("want StatusFailed, got %q", c.status)
	}
}

func TestForward_permanentError(t *testing.T) {
	kp := newTestKeypair(t)
	st := &mockStore{intent: testIntent(kp.Address())}
	hz := &mockHorizon{account: hProtocol.Account{AccountID: kp.Address(), Sequence: 100}}
	// Simulation returning an error → permanent failure in submit()
	rpc := &mockRPC{simResp: rpcprotocol.SimulateTransactionResponse{Error: "contract not deployed"}}
	f := &Forwarder{store: st, config: testConfigWithKeypair(t, kp), horizon: hz, rpc: rpc}

	f.Forward(context.Background(), "in-perm", 1, "GABC", "10.0000000", "native")

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

	f.Forward(context.Background(), "in-trans", 1, "GABC", "10.0000000", "native")

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

	f.Forward(context.Background(), "in-hash-abc", 1, "GABC", "10.0000000", "native")

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
		NetworkPassphrase: network.TestNetworkPassphrase,
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
