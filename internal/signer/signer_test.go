package signer

import (
	"context"
	"testing"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/network"
	"github.com/stellar/go-stellar-sdk/txnbuild"
)

// SignTx through the interface must produce exactly what the SDK's own
// keypair signing produces — same signature, same hint.
func TestSignTxMatchesSDKSigning(t *testing.T) {
	kp := keypair.MustRandom()
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: &txnbuild.SimpleAccount{AccountID: kp.Address(), Sequence: 1},
		Operations:    []txnbuild.Operation{&txnbuild.BumpSequence{BumpTo: 2}},
		BaseFee:       txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
	})
	if err != nil {
		t.Fatal(err)
	}

	ours, err := SignTx(context.Background(), FromKeypair(kp), tx, network.TestNetworkPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	sdk, err := tx.Sign(network.TestNetworkPassphrase, kp)
	if err != nil {
		t.Fatal(err)
	}

	a, _ := ours.Base64()
	b, _ := sdk.Base64()
	if a != b {
		t.Fatal("signed envelope differs from SDK signing")
	}
}
