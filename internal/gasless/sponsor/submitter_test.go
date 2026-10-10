package sponsor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/gasless/channels"
	"github.com/latch/relayer/internal/signer"
)

// ── fakes ─────────────────────────────────────────────────────────────────────

type fakeRPC struct {
	mu        sync.Mutex
	simErr    string
	resource  int64
	p90       uint64
	sends     []string // signed envelopes, in order
	sendQueue []protocol.SendTransactionResponse
	pollResp  protocol.GetTransactionResponse
	pollErr   error
	accountSq int64
	// record mode: the auth entries a recording simulation returns
	recordAuth []string
	simModes   []string
	simTxs     []string
}

func (f *fakeRPC) GetNetwork(context.Context) (protocol.GetNetworkResponse, error) {
	return protocol.GetNetworkResponse{Passphrase: network.TestNetworkPassphrase}, nil
}

func (f *fakeRPC) GetLedgerEntries(_ context.Context, req protocol.GetLedgerEntriesRequest) (protocol.GetLedgerEntriesResponse, error) {
	var out protocol.GetLedgerEntriesResponse
	for _, k := range req.Keys {
		var lk xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(k, &lk); err != nil {
			return out, err
		}
		data := xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeAccount, Account: &xdr.AccountEntry{
			AccountId: lk.Account.AccountId, Balance: 10_000_000, SeqNum: xdr.SequenceNumber(f.accountSq),
		}}
		b64, _ := xdr.MarshalBase64(data)
		out.Entries = append(out.Entries, protocol.LedgerEntryResult{KeyXDR: k, DataXDR: b64})
	}
	return out, nil
}

func (f *fakeRPC) GetLatestLedger(context.Context) (protocol.GetLatestLedgerResponse, error) {
	return protocol.GetLatestLedgerResponse{Sequence: 5000}, nil
}

func (f *fakeRPC) SimulateTransaction(_ context.Context, req protocol.SimulateTransactionRequest) (protocol.SimulateTransactionResponse, error) {
	f.mu.Lock()
	f.simModes = append(f.simModes, req.AuthMode)
	f.simTxs = append(f.simTxs, req.Transaction)
	f.mu.Unlock()
	if req.AuthMode == protocol.AuthModeRecord {
		if f.recordAuth == nil {
			return protocol.SimulateTransactionResponse{}, errors.New("unexpected record-mode simulation")
		}
		auth := f.recordAuth
		return protocol.SimulateTransactionResponse{MinResourceFee: f.resource,
			Results: []protocol.SimulateHostFunctionResult{{AuthXDR: &auth}}}, nil
	}
	if req.AuthMode != protocol.AuthModeEnforce {
		return protocol.SimulateTransactionResponse{}, errors.New("simulation must enforce auth")
	}
	if f.simErr != "" {
		return protocol.SimulateTransactionResponse{Error: f.simErr}, nil
	}
	sd := xdr.SorobanTransactionData{ResourceFee: xdr.Int64(f.resource)}
	b64, _ := xdr.MarshalBase64(sd)
	return protocol.SimulateTransactionResponse{TransactionDataXDR: b64, MinResourceFee: f.resource}, nil
}

func (f *fakeRPC) GetFeeStats(context.Context) (protocol.GetFeeStatsResponse, error) {
	return protocol.GetFeeStatsResponse{SorobanInclusionFee: protocol.FeeDistribution{P90: f.p90}}, nil
}

func (f *fakeRPC) SendTransaction(_ context.Context, req protocol.SendTransactionRequest) (protocol.SendTransactionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, req.Transaction)
	if len(f.sendQueue) == 0 {
		return protocol.SendTransactionResponse{Status: "PENDING"}, nil
	}
	r := f.sendQueue[0]
	f.sendQueue = f.sendQueue[1:]
	return r, nil
}

func (f *fakeRPC) GetTransaction(context.Context, protocol.GetTransactionRequest) (protocol.GetTransactionResponse, error) {
	return f.pollResp, f.pollErr
}

func (f *fakeRPC) PollTransaction(context.Context, string) (protocol.GetTransactionResponse, error) {
	return f.pollResp, f.pollErr
}

type released struct {
	seq    *int64
	resync bool
}

type fakeLeaser struct {
	addr     string
	seq      int64
	resync   bool
	busy     bool
	releases []released
}

func (l *fakeLeaser) AcquireWait(context.Context, time.Duration, time.Duration) (*channels.Lease, error) {
	if l.busy {
		return nil, channels.ErrPoolCapacity
	}
	return &channels.Lease{Address: l.addr, Seq: l.seq, NeedsResync: l.resync, Token: "t"}, nil
}

