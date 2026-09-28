package forwarder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// authValidityLedgers is how long the pool's signature on a forward's auth
// entry stays valid: past the transaction's own max time (txValidity, about 24
// ledgers), so the transaction expires first and the auth is never the reason
// an otherwise valid forward fails.
const authValidityLedgers = 60

// prepareChannelPayment builds the SAC transfer pool → cAddress with ch as the
// transaction source (#48). The pool approves the transfer by signing its
// authorization entry and pays the fee by fee-bump; the channel only supplies
// the sequence number, and with it a ledger slot of its own.
//
// Two simulations: the first (recording mode) returns the pool's auth entry and
// its nonce; the second, with the pool's signature in place, prices the
// transaction including the signature check.
func (f *Forwarder) prepareChannelPayment(
	ctx context.Context,
	src *txnbuild.SimpleAccount,
	ch, pool *keypair.Full,
	cAddress, amount, asset string,
	validUntil time.Time,
) (prepared, error) {
	parsedAsset, err := parseAsset(asset)
	if err != nil {
		return prepared{}, permanent(err)
	}
	// SourceAccount here is the transfer's `from`: the pool.
	op, err := txnbuild.NewPaymentToContract(txnbuild.PaymentToContractParams{
		NetworkPassphrase: f.config.NetworkPassphrase,
		Destination:       cAddress,
		Amount:            amount,
		Asset:             parsedAsset,
		SourceAccount:     pool.Address(),
	})
	if err != nil {
		return prepared{}, permanent(fmt.Errorf("build payment-to-contract: %w", err))
	}
	// The channel sends it; the pool authorizes by auth entry, not as the
	// operation's source. Simulation supplies the footprint.
	op.SourceAccount = ""
	op.Ext = xdr.TransactionExt{}

	build := func(auth []xdr.SorobanAuthorizationEntry) (*txnbuild.Transaction, error) {
		op.Auth = auth
		return txnbuild.NewTransaction(txnbuild.TransactionParams{
			SourceAccount:        src,
			IncrementSequenceNum: false, // the sequencer already chose the number
			Operations:           []txnbuild.Operation{&op},
			BaseFee:              txnbuild.MinBaseFee,
			Preconditions:        txnbuild.Preconditions{TimeBounds: txnbuild.NewTimebounds(0, validUntil.Unix())},
		})
	}

	tx, err := build(nil)
	if err != nil {
		return prepared{}, permanent(fmt.Errorf("build tx for simulation: %w", err))
	}
	sim, err := f.simulate(ctx, tx)
	if err != nil {
		return prepared{}, err
	}
	if len(sim.Results) == 0 || sim.Results[0].AuthXDR == nil || len(*sim.Results[0].AuthXDR) == 0 {
		return prepared{}, permanent(fmt.Errorf("simulation returned no authorization entry for the pool"))
	}
	var signed []xdr.SorobanAuthorizationEntry
	for _, raw := range *sim.Results[0].AuthXDR {
		var entry xdr.SorobanAuthorizationEntry
		if err := xdr.SafeUnmarshalBase64(raw, &entry); err != nil {
			return prepared{}, permanent(fmt.Errorf("decode auth entry: %w", err))
		}
		if entry, err = signAuthEntry(entry, pool, f.config.NetworkPassphrase, sim.LatestLedger+authValidityLedgers); err != nil {
			return prepared{}, permanent(err)
		}
		signed = append(signed, entry)
	}

	if tx, err = build(signed); err != nil {
		return prepared{}, permanent(fmt.Errorf("build signed tx: %w", err))
	}
	sim, err = f.simulate(ctx, tx)
	if err != nil {
		return prepared{}, err
	}
	var sorobanData xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshalBase64(sim.TransactionDataXDR, &sorobanData); err != nil {
		return prepared{}, permanent(fmt.Errorf("unmarshal soroban data: %w", err))
	}
	resourceFee := sim.MinResourceFee

	return prepared{
		env: tx.ToXDR(),
		setFee: func(env *xdr.TransactionEnvelope, inclusionFee uint32) {
			env.V1.Tx.Ext = xdr.TransactionExt{V: 1, SorobanData: &sorobanData}
			env.V1.Tx.Fee = xdr.Uint32(inclusionFee + uint32(resourceFee) + 10_000)
		},
		sign: func(env xdr.TransactionEnvelope) (signedTx, error) {
			return f.feeBump(env, ch, pool)
		},
	}, nil
}

// feeBump signs the inner transaction as the channel and wraps it in a
// fee-bump paid and signed by the pool, so channels never spend their own XLM.
// The hash returned is the fee-bump's: the one the network reports the
// transaction under.
func (f *Forwarder) feeBump(env xdr.TransactionEnvelope, ch, pool *keypair.Full) (signedTx, error) {
	raw, err := env.MarshalBinary()
	if err != nil {
		return signedTx{}, permanent(fmt.Errorf("marshal inner tx: %w", err))
	}
	generic, err := txnbuild.TransactionFromXDR(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		return signedTx{}, permanent(fmt.Errorf("parse inner tx: %w", err))
	}
	inner, ok := generic.Transaction()
	if !ok {
		return signedTx{}, permanent(fmt.Errorf("inner envelope is not a simple transaction"))
	}
	if inner, err = inner.Sign(f.config.NetworkPassphrase, ch); err != nil {
		return signedTx{}, permanent(fmt.Errorf("sign inner tx: %w", err))
	}
	fb, err := txnbuild.NewFeeBumpTransaction(txnbuild.FeeBumpTransactionParams{
		Inner:      inner,
		FeeAccount: pool.Address(),
		BaseFee:    int64(env.V1.Tx.Fee),
	})
	if err != nil {
		return signedTx{}, permanent(fmt.Errorf("build fee-bump: %w", err))
	}
	if fb, err = fb.Sign(f.config.NetworkPassphrase, pool); err != nil {
		return signedTx{}, permanent(fmt.Errorf("sign fee-bump: %w", err))
	}
	b64, err := fb.Base64()
	if err != nil {
		return signedTx{}, permanent(fmt.Errorf("marshal fee-bump: %w", err))
	}
	hash, err := fb.HashHex(f.config.NetworkPassphrase)
	if err != nil {
		return signedTx{}, permanent(fmt.Errorf("hash fee-bump: %w", err))
	}
	return signedTx{b64: b64, hash: hash}, nil
}

