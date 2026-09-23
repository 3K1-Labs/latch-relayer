// Package signer abstracts over where a Stellar signing key lives.
//
// Today every key is an env-loaded seed (Keypair). The interface exists so the
// executor and funder keys can move to a KMS for mainnet — where the private
// key never enters this process — without changing any caller.
package signer

import (
	"context"
	"fmt"

	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// Signer signs 32-byte hashes (transaction hashes, auth-entry payloads).
type Signer interface {
	Address() string
	Sign(ctx context.Context, hash [32]byte) ([]byte, error)
}

// Keypair is a Signer backed by an in-memory ed25519 seed.
type Keypair struct{ kp *keypair.Full }

func FromKeypair(kp *keypair.Full) *Keypair { return &Keypair{kp: kp} }

func (k *Keypair) Address() string { return k.kp.Address() }

func (k *Keypair) Sign(_ context.Context, hash [32]byte) ([]byte, error) {
	return k.kp.Sign(hash[:])
}

// decorated signs hash and attaches the signer's hint (the last 4 bytes of
// its public key), which validators use to match a signature to a signer.
func decorated(ctx context.Context, s Signer, hash [32]byte) (xdr.DecoratedSignature, error) {
	sig, err := s.Sign(ctx, hash)
	if err != nil {
		return xdr.DecoratedSignature{}, fmt.Errorf("sign as %s: %w", s.Address(), err)
	}
	pub, err := keypair.ParseAddress(s.Address())
	if err != nil {
		return xdr.DecoratedSignature{}, err
	}
	return xdr.NewDecoratedSignature(sig, pub.Hint()), nil
}

// SignTx adds s's signature to tx.
func SignTx(ctx context.Context, s Signer, tx *txnbuild.Transaction, passphrase string) (*txnbuild.Transaction, error) {
	hash, err := tx.Hash(passphrase)
	if err != nil {
		return nil, fmt.Errorf("hash tx: %w", err)
	}
	ds, err := decorated(ctx, s, hash)
	if err != nil {
		return nil, err
	}
	return tx.AddSignatureDecorated(ds)
}

// SignFeeBump adds s's signature to a fee-bump envelope.
func SignFeeBump(ctx context.Context, s Signer, tx *txnbuild.FeeBumpTransaction, passphrase string) (*txnbuild.FeeBumpTransaction, error) {
	hash, err := tx.Hash(passphrase)
	if err != nil {
		return nil, fmt.Errorf("hash fee-bump tx: %w", err)
	}
	ds, err := decorated(ctx, s, hash)
	if err != nil {
		return nil, err
	}
	return tx.AddSignatureDecorated(ds)
}
