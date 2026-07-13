package forwarder

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/store"
)

// forwardStore is the subset of store.Store that Forwarder calls.
// Narrow interface keeps test mocks small.
type forwardStore interface {
	InsertForward(ctx context.Context, txHash string, memoID uint64, fromAddress, amount, asset string) error
	GetIntentByMemoID(ctx context.Context, memoID uint64) (*store.Intent, error)
	MarkForwardDone(ctx context.Context, txHash, forwardTx string) error
	CompleteIntent(ctx context.Context, memoID uint64) error
	MarkForwardFailed(ctx context.Context, txHash, status, errMsg string) error
	PermanentlyFail(ctx context.Context, txHash, errMsg string) error
	FailIntent(ctx context.Context, memoID uint64) error
}

// horizonClient is the subset of horizonclient.Client used by Forwarder.
type horizonClient interface {
	AccountDetail(request horizonclient.AccountRequest) (hProtocol.Account, error)
	SubmitTransaction(transaction *txnbuild.Transaction) (hProtocol.Transaction, error)
}

// rpcClient is the subset of rpcclient.Client used by Forwarder.
type rpcClient interface {
	SimulateTransaction(ctx context.Context, request rpcprotocol.SimulateTransactionRequest) (rpcprotocol.SimulateTransactionResponse, error)
	SendTransaction(ctx context.Context, request rpcprotocol.SendTransactionRequest) (rpcprotocol.SendTransactionResponse, error)
	PollTransaction(ctx context.Context, txHash string) (rpcprotocol.GetTransactionResponse, error)
}

const (
	// maxRetries is the ceiling of MarkForwardFailed calls before permanent failure.
	// The retry worker checks this on entry so a forward that has already failed N times
	// does not get submitted again.
	maxRetries = 5

	// maxTryAgain is how many times we sleep one ledger cycle and retry when Stellar
	// returns TRY_AGAIN_LATER within a single submit call.
	maxTryAgain = 5

	// maxFeeRetries is how many times we double the inclusion fee and resubmit after
	// a tx_insufficient_fee rejection.
	maxFeeRetries = 2

	// ledgerTime is the approximate Stellar ledger close time used for TRY_AGAIN_LATER
	// backoff and for transaction time bounds.
	ledgerTime = 5 * time.Second

	// pollTimeout is the max time we wait for a PENDING transaction to reach a
	// terminal state on-chain before declaring a transient failure.
	pollTimeout = 60 * time.Second
)

// Forwarder builds, signs, and submits the outbound payment for each inbound deposit.
type Forwarder struct {
	store   forwardStore
	config  *config.Config
	horizon horizonClient
	rpc     rpcClient
}

func New(st *store.Store, cfg *config.Config, hz *horizonclient.Client, rpc *rpcclient.Client) *Forwarder {
	return &Forwarder{store: st, config: cfg, horizon: hz, rpc: rpc}
}

// ── Error classification ─────────────────────────────────────────────

// submitError wraps any error from the submit path and records whether it is permanent.
//
// Permanent errors must not be retried — the same inputs will always produce the
// same failure (bad C-address, XDR invalid, signing failure, contract rejection).
//
// Transient errors are safe to retry (network timeout, sequence race, fee congestion,
// RPC overload). On transient failure the forward goes to pending_retry.
type submitError struct {
	permanent bool
	err       error
}

func (e *submitError) Error() string { return e.err.Error() }
func (e *submitError) Unwrap() error { return e.err }

func permanent(err error) error { return &submitError{permanent: true, err: err} }
func transient(err error) error { return &submitError{permanent: false, err: err} }

func isPermanent(err error) bool {
	var se *submitError
	return errors.As(err, &se) && se.permanent
}

// feeError is an internal sentinel returned by classifyResultXDR when the network
// rejected the transaction for insufficient fee. submit() catches this and bumps
// the inclusion fee before retrying, without counting it as a general failure.
type feeError struct{ msg string }

func (e *feeError) Error() string { return e.msg }

// ── Forward ───────────────────────────────────────────────────────────────────