func (l *fakeLeaser) Release(_ context.Context, _ *channels.Lease, seq *int64, resync bool) error {
	l.releases = append(l.releases, released{seq, resync})
	return nil
}

type fakeRecords struct {
	recs     map[string]Record
	reserved []Reservation
	capErr   error
}

func (r *fakeRecords) Get(_ context.Context, id string) (Record, bool, error) {
	rec, ok := r.recs[id]
	return rec, ok, nil
}

func (r *fakeRecords) Reserve(_ context.Context, res Reservation, _ Limits) (Record, bool, error) {
	if r.capErr != nil {
		return Record{}, false, r.capErr
	}
	r.reserved = append(r.reserved, res)
	rec := Record{RequestID: res.RequestID, Wallet: res.Wallet, Mode: res.Mode, Status: StatusPending, payloadHash: res.PayloadHash}
	r.recs[res.RequestID] = rec
	return rec, false, nil
}

func (r *fakeRecords) Finish(_ context.Context, id string, o Outcome) (Record, error) {
	rec := r.recs[id]
	rec.Status, rec.TxHash, rec.FeeChargedStroops, rec.ErrorCode, rec.ErrorMessage =
		o.Status, o.TxHash, o.FeeChargedStroops, o.ErrorCode, o.ErrorMessage
	r.recs[id] = rec
	return rec, nil
}

func (r *fakeRecords) SetHash(_ context.Context, id, hash string) error {
	rec := r.recs[id]
	rec.TxHash = hash
	r.recs[id] = rec
	return nil
}

func (r *fakeRecords) Stale(context.Context, time.Duration) ([]StaleRecord, error) { return nil, nil }

// ── fixture ───────────────────────────────────────────────────────────────────

type fixture struct {
	sub     *Submitter
	rpc     *fakeRPC
	leaser  *fakeLeaser
	records *fakeRecords
	channel *keypair.Full
	funder  *keypair.Full
	req     Request
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ledgerTime = time.Millisecond
	t.Cleanup(func() { ledgerTime = 5 * time.Second })

	wallet, walletSc := contractAddr(t, 1)
	policy, err := ParsePolicy("wallet:add_context_rule")
	if err != nil {
		t.Fatal(err)
	}
	channel, funder := keypair.MustRandom(), keypair.MustRandom()
	f := &fixture{
		rpc:     &fakeRPC{resource: 50_000, p90: 300, accountSq: 41, pollResp: successResp(t, 80_000)},
		leaser:  &fakeLeaser{addr: channel.Address(), seq: 100},
		records: &fakeRecords{recs: map[string]Record{}},
		channel: channel,
		funder:  funder,
	}
	f.sub = &Submitter{
		RPC: f.rpc, Channels: f.leaser, Records: f.records,
		ChannelKeys:     map[string]*keypair.Full{channel.Address(): channel},
		Funder:          signer.FromKeypair(funder),
		Passphrase:      network.TestNetworkPassphrase,
		Policy:          policy,
		Limits:          Limits{MaxTxPerWallet: 5, MaxStroopsPerWallet: 20_000_000, MaxStroopsPerDay: 1_000_000_000},
		MaxInclusionFee: 20_000,
		Available:       func() bool { return true },
		RelayerAccounts: map[string]bool{channel.Address(): true, funder.Address(): true},
	}
	invoke := invokeArgs(walletSc, "add_context_rule")
	f.req = Request{RequestID: "req_00001", Wallet: wallet, Mode: ModeSponsored,
		Transaction: envelopeB64(t, invokeOp(invoke, addressAuth(walletSc, invoke)))}
	return f
}

