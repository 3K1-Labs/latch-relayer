// Package startup verifies, before the gasless service takes traffic, that
// its configuration matches the network: right network, executor actually
// holds the FeeForwarder role, and the executor and funder accounts exist.
//
// These are configuration mistakes, not outages, so they fail the boot
// loudly: on Render a failed boot keeps the previous healthy deploy serving,
// which is the right outcome for a misconfigured key.
package startup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/latch/relayer/internal/gasless/chain"
)

// ExecutorRole is the FeeForwarder role that gates forward().
const ExecutorRole = "executor"

// Check is what Preflight verifies.
type Check struct {
	Passphrase     string
	FeeForwarderID string
	Executor       string
	Funder         string
}

// ErrMisconfigured wraps failures that retrying cannot fix.
var ErrMisconfigured = errors.New("gasless misconfigured")

// Preflight runs the checks, retrying transient RPC failures (the network or
// RPC provider being briefly unreachable at boot) for up to retryFor.
func Preflight(ctx context.Context, rpc chain.RPC, c Check, retryFor time.Duration) error {
	deadline := time.Now().Add(retryFor)
	backoff := time.Second
	for {
		err := preflightOnce(ctx, rpc, c)
		if err == nil || errors.Is(err, ErrMisconfigured) || time.Now().After(deadline) {
			return err
		}
		slog.Warn("gasless preflight: transient failure, retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 15*time.Second)
	}
}

func preflightOnce(ctx context.Context, rpc chain.RPC, c Check) error {
	net, err := rpc.GetNetwork(ctx)
	if err != nil {
		return fmt.Errorf("getNetwork: %w", err)
	}
	if net.Passphrase != c.Passphrase {
		return fmt.Errorf("%w: RPC_URL serves %q but NETWORK expects %q", ErrMisconfigured, net.Passphrase, c.Passphrase)
	}

	accts, err := chain.Accounts(ctx, rpc, []string{c.Executor, c.Funder})
	if err != nil {
		return err
	}
	for name, addr := range map[string]string{"executor": c.Executor, "funder": c.Funder} {
		if !accts[addr].Exists {
			return fmt.Errorf("%w: %s account %s does not exist on the network (fund it first)", ErrMisconfigured, name, addr)
		}
	}

	ok, err := chain.HasRole(ctx, rpc, c.Passphrase, c.Funder, c.FeeForwarderID, c.Executor, ExecutorRole)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s does not hold the %q role on FeeForwarder %s; the contract admin must grant_role it",
			ErrMisconfigured, c.Executor, ExecutorRole, c.FeeForwarderID)
	}
	return nil
}
