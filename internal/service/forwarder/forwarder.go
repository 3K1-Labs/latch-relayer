package forwarder

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/store"
)

// Forwarder builds, signs, and submits the outbound payment for each inbound deposit.
// Each call to Forward runs synchronously so callers can dispatch it in a goroutine.
type Forwarder struct {
	store   *store.Store
	config  *config.Config
	horizon *horizonclient.Client
	rpc     *rpcclient.Client
}

func New(st *store.Store, cfg *config.Config, hz *horizonclient.Client, rpc *rpcclient.Client) *Forwarder {
	return &Forwarder{store: st, config: cfg, horizon: hz, rpc: rpc}
}

// backoffs mirrors the retry strategy in ARCHITECTURE.md:
// 3 quick attempts with 0.5s → 1s → 2s between them.
var backoffs = []time.Duration{
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// Forward processes one inbound payment end-to-end.
// It inserts the forward record, resolves the C-address from the intent, and submits the outbound tx.
// On 3 consecutive failures it marks the record pending_retry for the background worker.
// Safe to call in a goroutine — the SSE watcher does exactly that.
func (f *Forwarder) Forward(ctx context.Context, txHash string, memoID uint64, fromAddress, amount, asset string) {
	// Insert immediately so we have an audit trail even if everything else fails.
	if err := f.store.InsertForward(ctx, txHash, memoID, fromAddress, amount, asset); err != nil {
		slog.Error("forwarder: insert forward", "tx_hash", txHash, "err", err)
		return
	}

	intent, err := f.store.GetIntentByMemoID(ctx, memoID)
	if err != nil {
		slog.Warn("forwarder: unknown memo_id, sweeping to recovery", "tx_hash", txHash, "memo_id", memoID)
		f.sweep(ctx, txHash, amount, asset)
		_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusFailed, "unknown memo_id — swept to recovery")
		return
	}

	if intent.Status == store.IntentExpired {
		slog.Warn("forwarder: intent expired, sweeping to recovery", "tx_hash", txHash, "memo_id", memoID)
		f.sweep(ctx, txHash, amount, asset)
		_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusFailed, "intent expired — swept to recovery")
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

		outboundHash, err := f.submit(ctx, intent.CAddress, intent.PoolAddress, amount, asset)
		if err == nil {
			_ = f.store.MarkForwardDone(ctx, txHash, outboundHash)
			_ = f.store.CompleteIntent(ctx, memoID)
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
	intent, err := f.store.GetIntentByMemoID(ctx, fwd.MemoID)
	if err != nil {
		_ = f.store.MarkForwardFailed(ctx, fwd.TxHash, store.StatusFailed, "intent not found")
		slog.Error("forwarder: retry — intent not found", "tx_hash", fwd.TxHash)
		return
	}

	outboundHash, err := f.submit(ctx, intent.CAddress, intent.PoolAddress, fwd.Amount, fwd.Asset)
	if err != nil {
		_ = f.store.MarkForwardFailed(ctx, fwd.TxHash, store.StatusFailed, err.Error())
		_ = f.store.FailIntent(ctx, fwd.MemoID)
		slog.Error("forwarder: retry failed", "tx_hash", fwd.TxHash, "err", err)
		return
	}

	_ = f.store.MarkForwardDone(ctx, fwd.TxHash, outboundHash)
	_ = f.store.CompleteIntent(ctx, fwd.MemoID)
	slog.Info("forwarder: retry succeeded", "inbound", fwd.TxHash, "outbound", outboundHash)
}

// submit builds, signs, and submits the outbound payment for one forward.
func (f *Forwarder) submit(ctx context.Context, cAddress, poolAddress, amount, asset string) (string, error) {
	kp, err := f.keypairFor(poolAddress)
	if err != nil {
		return "", err
	}

	// Fresh sequence number every attempt — a previous failed submission may have
	// incremented the sequence on Horizon even if we got a timeout error back.
	sourceAccount, err := f.horizon.AccountDetail(
		horizonclient.AccountRequest{AccountID: poolAddress},
	)
	if err != nil {
		return "", fmt.Errorf("fetch pool account: %w", err)
	}

	if asset != "native" {
		return "", fmt.Errorf("unsupported asset %q", asset)
	}

	// Classic Payment only accepts G-addresses. C-addresses (Soroban contracts)
	// must be paid via the native XLM SAC's transfer function instead.
	op, err := txnbuild.NewPaymentToContract(txnbuild.PaymentToContractParams{
		NetworkPassphrase: f.config.NetworkPassphrase,
		Destination:       cAddress,
		Amount:            amount,
		Asset:             txnbuild.NativeAsset{},
		SourceAccount:     poolAddress,
	})
	if err != nil {
		return "", fmt.Errorf("build payment-to-contract: %w", err)
	}

	// Build with placeholder fee — simulation will give us the real values.
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &sourceAccount,
		IncrementSequenceNum: true,
		Operations: []txnbuild.Operation{&op},
		BaseFee:    txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimeout(300),
		},
	})
	if err != nil {
		return "", fmt.Errorf("build tx: %w", err)
	}

	// ── Simulate via Stellar RPC to get the exact footprint and resource fee ──
	// Soroban transactions are rejected if the footprint or fee doesn't match the
	// network's actual resource usage. Simulation gives us the ground truth.
	env := tx.ToXDR()
	envBytes, err := env.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("marshal tx for simulation: %w", err)
	}

	simResp, err := f.rpc.SimulateTransaction(ctx, rpcprotocol.SimulateTransactionRequest{
		Transaction: base64.StdEncoding.EncodeToString(envBytes),
	})
	if err != nil {
		return "", fmt.Errorf("simulate tx: %w", err)
	}
	if simResp.Error != "" {
		return "", fmt.Errorf("simulate tx: %s", simResp.Error)
	}

	// Decode the simulation's updated SorobanTransactionData (corrected footprint + limits).
	simDataBytes, err := base64.StdEncoding.DecodeString(simResp.TransactionDataXDR)
	if err != nil {
		return "", fmt.Errorf("decode simulation data: %w", err)
	}
	var sorobanData xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshal(simDataBytes, &sorobanData); err != nil {
		return "", fmt.Errorf("unmarshal soroban data: %w", err)
	}

	// Apply simulation results: update Ext with real footprint and set total fee.
	// Total fee = inclusion fee (min 100) + resource fee from simulation + small buffer.
	env.V1.Tx.Ext = xdr.TransactionExt{V: 1, SorobanData: &sorobanData}
	env.V1.Tx.Fee = xdr.Uint32(uint32(txnbuild.MinBaseFee) + uint32(simResp.MinResourceFee) + 10_000)

	// Re-parse from XDR so we can use the txnbuild Sign API on the updated envelope.
	updatedBytes, err := env.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("marshal updated tx: %w", err)
	}
	generic, err := txnbuild.TransactionFromXDR(base64.StdEncoding.EncodeToString(updatedBytes))
	if err != nil {
		return "", fmt.Errorf("parse updated tx: %w", err)
	}
	tx, ok := generic.Transaction()
	if !ok {
		return "", fmt.Errorf("updated envelope is not a simple transaction")
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
