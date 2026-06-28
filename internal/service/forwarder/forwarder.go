package forwarder

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	"github.com/stellar/go-stellar-sdk/txnbuild"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/store"
)

// Forwarder builds, signs, and submits the outbound payment for each inbound deposit.
// Each call to Forward runs synchronously so callers can dispatch it in a goroutine.
type Forwarder struct {
	store   *store.Store
	config  *config.Config
	horizon *horizonclient.Client
}

func New(st *store.Store, cfg *config.Config, hz *horizonclient.Client) *Forwarder {
	return &Forwarder{store: st, config: cfg, horizon: hz}
}

// backoffs mirrors the retry strategy in ARCHITECTURE.md:
// 3 quick attempts with 0.5s → 1s → 2s between them.
var backoffs = []time.Duration{
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// Forward processes one inbound payment end-to-end.
// It inserts the forward record, resolves the C-address, and submits the outbound tx.
// On 3 consecutive failures it marks the record pending_retry for the background worker.
// Safe to call in a goroutine — the SSE watcher does exactly that.
func (f *Forwarder) Forward(ctx context.Context, txHash string, memoID uint64, fromAddress, amount, asset string) {
	// Insert immediately so we have an audit trail even if everything else fails.
	if err := f.store.InsertForward(ctx, txHash, memoID, fromAddress, amount, asset); err != nil {
		slog.Error("forwarder: insert forward", "tx_hash", txHash, "err", err)
		return
	}

	reg, err := f.store.GetRegistration(ctx, memoID)
	if err != nil {
		// Unknown memo_id — sweep to recovery and log.
		slog.Warn("forwarder: unknown memo_id, sweeping to recovery", "tx_hash", txHash, "memo_id", memoID)
		f.sweep(ctx, txHash, amount, asset)
		_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusFailed, "unknown memo_id — swept to recovery")
		return
	}

	var lastErr error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoffs[attempt-1]):
			}
		}

		outboundHash, err := f.submit(ctx, reg, amount, asset, txHash)
		if err == nil {
			_ = f.store.MarkForwardDone(ctx, txHash, outboundHash)
			slog.Info("forwarder: forwarded",
				"inbound", txHash, "outbound", outboundHash,
				"memo_id", memoID, "amount", amount)
			return
		}
		lastErr = err
		slog.Warn("forwarder: attempt failed", "tx_hash", txHash, "attempt", attempt+1, "err", err)
	}

	// All 3 attempts failed — hand off to the retry worker.
	_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusPendingRetry, lastErr.Error())
	slog.Error("forwarder: all attempts failed, queued for retry", "tx_hash", txHash, "err", lastErr)
}

// Retry is called by the background retry worker for pending_retry forwards.
// One final attempt — marks done or failed, no further queuing.
func (f *Forwarder) Retry(ctx context.Context, fwd store.Forward) {
	reg, err := f.store.GetRegistration(ctx, fwd.MemoID)
	if err != nil {
		_ = f.store.MarkForwardFailed(ctx, fwd.TxHash, store.StatusFailed, "registration not found")
		slog.Error("forwarder: retry — registration not found", "tx_hash", fwd.TxHash)
		return
	}

	outboundHash, err := f.submit(ctx, reg, fwd.Amount, fwd.Asset, fwd.TxHash)
	if err != nil {
		_ = f.store.MarkForwardFailed(ctx, fwd.TxHash, store.StatusFailed, err.Error())
		slog.Error("forwarder: retry failed", "tx_hash", fwd.TxHash, "err", err)
		return
	}

	_ = f.store.MarkForwardDone(ctx, fwd.TxHash, outboundHash)
	slog.Info("forwarder: retry succeeded", "inbound", fwd.TxHash, "outbound", outboundHash)
}