// backoffs are the delays between the three quick in-process attempts in Forward.
var backoffs = []time.Duration{
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// Forward processes one inbound payment end-to-end. Safe to call in a goroutine.
func (f *Forwarder) Forward(ctx context.Context, txHash string, memoID uint64, fromAddress, amount, asset string) {
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

		// Permanent errors cannot succeed on retry — fail immediately.
		if isPermanent(err) {
			_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusFailed, err.Error())
			slog.Error("forwarder: permanent failure, not retrying", "tx_hash", txHash, "err", err)
			return
		}

		lastErr = err
		slog.Warn("forwarder: attempt failed", "tx_hash", txHash, "attempt", attempt+1, "err", err)
	}

	_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusPendingRetry, lastErr.Error())
	slog.Error("forwarder: all attempts failed, queued for retry", "tx_hash", txHash, "err", lastErr)
}

// ── Retry ─────────────────────────────────────────────────────────────────────

// Retry is called by the background retry worker for pending_retry forwards.
func (f *Forwarder) Retry(ctx context.Context, fwd store.Forward) {
	// enforce retry ceiling — a forward that has already failed maxRetries
	// times will never succeed and must be permanently closed.
	if fwd.Retries >= maxRetries {
		_ = f.store.PermanentlyFail(ctx, fwd.TxHash, fmt.Sprintf("exceeded max retries (%d)", maxRetries))
		_ = f.store.FailIntent(ctx, fwd.MemoID)
		slog.Error("forwarder: retry ceiling reached, permanently failing",
			"tx_hash", fwd.TxHash, "retries", fwd.Retries)
		return
	}

	intent, err := f.store.GetIntentByMemoID(ctx, fwd.MemoID)
	if err != nil {
		_ = f.store.PermanentlyFail(ctx, fwd.TxHash, "intent not found")
		slog.Error("forwarder: retry — intent not found", "tx_hash", fwd.TxHash)
		return
	}

	outboundHash, err := f.submit(ctx, intent.CAddress, intent.PoolAddress, fwd.Amount, fwd.Asset)
	if err != nil {
		// Permanent error — do not re-queue, close the forward for good.
		if isPermanent(err) {
			_ = f.store.PermanentlyFail(ctx, fwd.TxHash, err.Error())
			_ = f.store.FailIntent(ctx, fwd.MemoID)
			slog.Error("forwarder: retry permanent failure", "tx_hash", fwd.TxHash, "err", err)
			return
		}
		// Transient — increment retries and put back in the queue.
		_ = f.store.MarkForwardFailed(ctx, fwd.TxHash, store.StatusPendingRetry, err.Error())
		slog.Error("forwarder: retry failed (transient), re-queued",
			"tx_hash", fwd.TxHash, "retries", fwd.Retries+1, "err", err)
		return
	}

	_ = f.store.MarkForwardDone(ctx, fwd.TxHash, outboundHash)
	_ = f.store.CompleteIntent(ctx, fwd.MemoID)
	slog.Info("forwarder: retry succeeded", "inbound", fwd.TxHash, "outbound", outboundHash)
}

// ── Submit pipeline ───────────────────────────────────────────────────────────