func resultXDR(t *testing.T, code xdr.TransactionResultCode, fee int64) string {
	t.Helper()
	inner := xdr.InnerTransactionResultPair{Result: xdr.InnerTransactionResult{
		Result: xdr.InnerTransactionResultResult{Code: code, Results: &[]xdr.OperationResult{}},
	}}
	outer := xdr.TransactionResultCodeTxFeeBumpInnerSuccess
	if code != xdr.TransactionResultCodeTxSuccess {
		outer = xdr.TransactionResultCodeTxFeeBumpInnerFailed
	}
	r := xdr.TransactionResult{FeeCharged: xdr.Int64(fee), Result: xdr.TransactionResultResult{Code: outer, InnerResultPair: &inner}}
	b64, err := xdr.MarshalBase64(r)
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

func successResp(t *testing.T, fee int64) protocol.GetTransactionResponse {
	return protocol.GetTransactionResponse{TransactionDetails: protocol.TransactionDetails{
		Status: protocol.TransactionStatusSuccess, ResultXDR: resultXDR(t, xdr.TransactionResultCodeTxSuccess, fee),
	}}
}

func rejected(t *testing.T, code xdr.TransactionResultCode) protocol.SendTransactionResponse {
	return protocol.SendTransactionResponse{Status: "ERROR", ErrorResultXDR: resultXDR(t, code, 0)}
}

func (f *fixture) run(t *testing.T) Record {
	t.Helper()
	p, existing, err := f.sub.Prepare(context.Background(), f.req)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if existing {
		t.Fatal("prepare: unexpected existing record")
	}
	return f.sub.Run(context.Background(), p)
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestSubmit_Success(t *testing.T) {
	f := newFixture(t)
	rec := f.run(t)

	if rec.Status != StatusSuccess || rec.FeeChargedStroops == nil || *rec.FeeChargedStroops != 80_000 {
		t.Fatalf("record = %+v", rec)
	}
	if len(f.records.reserved) != 1 || f.records.reserved[0].MaxFeeStroops != 2*20_000+50_000 {
		t.Fatalf("reservation = %+v", f.records.reserved)
	}

	// Fee-bump from the funder around the channel's transaction at seq+1.
	if len(f.rpc.sends) != 1 {
		t.Fatalf("sends = %d", len(f.rpc.sends))
	}
	parsed, err := txnbuild.TransactionFromXDR(f.rpc.sends[0])
	if err != nil {
		t.Fatal(err)
	}
	fb, ok := parsed.FeeBump()
	if !ok {
		t.Fatal("sent envelope is not a fee-bump")
	}
	if fb.FeeAccount() != f.funder.Address() {
		t.Fatalf("fee source = %s, want funder", fb.FeeAccount())
	}
	inner := fb.InnerTransaction()
	if inner.SourceAccount().AccountID != f.channel.Address() || inner.SequenceNumber() != 101 {
		t.Fatalf("inner source %s seq %d", inner.SourceAccount().AccountID, inner.SequenceNumber())
	}
	if len(inner.Signatures()) != 1 || len(fb.Signatures()) != 1 {
		t.Fatalf("signatures: inner %d, outer %d", len(inner.Signatures()), len(fb.Signatures()))
	}
	innerEnv := inner.ToXDR()
	if fee := int64(innerEnv.V1.Tx.Fee); fee != 300+50_000 {
		t.Fatalf("inner fee = %d, want p90 inclusion + resource fee", fee)
	}

	if len(f.leaser.releases) != 1 || f.leaser.releases[0].seq == nil || *f.leaser.releases[0].seq != 101 || f.leaser.releases[0].resync {
		t.Fatalf("release = %+v, want consumed seq 101", f.leaser.releases)
	}
}

func TestSubmit_ResyncsChannelSequence(t *testing.T) {
	f := newFixture(t)
	f.leaser.resync = true // the network says 41
	f.run(t)
	parsed, _ := txnbuild.TransactionFromXDR(f.rpc.sends[0])
	fb, _ := parsed.FeeBump()
	if got := fb.InnerTransaction().SequenceNumber(); got != 42 {
		t.Fatalf("seq = %d, want network seq + 1", got)
	}
}

func TestSubmit_BadSeqRetriesOnFreshLease(t *testing.T) {
	f := newFixture(t)
	f.rpc.sendQueue = []protocol.SendTransactionResponse{rejected(t, xdr.TransactionResultCodeTxBadSeq)}
	rec := f.run(t)
	if rec.Status != StatusSuccess || len(f.rpc.sends) != 2 {
		t.Fatalf("status %s after %d sends", rec.Status, len(f.rpc.sends))
	}
	if !f.leaser.releases[0].resync {
		t.Fatal("a txBadSeq lease must be released for resync")
	}
}

func TestSubmit_InsufficientFeeDoublesInclusion(t *testing.T) {
	f := newFixture(t)
	f.rpc.sendQueue = []protocol.SendTransactionResponse{rejected(t, xdr.TransactionResultCodeTxInsufficientFee)}
	if rec := f.run(t); rec.Status != StatusSuccess {
		t.Fatalf("status = %s", rec.Status)
	}
	parsed, _ := txnbuild.TransactionFromXDR(f.rpc.sends[1])
	fb, _ := parsed.FeeBump()
	env := fb.InnerTransaction().ToXDR()
	if fee := int64(env.V1.Tx.Fee); fee != 600+50_000 {
		t.Fatalf("retry inner fee = %d, want doubled inclusion", fee)
	}
}

func TestSubmit_TryAgainLaterExhausted(t *testing.T) {
	f := newFixture(t)
	for range maxTryAgain + 1 {
		f.rpc.sendQueue = append(f.rpc.sendQueue, protocol.SendTransactionResponse{Status: "TRY_AGAIN_LATER"})
	}
	rec := f.run(t)
	if rec.Status != StatusRejected || rec.ErrorCode != "network_busy" {
		t.Fatalf("record = %+v", rec)
	}
	if r := f.leaser.releases[0]; r.resync || r.seq == nil || *r.seq != 100 {
		t.Fatalf("release = %+v, want the unused sequence returned", r)
	}
}

func TestSubmit_OnChainFailureIsFailedAndCharged(t *testing.T) {
	f := newFixture(t)
	f.rpc.pollResp = protocol.GetTransactionResponse{TransactionDetails: protocol.TransactionDetails{
		Status: protocol.TransactionStatusFailed, ResultXDR: resultXDR(t, xdr.TransactionResultCodeTxFailed, 70_000),
	}}
	rec := f.run(t)
	if rec.Status != StatusFailed || rec.ErrorCode != "TransactionResultCodeTxFailed" || *rec.FeeChargedStroops != 70_000 {
		t.Fatalf("record = %+v", rec)
	}
}

func TestSubmit_PollTimeoutIsUnconfirmed(t *testing.T) {
	f := newFixture(t)
	f.rpc.pollErr = context.DeadlineExceeded
	rec := f.run(t)
	if rec.Status != StatusUnconfirmed || rec.TxHash == "" {
		t.Fatalf("record = %+v", rec)
	}
	if !f.leaser.releases[0].resync {
		t.Fatal("an unconfirmed lease must be released for resync")
	}
}

func TestSubmit_ChannelsBusyChargesNothing(t *testing.T) {
	f := newFixture(t)
	f.leaser.busy = true
	rec := f.run(t)
	if rec.Status != StatusRejected || rec.ErrorCode != "channels_busy" || len(f.rpc.sends) != 0 {
		t.Fatalf("record = %+v, sends = %d", rec, len(f.rpc.sends))
	}
}

func TestPrepare_Refusals(t *testing.T) {
	t.Run("simulation failure reserves nothing", func(t *testing.T) {
		f := newFixture(t)
		f.rpc.simErr = "HostError: auth failed"
		if _, _, err := f.sub.Prepare(context.Background(), f.req); !errors.Is(err, ErrSimulation) {
			t.Fatalf("err = %v", err)
		}
		if len(f.records.reserved) != 0 {
			t.Fatal("reserved despite failed simulation")
		}
	})
	t.Run("funder below floor", func(t *testing.T) {
		f := newFixture(t)
		f.sub.Available = func() bool { return false }
		if _, _, err := f.sub.Prepare(context.Background(), f.req); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cap reached", func(t *testing.T) {
		f := newFixture(t)
		f.records.capErr = ErrWalletCap
		if _, _, err := f.sub.Prepare(context.Background(), f.req); !errors.Is(err, ErrWalletCap) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("repeat returns the stored record without simulating", func(t *testing.T) {
		f := newFixture(t)
		first := f.run(t)
		f.rpc.simErr = "would fail if simulated again"
		p, existing, err := f.sub.Prepare(context.Background(), f.req)
		if err != nil || !existing || p.Record.Status != first.Status {
			t.Fatalf("existing=%v err=%v record=%+v", existing, err, p.Record)
		}
	})
	t.Run("reused request_id with another body", func(t *testing.T) {
		f := newFixture(t)
		f.run(t)
		// Same call, different envelope bytes (envelopeB64 picks a fresh source).
		_, walletSc := contractAddr(t, 1)
		invoke := invokeArgs(walletSc, "add_context_rule")
		f.req.Transaction = envelopeB64(t, invokeOp(invoke, addressAuth(walletSc, invoke)))
		if _, _, err := f.sub.Prepare(context.Background(), f.req); !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
	})
}

func TestResolver(t *testing.T) {
	f := newFixture(t)
	r := &Resolver{Submitter: f.sub}

	if o, done := r.resolve(context.Background(), StaleRecord{RequestID: "a"}); !done || o.Status != StatusRejected {
		t.Fatalf("no hash: %+v %v", o, done)
	}
	if o, done := r.resolve(context.Background(), StaleRecord{RequestID: "b", TxHash: "h"}); !done || o.Status != StatusSuccess {
		t.Fatalf("landed: %+v %v", o, done)
	}
	f.rpc.pollResp = protocol.GetTransactionResponse{TransactionDetails: protocol.TransactionDetails{Status: protocol.TransactionStatusNotFound}}
	if _, done := r.resolve(context.Background(), StaleRecord{RequestID: "c", TxHash: "h", UpdatedAt: time.Now()}); done {
		t.Fatal("not found but still within its time bounds: must wait")
	}
	if o, done := r.resolve(context.Background(), StaleRecord{RequestID: "d", TxHash: "h", UpdatedAt: time.Now().Add(-10 * time.Minute)}); !done || o.Status != StatusRejected {
		t.Fatalf("expired: %+v %v", o, done)
	}
}
