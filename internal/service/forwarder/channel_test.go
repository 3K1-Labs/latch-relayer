package forwarder

import (
	"context"
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/gasless/channels"
	"github.com/latch/relayer/internal/gasless/keys"
)

// With channels (#48) a transfer's sequence comes from a leased channel, not
// the pool, so one pool's transfers no longer queue behind each other.

type releaseCall struct {
	seq    *int64
	resync bool
}

type mockLeaser struct {
	mu       sync.Mutex
	lease    channels.Lease
	err      error
	acquired int
	released []releaseCall
}

func (m *mockLeaser) AcquireWait(context.Context, time.Duration, time.Duration) (*channels.Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	m.acquired++
	l := m.lease
	return &l, nil
}

func (m *mockLeaser) Release(_ context.Context, _ *channels.Lease, seq *int64, resync bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s *int64
	if seq != nil {
		v := *seq
		s = &v
	}
	m.released = append(m.released, releaseCall{s, resync})
	return nil
}

func (m *mockLeaser) only(t *testing.T) releaseCall {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.released) != 1 {
		t.Fatalf("channel released %d times, want exactly once", len(m.released))
	}
	return m.released[0]
}

// channelForwarder is forwarderWith plus one leased channel whose last
// consumed sequence is 500.
func channelForwarder(t *testing.T, rpc *mockRPC) (*Forwarder, *mockStore, *mockLeaser, keys.Channel) {
	t.Helper()
	f, st := forwarderWith(t, rpc)
	ch := keys.Channel{Index: 0, Keypair: newTestKeypair(t)}
	l := &mockLeaser{lease: channels.Lease{ChannelID: 1, Address: ch.Address(), Token: "tok", Seq: 500}}
	f.UseChannels(l, []keys.Channel{ch})
	return f, st, l, ch
}

func feeBump(t *testing.T, txB64 string) *txnbuild.FeeBumpTransaction {
	t.Helper()
	parsed, err := txnbuild.TransactionFromXDR(txB64)
	if err != nil {
		t.Fatal(err)
	}
	fb, ok := parsed.FeeBump()
	if !ok {
		t.Fatal("sent a plain transaction, want a fee-bump")
	}
	return fb
}

// The channel supplies the sequence, the pool authorizes the transfer and pays
// the fee.
func assertChannelEnvelope(t *testing.T, txB64 string, pool, channel string, seq int64) {
	t.Helper()
	fb := feeBump(t, txB64)
	if fb.FeeAccount() != pool {
		t.Errorf("fee account = %s, want pool %s", fb.FeeAccount(), pool)
	}
	if len(fb.Signatures()) != 1 {
		t.Errorf("fee-bump has %d signatures, want 1 (pool)", len(fb.Signatures()))
	}
	inner := fb.InnerTransaction()
	if src := inner.SourceAccount(); src.AccountID != channel || src.Sequence != seq {
		t.Errorf("inner source = %s/%d, want channel %s/%d", src.AccountID, src.Sequence, channel, seq)
	}
	if len(inner.Signatures()) != 2 {
		t.Errorf("inner has %d signatures, want 2 (channel, pool)", len(inner.Signatures()))
	}
	if ops := inner.Operations(); len(ops) != 1 || ops[0].GetSourceAccount() != pool {
		t.Errorf("operation source = %v, want pool %s", ops, pool)
	}
}

func TestChannel_forwardIsFeeBumpedFromChannel(t *testing.T) {
	rpc := successRPC(t, "out-channel")
	f, st, l, ch := channelForwarder(t, rpc)
	pool := f.config.PoolAccounts[0].Address

	forward(f, "in-channel")

	if len(st.doneCalls) != 1 {
		t.Fatalf("forward not completed: done=%v failed=%v requeue=%v", st.doneCalls, st.markFailedCalls, st.requeueCalls)
	}
	if rpc.sent != 1 {
		t.Fatalf("sent %d, want 1", rpc.sent)
	}
	assertChannelEnvelope(t, rpc.sentXDR[0], pool, ch.Address(), 501)
	if len(rpc.simulated) == 0 || txSource(t, rpc.simulated[0]) != ch.Address() {
		t.Error("simulation did not use the channel as source")
	}

	// The hash recorded before sending is the one the network knows: the outer.
	hash, err := feeBump(t, rpc.sentXDR[0]).HashHex(f.config.NetworkPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.recordCalls) != 1 || st.recordCalls[0].submittedTx != hash {
		t.Fatalf("recorded %v, want fee-bump hash %s", st.recordCalls, hash)
	}
	if r := l.only(t); r.resync || r.seq == nil || *r.seq != 501 {
		t.Fatalf("release = %+v, want sequence 501 consumed", r)
	}
}

