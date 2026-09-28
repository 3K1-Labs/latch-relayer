package forwarder

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/gasless/keys"
)

// poolAuthEntryXDR is what recording-mode simulation returns for a transfer
// out of pool when pool is not the transaction source: an Address-credential
// entry with a nonce and no signature yet.
func poolAuthEntryXDR(t *testing.T, pool string) string {
	t.Helper()
	aid := xdr.MustAddress(pool)
	addr := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	fn := xdr.ScSymbol("transfer")
	var sac xdr.ContractId
	entry := xdr.SorobanAuthorizationEntry{
		Credentials: xdr.SorobanCredentials{
			Type: xdr.SorobanCredentialsTypeSorobanCredentialsAddress,
			Address: &xdr.SorobanAddressCredentials{
				Address:   addr,
				Nonce:     42,
				Signature: xdr.ScVal{Type: xdr.ScValTypeScvVoid},
			},
		},
		RootInvocation: xdr.SorobanAuthorizedInvocation{
			Function: xdr.SorobanAuthorizedFunction{
				Type: xdr.SorobanAuthorizedFunctionTypeSorobanAuthorizedFunctionTypeContractFn,
				ContractFn: &xdr.InvokeContractArgs{
					ContractAddress: xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &sac},
					FunctionName:    fn,
				},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

// channelForwarder is forwarderWith plus one forward channel.
func channelForwarder(t *testing.T, outHash string) (*Forwarder, *mockStore, *mockRPC, *keypair.Full, *keypair.Full) {
	t.Helper()
	pool := newTestKeypair(t)
	ch := keypair.MustRandom()
	cfg := testConfigWithKeypair(t, pool)
	cfg.Channels = []keys.Channel{{Index: 0, Keypair: ch}}

	rpc := successRPC(t, outHash)
	auth := []string{poolAuthEntryXDR(t, pool.Address())}
	rpc.simResp.Results = []rpcprotocol.SimulateHostFunctionResult{{AuthXDR: &auth}}
	rpc.simResp.LatestLedger = 1000

	st := &mockStore{intent: testIntent(pool.Address())}
	hz := &mockHorizon{
		account:  hProtocol.Account{AccountID: pool.Address(), Sequence: 100},
		accounts: map[string]hProtocol.Account{ch.Address(): {AccountID: ch.Address(), Sequence: 500}},
	}
	return &Forwarder{store: st, config: cfg, horizon: hz, rpc: rpc}, st, rpc, pool, ch
}

// With channels, a forward is a fee-bump: the channel is the inner source (its
// sequence, its signature), the pool pays and signs the fee-bump, and the pool
// approves the transfer out of itself only through a signed auth entry.
func TestForward_channelSendsFeeBumpAuthorizedByPool(t *testing.T) {
	f, st, rpc, pool, ch := channelForwarder(t, "fb-hash")

	f.Forward(context.Background(), pool.Address(), "in-ch", 1, "GABC", "10.0000000", "native", time.Now())

	if len(st.doneCalls) != 1 || st.doneCalls[0] != [2]string{"in-ch", "fb-hash"} {
		t.Fatalf("done = %v, want in-ch settled on fb-hash", st.doneCalls)
	}
	if len(rpc.simulated) != 2 {
		t.Fatalf("simulations = %d, want 2 (recording, then with the pool's signature)", len(rpc.simulated))
	}
	if rpc.sent != 1 {
		t.Fatalf("sent %d transactions, want 1", rpc.sent)
	}

	generic, err := txnbuild.TransactionFromXDR(rpc.sentXDR[0])
	if err != nil {
		t.Fatal(err)
	}
	fb, ok := generic.FeeBump()
	if !ok {
		t.Fatal("sent transaction is not a fee-bump")
	}
	if fb.FeeAccount() != pool.Address() {
		t.Errorf("fee source = %s, want the pool", fb.FeeAccount())
	}
	inner := fb.InnerTransaction()
	if inner.SourceAccount().AccountID != ch.Address() {
		t.Errorf("inner source = %s, want the channel", inner.SourceAccount().AccountID)
	}
	if got := inner.SourceAccount().Sequence; got != 501 {
		t.Errorf("inner sequence = %d, want the channel's next (501)", got)
	}

	// The recorded submission is the hash the network reports: the fee-bump's.
	fbHash, _ := fb.HashHex(network.TestNetworkPassphrase)
	if len(st.recordCalls) != 1 || st.recordCalls[0].submittedTx != fbHash {
		t.Fatalf("recorded %v, want the fee-bump hash %s", st.recordCalls, fbHash)
	}

	// The pool's auth entry is signed, and the signature verifies.
	op, ok := inner.Operations()[0].(*txnbuild.InvokeHostFunction)
	if !ok || len(op.Auth) != 1 {
		t.Fatalf("inner op = %T with %d auth entries", inner.Operations()[0], len(op.Auth))
	}
	if op.SourceAccount != "" {
		t.Errorf("op source = %q, want none: the pool authorizes by auth entry", op.SourceAccount)
	}
	creds := op.Auth[0].Credentials.Address
	if uint32(creds.SignatureExpirationLedger) != 1000+authValidityLedgers {
		t.Errorf("auth expiry = %d, want %d", creds.SignatureExpirationLedger, 1000+authValidityLedgers)
	}
	pre, _ := xdr.HashIdPreimage{
		Type: xdr.EnvelopeTypeEnvelopeTypeSorobanAuthorization,
		SorobanAuthorization: &xdr.HashIdPreimageSorobanAuthorization{
			NetworkId:                 xdr.Hash(network.ID(network.TestNetworkPassphrase)),
			Nonce:                     creds.Nonce,
			SignatureExpirationLedger: creds.SignatureExpirationLedger,
			Invocation:                op.Auth[0].RootInvocation,
		},
	}.MarshalBinary()
	digest := sha256.Sum256(pre)
	vec := **creds.Signature.Vec
	sigMap := **vec[0].Map
	if err := pool.Verify(digest[:], []byte(*sigMap[1].Val.Bytes)); err != nil {
		t.Errorf("pool's auth signature does not verify: %v", err)
	}
}

// A channel is held until its transfer settles, then returned: the next
// forward gets it back with the following sequence number.
func TestForward_channelReleasedAndReused(t *testing.T) {
	f, st, rpc, pool, ch := channelForwarder(t, "fb-hash")

	f.Forward(context.Background(), pool.Address(), "in-1", 1, "GABC", "1.0000000", "native", time.Now())
	f.Forward(context.Background(), pool.Address(), "in-2", 1, "GABC", "1.0000000", "native", time.Now())

	if len(st.doneCalls) != 2 || rpc.sent != 2 {
		t.Fatalf("done=%d sent=%d, want both forwards through the one channel", len(st.doneCalls), rpc.sent)
	}
	var seqs []int64
	for _, raw := range rpc.sentXDR {
		g, _ := txnbuild.TransactionFromXDR(raw)
		fb, _ := g.FeeBump()
		inner := fb.InnerTransaction()
		if inner.SourceAccount().AccountID != ch.Address() {
			t.Fatalf("inner source = %s", inner.SourceAccount().AccountID)
		}
		seqs = append(seqs, inner.SourceAccount().Sequence)
	}
	if seqs[0] != 501 || seqs[1] != 502 {
		t.Errorf("channel sequences = %v, want [501 502]", seqs)
	}
}

// Without channels nothing changes: the pool is the source and signs directly.
func TestForward_noChannelsKeepsPoolSource(t *testing.T) {
	f, st := forwarderWith(t, successRPC(t, "out-hash"))
	rpc := f.rpc.(*mockRPC)

	forward(f, "in-pool")

	if len(st.doneCalls) != 1 || rpc.sent != 1 {
		t.Fatalf("done=%d sent=%d", len(st.doneCalls), rpc.sent)
	}
	g, _ := txnbuild.TransactionFromXDR(rpc.sentXDR[0])
	tx, ok := g.Transaction()
	if !ok {
		t.Fatal("pool-sourced forward should not be a fee-bump")
	}
	if tx.SourceAccount().AccountID != f.config.PoolAccounts[0].Address {
		t.Errorf("source = %s, want the pool", tx.SourceAccount().AccountID)
	}
}

// A fee-bump that failed because the channel's sequence was stale is
// contention, not a permanent failure.
func TestClassifyResultXDR_feeBumpInnerBadSeq(t *testing.T) {
	inner := xdr.InnerTransactionResultPair{
		Result: xdr.InnerTransactionResult{Result: xdr.InnerTransactionResultResult{Code: xdr.TransactionResultCodeTxBadSeq}},
	}
	res := xdr.TransactionResult{Result: xdr.TransactionResultResult{
		Code:            xdr.TransactionResultCodeTxFeeBumpInnerFailed,
		InnerResultPair: &inner,
	}}
	b64, err := xdr.MarshalBase64(res)
	if err != nil {
		t.Fatal(err)
	}
	err = classifyResultXDR(b64)
	if !isContention(err) || isPermanent(err) {
		t.Fatalf("classify = %v (contention=%v permanent=%v), want contention", err, isContention(err), isPermanent(err))
	}
}