// submit builds, simulates, signs, and submits one outbound Soroban payment.
// Errors are tagged permanent or transient so callers can route accordingly.
func (f *Forwarder) submit(ctx context.Context, cAddress, poolAddress, amount, asset string) (string, error) {
	kp, err := f.keypairFor(poolAddress)
	if err != nil {
		return "", permanent(err)
	}

	// Fresh sequence number every attempt — a previous failed submission may have
	// consumed the sequence on Horizon even if we received a timeout back.
	sourceAccount, err := f.horizon.AccountDetail(
		horizonclient.AccountRequest{AccountID: poolAddress},
	)
	if err != nil {
		return "", transient(fmt.Errorf("fetch pool account: %w", err))
	}

	if asset != "native" {
		return "", permanent(fmt.Errorf("unsupported asset %q", asset))
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
		return "", permanent(fmt.Errorf("build payment-to-contract: %w", err))
	}

	// Build with placeholder fee for simulation — Stellar RPC ignores the fee value
	// during simulation and returns the true minimum resource fee in the response.
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount:        &sourceAccount,
		IncrementSequenceNum: true,
		Operations:           []txnbuild.Operation{&op},
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimeout(300),
		},
	})
	if err != nil {
		return "", permanent(fmt.Errorf("build tx for simulation: %w", err))
	}

	// ── Simulate via Stellar RPC ──────────────────────────────────────────────
	// Soroban transactions are rejected if the footprint or resource fee doesn't
	// match the network's actual usage. Simulation gives us the ground truth once.
	env := tx.ToXDR()
	envBytes, err := env.MarshalBinary()
	if err != nil {
		return "", permanent(fmt.Errorf("marshal tx for simulation: %w", err))
	}

	simResp, err := f.rpc.SimulateTransaction(ctx, rpcprotocol.SimulateTransactionRequest{
		Transaction: base64.StdEncoding.EncodeToString(envBytes),
	})
	if err != nil {
		return "", transient(fmt.Errorf("simulate tx: %w", err))
	}
	if simResp.Error != "" {
		// Simulation errors are permanent — the same transaction will always fail
		// simulation (e.g., contract not found, invalid arguments).
		return "", permanent(fmt.Errorf("simulate tx: %s", simResp.Error))
	}

	simDataBytes, err := base64.StdEncoding.DecodeString(simResp.TransactionDataXDR)
	if err != nil {
		return "", permanent(fmt.Errorf("decode simulation data: %w", err))
	}
	var sorobanData xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshal(simDataBytes, &sorobanData); err != nil {
		return "", permanent(fmt.Errorf("unmarshal soroban data: %w", err))
	}

	// ── Submit with fee-bump retry ────────────────────────────────────────────
	// The simulation footprint is reused across fee attempts — only the inclusion
	// fee portion changes. detect tx_insufficient_fee and double the fee.
	inclusionFee := uint32(txnbuild.MinBaseFee)
	for feeAttempt := range maxFeeRetries + 1 {
		hash, err := f.signAndSend(ctx, kp, env, sorobanData, simResp.MinResourceFee, inclusionFee)
		if err == nil {
			return hash, nil
		}

		var fe *feeError
		if errors.As(err, &fe) && feeAttempt < maxFeeRetries {
			inclusionFee *= 2
			slog.Warn("forwarder: tx_insufficient_fee, bumping inclusion fee",
				"fee_attempt", feeAttempt+1, "new_inclusion_fee", inclusionFee)
			continue
		}

		// Convert lingering feeError (after exhausting retries) to a transient error
		// so the retry worker will try again later when congestion eases.
		if errors.As(err, &fe) {
			return "", transient(fmt.Errorf("%s (fee retries exhausted)", fe.msg))
		}
		return "", err
	}

	return "", transient(fmt.Errorf("insufficient fee after %d retries", maxFeeRetries))
}

// signAndSend applies simulation results to the envelope, re-signs it, submits via
// Stellar RPC, and polls for the terminal state.
//
// submits via rpc.SendTransaction (not horizon.SubmitTransaction) so we receive
// Stellar-native status codes including TRY_AGAIN_LATER and ERROR result codes.
//
// retries on TRY_AGAIN_LATER up to maxTryAgain times with a one-ledger sleep.
func (f *Forwarder) signAndSend(
	ctx context.Context,
	kp *keypair.Full,
	env xdr.TransactionEnvelope,
	sorobanData xdr.SorobanTransactionData,
	minResourceFee int64,
	inclusionFee uint32,
) (string, error) {
	// Apply the simulation footprint and total fee.
	// total = inclusion (per-op base fee) + resource (from simulation) + buffer.
	env.V1.Tx.Ext = xdr.TransactionExt{V: 1, SorobanData: &sorobanData}
	env.V1.Tx.Fee = xdr.Uint32(inclusionFee + uint32(minResourceFee) + 10_000)

	// Marshal, re-parse, and sign so the signature covers the updated fee and footprint.
	updatedBytes, err := env.MarshalBinary()
	if err != nil {
		return "", permanent(fmt.Errorf("marshal updated tx: %w", err))
	}
	generic, err := txnbuild.TransactionFromXDR(base64.StdEncoding.EncodeToString(updatedBytes))
	if err != nil {
		return "", permanent(fmt.Errorf("parse updated tx: %w", err))
	}
	tx, ok := generic.Transaction()
	if !ok {
		return "", permanent(fmt.Errorf("updated envelope is not a simple transaction"))
	}
	tx, err = tx.Sign(f.config.NetworkPassphrase, kp)
	if err != nil {
		return "", permanent(fmt.Errorf("sign tx: %w", err))
	}

	signedEnv := tx.ToXDR()
	signedBytes, err := signedEnv.MarshalBinary()
	if err != nil {
		return "", permanent(fmt.Errorf("marshal signed tx: %w", err))
	}
	signedB64 := base64.StdEncoding.EncodeToString(signedBytes)

	// TRY_AGAIN_LATER retry loop — Stellar Core's mempool can be temporarily full.
	// The correct response is to wait one ledger cycle (~5s) and resubmit the same
	// signed envelope. We do not count these as failures.
	for attempt := range maxTryAgain + 1 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", transient(ctx.Err())
			case <-time.After(ledgerTime):
			}
		}

		resp, err := f.rpc.SendTransaction(ctx, rpcprotocol.SendTransactionRequest{
			Transaction: signedB64,
		})
		if err != nil {
			return "", transient(fmt.Errorf("send transaction: %w", err))
		}

		switch resp.Status {
		case "PENDING":
			// Transaction accepted by Stellar Core — poll until ledger inclusion.
			return f.pollResult(ctx, resp.Hash)
		case "DUPLICATE":
			// Already submitted (e.g. a previous attempt that timed out on our side
			// but succeeded on the network). Poll for the existing result.
			return f.pollResult(ctx, resp.Hash)
		case "TRY_AGAIN_LATER":
			slog.Warn("forwarder: TRY_AGAIN_LATER, sleeping one ledger",
				"attempt", attempt+1, "max", maxTryAgain)
			continue
		case "ERROR":
			return "", classifyResultXDR(resp.ErrorResultXDR)
		default:
			return "", transient(fmt.Errorf("unknown send status: %s", resp.Status))
		}
	}

	return "", transient(fmt.Errorf("TRY_AGAIN_LATER after %d retries", maxTryAgain))
}

