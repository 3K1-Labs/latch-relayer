// Package feebump wraps a signed inner transaction in a fee-bump that bids
// exactly what the protocol requires. Shared by the deposit bridge's channel
// path and the gasless service, which both fee-bump channel-sourced
// transactions.
package feebump

import (
	"fmt"

	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// AtInnerRate wraps a signed inner transaction in an unsigned fee-bump
// paid by feeSource, bidding exactly what the protocol requires: the inner
// transaction's inclusion rate for one more operation than it has, plus its
// Soroban resource fee once.
//
// txnbuild.NewFeeBumpTransaction can't express that bid. It requires the outer
// per-operation fee to be at least the inner transaction's whole fee, which
// for a Soroban transaction includes the resource fee, and then adds the
// resource fee again. That bid was about 108,000 stroops against 49,000
// needed. The network charges the going rate, not the bid, so outside surge
// pricing the result was the same. Under surge pricing the inclusion fee rises
// toward the bid, which would have cost the fee source up to twice as much in
// exactly the busy periods channels are for.
func AtInnerRate(innerEnv xdr.TransactionEnvelope, feeSource string) (*txnbuild.FeeBumpTransaction, error) {
	if innerEnv.Type != xdr.EnvelopeTypeEnvelopeTypeTx || innerEnv.V1 == nil {
		return nil, fmt.Errorf("fee-bump: inner is %s, want a v1 transaction", innerEnv.Type)
	}
	var resourceFee int64
	if sd := innerEnv.V1.Tx.Ext.SorobanData; sd != nil {
		resourceFee = int64(sd.ResourceFee)
	}
	ops := int64(len(innerEnv.V1.Tx.Operations))
	inclusion := int64(innerEnv.V1.Tx.Fee) - resourceFee
	if ops == 0 || inclusion < ops*txnbuild.MinBaseFee {
		return nil, fmt.Errorf("fee-bump: inner fee %d leaves inclusion %d for %d operation(s)", innerEnv.V1.Tx.Fee, inclusion, ops)
	}
	// The inner inclusion rate, rounded up, for ops+1 operations.
	rate := (inclusion + ops - 1) / ops

	var source xdr.MuxedAccount
	if err := source.SetAddress(feeSource); err != nil {
		return nil, fmt.Errorf("fee-bump: fee source: %w", err)
	}
	env := xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTxFeeBump,
		FeeBump: &xdr.FeeBumpTransactionEnvelope{Tx: xdr.FeeBumpTransaction{
			FeeSource: source,
			Fee:       xdr.Int64(rate*(ops+1) + resourceFee),
			InnerTx:   xdr.FeeBumpTransactionInnerTx{Type: xdr.EnvelopeTypeEnvelopeTypeTx, V1: innerEnv.V1},
		}},
	}
	b64, err := xdr.MarshalBase64(env)
	if err != nil {
		return nil, fmt.Errorf("fee-bump: encode: %w", err)
	}
	parsed, err := txnbuild.TransactionFromXDR(b64)
	if err != nil {
		return nil, fmt.Errorf("fee-bump: parse: %w", err)
	}
	fb, ok := parsed.FeeBump()
	if !ok {
		return nil, fmt.Errorf("fee-bump: parsed envelope is not a fee-bump")
	}
	return fb, nil
}
