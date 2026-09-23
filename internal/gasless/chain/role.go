package chain

import (
	"context"
	"fmt"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// HasRole asks contract.has_role(account, role) by simulation — a read, so
// nothing is signed or submitted. OZ AccessControl returns Option<u32>: Some
// (the role index) when granted, None (void) when not. source must be an
// existing account; it's only the simulated transaction's source.
func HasRole(ctx context.Context, rpc RPC, passphrase, source, contract, account, role string) (bool, error) {
	acct, err := ScAddressVal(account)
	if err != nil {
		return false, err
	}
	fn, err := InvokeContract(contract, "has_role", acct, ScSymbol(role))
	if err != nil {
		return false, err
	}

	res, err := simulate(ctx, rpc, passphrase, source, fn)
	if err != nil {
		return false, fmt.Errorf("simulate has_role: %w", err)
	}
	switch res.Type {
	case xdr.ScValTypeScvVoid:
		return false, nil
	case xdr.ScValTypeScvU32:
		return true, nil
	default:
		return false, fmt.Errorf("has_role returned unexpected %v", res.Type)
	}
}

// simulate runs a read-only contract call and returns its return value.
func simulate(ctx context.Context, rpc RPC, passphrase, source string, fn xdr.HostFunction) (xdr.ScVal, error) {
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		// Simulation ignores the sequence number, so no account lookup is needed.
		SourceAccount: &txnbuild.SimpleAccount{AccountID: source, Sequence: 0},
		Operations:    []txnbuild.Operation{&txnbuild.InvokeHostFunction{HostFunction: fn}},
		BaseFee:       txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{TimeBounds: txnbuild.NewInfiniteTimeout()},
	})
	if err != nil {
		return xdr.ScVal{}, fmt.Errorf("build simulation tx: %w", err)
	}
	b64, err := tx.Base64()
	if err != nil {
		return xdr.ScVal{}, err
	}

	resp, err := rpc.SimulateTransaction(ctx, protocol.SimulateTransactionRequest{Transaction: b64})
	if err != nil {
		return xdr.ScVal{}, err
	}
	if resp.Error != "" {
		return xdr.ScVal{}, fmt.Errorf("%s", resp.Error)
	}
	if len(resp.Results) == 0 || resp.Results[0].ReturnValueXDR == nil {
		return xdr.ScVal{}, fmt.Errorf("simulation returned no result")
	}
	var val xdr.ScVal
	if err := xdr.SafeUnmarshalBase64(*resp.Results[0].ReturnValueXDR, &val); err != nil {
		return xdr.ScVal{}, fmt.Errorf("decode return value: %w", err)
	}
	return val, nil
}