// The pool's send lock serialises pool-sourced transactions. A channel
// transaction doesn't use the pool's sequence, so it must not wait for it.
func TestChannel_doesNotWaitForPoolSendLock(t *testing.T) {
	rpc := successRPC(t, "out-parallel")
	f, st, _, _ := channelForwarder(t, rpc)

	unlock := f.seq().lockSend(f.config.PoolAccounts[0].Address)
	defer unlock()
	done := make(chan struct{})
	go func() { forward(f, "in-parallel"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("channel forward blocked on the pool's send lock")
	}
	if len(st.doneCalls) != 1 {
		t.Fatalf("forward not completed: %v", st.markFailedCalls)
	}
}

func TestChannel_sweepGoesThroughChannel(t *testing.T) {
	rpc := successRPC(t, "sweep-channel")
	f, _, l, ch := channelForwarder(t, rpc)
	pool := f.config.PoolAccounts[0].Address

	if _, err := f.sweep(context.Background(), "in-sweep", pool, "1.0000000", "native"); err != nil {
		t.Fatal(err)
	}
	assertChannelEnvelope(t, rpc.sentXDR[0], pool, ch.Address(), 501)
	if r := l.only(t); r.resync || r.seq == nil || *r.seq != 501 {
		t.Fatalf("release = %+v", r)
	}
}

// Maybe queued: the sequence may be spent, so the next holder reloads it.
func TestChannel_unconfirmedSendResyncs(t *testing.T) {
	rpc := successRPC(t, "out")
	rpc.sendErr = errors.New("connection reset")
	f, st, l, _ := channelForwarder(t, rpc)

	forward(f, "in-unconfirmed")

	if rpc.sent != 1 || len(st.clearCalls) != 0 {
		t.Fatalf("sent=%d clear=%d; want one send kept on record", rpc.sent, len(st.clearCalls))
	}
	if r := l.only(t); !r.resync || r.seq != nil {
		t.Fatalf("release = %+v, want resync", r)
	}
}

// Rejected before the queue: the sequence is unused and the channel is where
// it was.
func TestChannel_rejectionKeepsSequence(t *testing.T) {
	rpc := successRPC(t, "out")
	rpc.sendResp = rpcprotocol.SendTransactionResponse{Status: "ERROR", ErrorResultXDR: makeResultXDR(t, xdr.TransactionResultCodeTxBadAuth)}
	f, st, l, _ := channelForwarder(t, rpc)

	forward(f, "in-rejected")

	if len(st.markFailedCalls) == 0 && len(st.permFailCalls) == 0 {
		t.Fatal("permanent rejection not failed")
	}
	if r := l.only(t); r.resync || r.seq == nil || *r.seq != 500 {
		t.Fatalf("release = %+v, want sequence 500 kept", r)
	}
}

func TestChannel_allBusyRequeuesWithoutSending(t *testing.T) {
	rpc := successRPC(t, "out")
	f, st, l, _ := channelForwarder(t, rpc)
	l.err = channels.ErrPoolCapacity

	forward(f, "in-busy")

	if rpc.sent != 0 || len(rpc.simulated) != 0 {
		t.Fatalf("sent=%d simulated=%d with no channel", rpc.sent, len(rpc.simulated))
	}
	if len(st.requeueCalls) != 1 || len(st.markFailedCalls) != 0 {
		t.Fatalf("want an uncharged requeue; requeue=%d markFailed=%d", len(st.requeueCalls), len(st.markFailedCalls))
	}
}

func TestChannel_needsResyncReadsHorizon(t *testing.T) {
	rpc := successRPC(t, "out")
	f, _, l, ch := channelForwarder(t, rpc)
	l.lease.NeedsResync = true
	l.lease.Seq = 0
	f.horizon.(*mockHorizon).accounts = map[string]hProtocol.Account{
		ch.Address(): {AccountID: ch.Address(), Sequence: 7000},
	}

	forward(f, "in-resync")

	assertChannelEnvelope(t, rpc.sentXDR[0], f.config.PoolAccounts[0].Address, ch.Address(), 7001)
	if r := l.only(t); r.resync || r.seq == nil || *r.seq != 7001 {
		t.Fatalf("release = %+v", r)
	}
}

// A channel transaction fails inside the fee-bump: classify the inner code.
func TestClassifyResultXDR_feeBumpInner(t *testing.T) {
	wrapped := func(inner xdr.TransactionResultCode) string {
		innerRes, err := xdr.NewInnerTransactionResultResult(inner, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := xdr.NewTransactionResultResult(xdr.TransactionResultCodeTxFeeBumpInnerFailed, xdr.InnerTransactionResultPair{
			Result: xdr.InnerTransactionResult{Result: innerRes},
		})
		if err != nil {
			t.Fatal(err)
		}
		b, err := xdr.TransactionResult{Result: res}.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(b)
	}

	if err := classifyResultXDR(wrapped(xdr.TransactionResultCodeTxBadSeq)); !isContention(err) {
		t.Errorf("inner txBadSeq = %v, want contention", err)
	}
	if err := classifyResultXDR(wrapped(xdr.TransactionResultCodeTxBadAuth)); !isPermanent(err) {
		t.Errorf("inner txBadAuth = %v, want permanent", err)
	}
	var fe *feeError
	if err := classifyResultXDR(wrapped(xdr.TransactionResultCodeTxInsufficientFee)); !errors.As(err, &fe) {
		t.Errorf("inner txInsufficientFee = %v, want fee error", err)
	}
}

// Stellar Core queues one transaction per source account. A channel handed on
// while its transaction is pending gets txBadSeq for the next one, so the
// lease is held until the transaction settles.
func TestChannel_leaseHeldUntilConfirmed(t *testing.T) {
	rpc := successRPC(t, "out-held")
	f, st, l, _ := channelForwarder(t, rpc)
	releasedDuringPoll := -1
	rpc.onPoll = func() {
		l.mu.Lock()
		releasedDuringPoll = len(l.released)
		l.mu.Unlock()
	}

	forward(f, "in-held")

	if len(st.doneCalls) != 1 {
		t.Fatalf("forward not completed: %v", st.markFailedCalls)
	}
	if releasedDuringPoll != 0 {
		t.Fatalf("channel released %d times before the poll, want held until confirmed", releasedDuringPoll)
	}
	if r := l.only(t); r.resync || r.seq == nil || *r.seq != 501 {
		t.Fatalf("release = %+v, want sequence 501 consumed", r)
	}
}

// Accepted but never seen to settle: it may still be queued, so the next
// holder reloads the sequence.
func TestChannel_unconfirmedPollResyncs(t *testing.T) {
	rpc := successRPC(t, "out")
	rpc.pollErr = errors.New("poll timed out")
	f, _, l, _ := channelForwarder(t, rpc)

	forward(f, "in-poll-unconfirmed")

	if r := l.only(t); !r.resync || r.seq != nil {
		t.Fatalf("release = %+v, want resync", r)
	}
}

// Failed in a ledger: the sequence number was still consumed.
func TestChannel_failedOnChainConsumesSequence(t *testing.T) {
	rpc := successRPC(t, "out")
	rpc.pollResp = rpcprotocol.GetTransactionResponse{
		TransactionDetails: rpcprotocol.TransactionDetails{
			Status:    rpcprotocol.TransactionStatusFailed,
			ResultXDR: makeResultXDR(t, xdr.TransactionResultCodeTxBadAuth),
		},
	}
	f, _, l, _ := channelForwarder(t, rpc)

	forward(f, "in-failed")

	if r := l.only(t); r.resync || r.seq == nil || *r.seq != 501 {
		t.Fatalf("release = %+v, want sequence 501 consumed", r)
	}
}

// txInsufficientFee on every fee retry usually means this sequence number is
// already queued (Core wants 10x to replace it), so the channel must be
// resynced rather than released at a sequence the network has moved past.
func TestChannel_feeRetriesExhaustedResyncs(t *testing.T) {
	rpc := successRPC(t, "out")
	rpc.sendResp = rpcprotocol.SendTransactionResponse{Status: "ERROR", ErrorResultXDR: makeResultXDR(t, xdr.TransactionResultCodeTxInsufficientFee)}
	f, _, l, _ := channelForwarder(t, rpc)

	forward(f, "in-fee")

	if rpc.sent == 0 || rpc.sent%(maxFeeRetries+1) != 0 {
		t.Fatalf("sent %d, want whole rounds of %d fee attempts", rpc.sent, maxFeeRetries+1)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.released) != rpc.sent/(maxFeeRetries+1) {
		t.Fatalf("released %d times for %d rounds", len(l.released), rpc.sent/(maxFeeRetries+1))
	}
	for _, r := range l.released {
		if !r.resync || r.seq != nil {
			t.Fatalf("release = %+v, want resync", r)
		}
	}
}