// pollResult waits for a PENDING transaction to reach SUCCESS or FAILED on-chain.
// Uses the SDK's built-in exponential-backoff poller (500ms → 3.5s intervals).
func (f *Forwarder) pollResult(ctx context.Context, hash string) (string, error) {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	result, err := f.rpc.PollTransaction(pollCtx, hash)
	if err != nil {
		return "", transient(fmt.Errorf("poll transaction %s: %w", hash, err))
	}

	switch result.Status {
	case rpcprotocol.TransactionStatusSuccess:
		return hash, nil
	case rpcprotocol.TransactionStatusFailed:
		// Classify the on-chain failure to decide whether retry makes sense.
		return "", classifyResultXDR(result.ResultXDR)
	default:
		return "", transient(fmt.Errorf("unexpected poll status %s for %s", result.Status, hash))
	}
}

// classifyResultXDR decodes a base64-encoded TransactionResult XDR and returns a
// classified error. This is used for both ERROR (pre-ledger rejection) and FAILED
// (included in ledger but operation failed) responses.
//
// Permanent codes: the same transaction will always produce the same result — do not retry.
// Transient codes: a fresh submission may succeed — put back in pending_retry.
// feeError: the caller (submit) catches this and bumps the inclusion fee.
func classifyResultXDR(resultXDR string) error {
	if resultXDR == "" {
		return transient(fmt.Errorf("transaction rejected (no result XDR)"))
	}

	b, err := base64.StdEncoding.DecodeString(resultXDR)
	if err != nil {
		return transient(fmt.Errorf("transaction rejected (cannot decode result XDR): %w", err))
	}
	var result xdr.TransactionResult
	if err := xdr.SafeUnmarshal(b, &result); err != nil {
		return transient(fmt.Errorf("transaction rejected (cannot parse result XDR): %w", err))
	}

	code := result.Result.Code
	switch code {
	// ── Fee error — handled by the fee-bump loop in submit() ──────────────────
	case xdr.TransactionResultCodeTxInsufficientFee:
		return &feeError{msg: fmt.Sprintf("tx_insufficient_fee (result code %d)", int32(code))}

	// ── Transient — a fresh attempt may succeed ────────────────────────────────
	case xdr.TransactionResultCodeTxTooEarly,
		xdr.TransactionResultCodeTxTooLate,   // time bounds expired; retry builds a fresh tx
		xdr.TransactionResultCodeTxBadSeq,    // sequence race; retry fetches a fresh sequence
		xdr.TransactionResultCodeTxInternalError:
		return transient(fmt.Errorf("transaction rejected with transient code %s", code.String()))

	// ── Permanent — do not retry ───────────────────────────────────────────────
	default:
		return permanent(fmt.Errorf("transaction rejected with permanent code %s", code.String()))
	}
}

// ── Sweep (classic XLM payment to G-address recovery account) ────────────────

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
