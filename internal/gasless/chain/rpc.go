// Package chain holds the gasless service's reads and simulations against
// Stellar RPC: account state, the FeeForwarder's role check, and the ScVal
// encoding they need.
package chain

import (
	"context"

	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
)

// RPC is the subset of rpcclient.Client the gasless service calls. Taking an
// interface lets tests substitute a fake network.
type RPC interface {
	GetNetwork(ctx context.Context) (protocol.GetNetworkResponse, error)
	GetLedgerEntries(ctx context.Context, req protocol.GetLedgerEntriesRequest) (protocol.GetLedgerEntriesResponse, error)
	SimulateTransaction(ctx context.Context, req protocol.SimulateTransactionRequest) (protocol.SimulateTransactionResponse, error)
}