// simulate runs a transaction through RPC simulation. Transport errors are
// transient; a simulation error is permanent, since the same transaction fails
// the same way.
func (f *Forwarder) simulate(ctx context.Context, tx *txnbuild.Transaction) (rpcprotocol.SimulateTransactionResponse, error) {
	b64, err := tx.Base64()
	if err != nil {
		return rpcprotocol.SimulateTransactionResponse{}, permanent(fmt.Errorf("marshal tx for simulation: %w", err))
	}
	sim, err := f.rpc.SimulateTransaction(ctx, rpcprotocol.SimulateTransactionRequest{Transaction: b64})
	if err != nil {
		return sim, transient(fmt.Errorf("simulate tx: %w", err))
	}
	if sim.Error != "" {
		return sim, permanent(fmt.Errorf("simulate tx: %s", sim.Error))
	}
	return sim, nil
}

// channelPool hands out the forward channels (#48): powerless accounts used as
// the transaction source so forwards are not capped at one per pool per ledger.
//
// Stellar lands one Soroban transaction per source account per ledger, so a
// channel carries at most one forward at a time and is held from build until
// the transfer settles, not just until it is sent. Throughput is then the
// number of channels per ledger (100 per ledger out of one pool, measured on
// testnet; docs/deposit-channels.md).
//
// In memory, for one relayer process. Each channel's sequence number comes from
// the shared sequencer keyed by the channel's address, exactly as a pool's did.
type channelPool struct {
	free  chan *keypair.Full
	addrs map[string]bool
}

// newChannelPool returns nil when there are no channels, which leaves forwards
// sourced from the pool.
func newChannelPool(kps []*keypair.Full) *channelPool {
	if len(kps) == 0 {
		return nil
	}
	p := &channelPool{free: make(chan *keypair.Full, len(kps)), addrs: make(map[string]bool, len(kps))}
	for _, kp := range kps {
		p.free <- kp
		p.addrs[kp.Address()] = true
	}
	return p
}

// acquire waits for a free channel. release must be called exactly once, after
// the transaction sent on it has settled or been handed to the retry worker.
func (p *channelPool) acquire(ctx context.Context) (*keypair.Full, func(), error) {
	select {
	case kp := <-p.free:
		return kp, func() { p.free <- kp }, nil
	case <-ctx.Done():
		return nil, nil, fmt.Errorf("wait for a forward channel: %w", ctx.Err())
	}
}

// signAuthEntry signs an Address-credential authorization entry as kp, the way
// a classic G-account authorizes a Soroban call: ed25519 over the sha256 of the
// entry's HashIdPreimage, carried as Vec[Map{public_key, signature}].
//
// This is how the pool approves a transfer out of itself while a channel is the
// transaction source. The entry's nonce (chosen by simulation) is random, so
// every forward is a distinct transaction.
func signAuthEntry(entry xdr.SorobanAuthorizationEntry, kp *keypair.Full, passphrase string, expiryLedger uint32) (xdr.SorobanAuthorizationEntry, error) {
	if entry.Credentials.Type != xdr.SorobanCredentialsTypeSorobanCredentialsAddress || entry.Credentials.Address == nil {
		return entry, fmt.Errorf("auth entry does not use address credentials")
	}
	creds := *entry.Credentials.Address
	creds.SignatureExpirationLedger = xdr.Uint32(expiryLedger)

	preimage := xdr.HashIdPreimage{
		Type: xdr.EnvelopeTypeEnvelopeTypeSorobanAuthorization,
		SorobanAuthorization: &xdr.HashIdPreimageSorobanAuthorization{
			NetworkId:                 xdr.Hash(network.ID(passphrase)),
			Nonce:                     creds.Nonce,
			SignatureExpirationLedger: creds.SignatureExpirationLedger,
			Invocation:                entry.RootInvocation,
		},
	}
	raw, err := preimage.MarshalBinary()
	if err != nil {
		return entry, fmt.Errorf("marshal auth preimage: %w", err)
	}
	digest := sha256.Sum256(raw)
	sig, err := kp.Sign(digest[:])
	if err != nil {
		return entry, fmt.Errorf("sign auth entry: %w", err)
	}
	pub, err := strkey.Decode(strkey.VersionByteAccountID, kp.Address())
	if err != nil {
		return entry, err
	}

	pubVal, sigVal := xdr.ScBytes(pub), xdr.ScBytes(sig)
	pkKey, sigKey := xdr.ScSymbol("public_key"), xdr.ScSymbol("signature")
	m := xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &pkKey}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &pubVal}},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sigKey}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &sigVal}},
	}
	mp := &m
	vec := xdr.ScVec{{Type: xdr.ScValTypeScvMap, Map: &mp}}
	vp := &vec
	creds.Signature = xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}

	signed := entry
	signed.Credentials.Address = &creds
	return signed, nil
}