// submit builds, signs, and submits the outbound payment for one forward.
// The inbound tx hash is embedded as MEMO_HASH so the outbound tx is traceable
// back to the original deposit without storing extra data.
func (f *Forwarder) submit(ctx context.Context, reg *store.Registration, amount, asset, inboundHash string) (string, error) {
	kp, err := f.keypairFor(reg.PoolAddress)
	if err != nil {
		return "", err
	}

	// Fresh sequence number every attempt — a previous failed submission may have
	// incremented the sequence on Horizon even if we got a timeout error back.
	sourceAccount, err := f.horizon.AccountDetail(
		horizonclient.AccountRequest{AccountID: reg.PoolAddress},
	)
	if err != nil {
		return "", fmt.Errorf("fetch pool account: %w", err)
	}

	if asset != "native" {
		return "", fmt.Errorf("unsupported asset %q", asset)
	}

	// Decode the hex tx hash into 32 raw bytes for MEMO_HASH.
	rawHash, err := hex.DecodeString(inboundHash)
	if err != nil || len(rawHash) != 32 {
		return "", fmt.Errorf("invalid inbound tx hash %q", inboundHash)
	}
	var memoHash txnbuild.MemoHash
	copy(memoHash[:], rawHash)

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &sourceAccount,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			&txnbuild.Payment{
				Destination: reg.CAddress,
				Amount:      amount,
				Asset:       txnbuild.NativeAsset{},
			},
		},
		Memo:    memoHash,
		BaseFee: txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimeout(300),
		},
	})
	if err != nil {
		return "", fmt.Errorf("build tx: %w", err)
	}

	tx, err = tx.Sign(f.config.NetworkPassphrase, kp)
	if err != nil {
		return "", fmt.Errorf("sign tx: %w", err)
	}

	result, err := f.horizon.SubmitTransaction(tx)
	if err != nil {
		return "", fmt.Errorf("submit tx: %w", err)
	}

	return result.Hash, nil
}

// sweep sends the deposit amount to the configured recovery address.
// Called when a deposit arrives with a memo_id that has no registration.
func (f *Forwarder) sweep(ctx context.Context, inboundHash, amount, asset string) {
	if f.config.RecoveryAddress == "" {
		slog.Warn("forwarder: no recovery address configured, funds remain in pool", "tx_hash", inboundHash)
		return
	}
	if asset != "native" {
		slog.Warn("forwarder: sweep only supports native asset", "tx_hash", inboundHash, "asset", asset)
		return
	}

	pool := f.config.PoolAccounts[0]
	sourceAccount, err := f.horizon.AccountDetail(
		horizonclient.AccountRequest{AccountID: pool.Address},
	)
	if err != nil {
		slog.Error("forwarder: sweep fetch account", "err", err)
		return
	}

	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &sourceAccount,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{
			&txnbuild.Payment{
				Destination: f.config.RecoveryAddress,
				Amount:      amount,
				Asset:       txnbuild.NativeAsset{},
			},
		},
		Memo:    txnbuild.MemoText("unknown-memo"),
		BaseFee: txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimeout(300),
		},
	})
	if err != nil {
		slog.Error("forwarder: sweep build tx", "err", err)
		return
	}

	tx, err = tx.Sign(f.config.NetworkPassphrase, pool.Keypair)
	if err != nil {
		slog.Error("forwarder: sweep sign tx", "err", err)
		return
	}

	result, err := f.horizon.SubmitTransaction(tx)
	if err != nil {
		slog.Error("forwarder: sweep submit", "tx_hash", inboundHash, "err", err)
		return
	}

	slog.Info("forwarder: swept to recovery", "inbound", inboundHash, "sweep_tx", result.Hash)
}

// keypairFor finds the signing keypair for a given pool address.
func (f *Forwarder) keypairFor(poolAddress string) (*keypair.Full, error) {
	for _, pa := range f.config.PoolAccounts {
		if pa.Address == poolAddress {
			return pa.Keypair, nil
		}
	}
	return nil, fmt.Errorf("no keypair found for pool address %s", poolAddress)
}
