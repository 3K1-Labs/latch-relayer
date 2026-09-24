package forwarder

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	sdkamount "github.com/stellar/go-stellar-sdk/amount"
	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/clients/rpcclient"
	"github.com/stellar/go-stellar-sdk/keypair"
	hProtocol "github.com/stellar/go-stellar-sdk/protocols/horizon"
	rpcprotocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/metrics"
	"github.com/latch/relayer/internal/store"
)

// forwardStore is the subset of store.Store that Forwarder calls.
// Narrow interface keeps test mocks small.
type forwardStore interface {
	InsertForward(ctx context.Context, txHash string, memoID uint64, poolAddress, fromAddress, amount, asset string) (bool, error)
	GetIntentByMemoID(ctx context.Context, memoID uint64) (*store.Intent, error)
	MarkForwardDone(ctx context.Context, txHash, forwardTx string) error
	CompleteIntent(ctx context.Context, memoID uint64) error
	MarkForwardFailed(ctx context.Context, txHash, status, errMsg string) error
	RequeueForContention(ctx context.Context, txHash, errMsg string) error
	PermanentlyFail(ctx context.Context, txHash, errMsg string) error
	FailIntent(ctx context.Context, memoID uint64) error
	MarkSweep(ctx context.Context, txHash, reason string) error
	MarkSwept(ctx context.Context, txHash, sweepTx string) error
	RecordSubmission(ctx context.Context, txHash, submittedTx string, until time.Time) error
	ClearSubmission(ctx context.Context, txHash string) error
}

// horizonClient is the subset of horizonclient.Client used by Forwarder.
type horizonClient interface {
	AccountDetail(request horizonclient.AccountRequest) (hProtocol.Account, error)
}

// rpcClient is the subset of rpcclient.Client used by Forwarder.
type rpcClient interface {
	SimulateTransaction(ctx context.Context, request rpcprotocol.SimulateTransactionRequest) (rpcprotocol.SimulateTransactionResponse, error)
	SendTransaction(ctx context.Context, request rpcprotocol.SendTransactionRequest) (rpcprotocol.SendTransactionResponse, error)
	PollTransaction(ctx context.Context, txHash string) (rpcprotocol.GetTransactionResponse, error)
	GetTransaction(ctx context.Context, request rpcprotocol.GetTransactionRequest) (rpcprotocol.GetTransactionResponse, error)
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
	// terminal state on-chain before handing it to the retry worker to resolve.
	pollTimeout = 60 * time.Second

	// txValidity is how long an outbound forward stays valid (its time bounds).
	// It is also how long a forward whose outcome is unknown must wait before it
	// may be built again: until then the first transaction can still land, and a
	// second one would pay the C-address twice. Long enough to cover simulation,
	// the TRY_AGAIN_LATER loop and the poll; short enough that an unresolved
	// forward is retried within a few minutes.
	txValidity = 2 * time.Minute

	// landingGrace is added to a transaction's max time before treating "not
	// found" as "never landed", to absorb clock skew against ledger close times
	// and RPC ingestion lag.
	landingGrace = 30 * time.Second
)

// Forwarder builds, signs, and submits the outbound payment for each inbound deposit.
type Forwarder struct {
	store   forwardStore
	config  *config.Config
	horizon horizonClient
	rpc     rpcClient

	seqOnce   sync.Once
	sequencer *sequencer
}

func New(st *store.Store, cfg *config.Config, hz *horizonclient.Client, rpc *rpcclient.Client) *Forwarder {
	return &Forwarder{store: st, config: cfg, horizon: hz, rpc: rpc}
}

// seq returns the shared sequencer, building it on first use so a Forwarder
// assembled as a struct literal (as the tests do) behaves like one from New.
func (f *Forwarder) seq() *sequencer {
	f.seqOnce.Do(func() {
		if f.sequencer == nil {
			f.sequencer = newSequencer(f.horizon)
		}
	})
	return f.sequencer
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
	permanent   bool
	contention  bool
	unconfirmed bool
	err         error
}

func (e *submitError) Error() string { return e.err.Error() }
func (e *submitError) Unwrap() error { return e.err }

func permanent(err error) error { return &submitError{permanent: true, err: err} }
func transient(err error) error { return &submitError{permanent: false, err: err} }

// contention marks a transient failure caused by another transaction holding the
// pool account's slot rather than by anything wrong with this one.
//
// It matters because it must not consume the retry budget. A deposit that is
// merely waiting its turn is not a deposit that is failing, and letting
// contention count toward maxRetries means a busy period can permanently fail
// forwards that are holding real customer funds — the more load, the more
// likely, which is exactly backwards.
func contention(err error) error { return &submitError{contention: true, err: err} }

// unconfirmed marks a failure after which the transaction may still land: the
// send call errored, or it was accepted and never seen to settle. Retrying such a
// forward means resolving the recorded transaction, never building a new one —
// the first can be included until its time bounds pass, so a second transfer
// would pay the C-address twice for one deposit.
func unconfirmed(err error) error { return &submitError{unconfirmed: true, err: err} }

func isUnconfirmed(err error) bool {
	var se *submitError
	return errors.As(err, &se) && se.unconfirmed
}

func isContention(err error) bool {
	var se *submitError
	return errors.As(err, &se) && se.contention
}

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
//
// poolAddress is the pool account the payment arrived at. Every outbound
// transfer for this deposit — the forward or a recovery sweep — is paid out of
// that pool, because that is where the money is.
func (f *Forwarder) Forward(ctx context.Context, poolAddress, txHash string, memoID uint64, fromAddress, amount, asset string) {
	// Measured from when we first see the deposit rather than from submission,
	// so the number reflects what the customer waits for.
	started := time.Now()

	inserted, err := f.store.InsertForward(ctx, txHash, memoID, poolAddress, fromAddress, amount, asset)
	if err != nil {
		slog.Error("forwarder: insert forward", "tx_hash", txHash, "err", err)
		return
	}

	// A row already existed, so this payment was dispatched before — by a run that
	// died before saving the SSE cursor, or by a second instance streaming the same
	// pool. Forwarding again would pay the C-address twice for one deposit. Dropping
	// it here is safe: if that earlier dispatch never reached submit(), the row is
	// still `pending` and GetPendingRetries sweeps it up after five minutes.
	if !inserted {
		slog.Warn("forwarder: duplicate tx_hash, already dispatched — skipping",
			"tx_hash", txHash, "memo_id", memoID)
		return
	}

	intent, err := f.store.GetIntentByMemoID(ctx, memoID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The alertable outcome: a deposit carrying a reference this relayer never
		// issued. It is swept to recovery rather than credited, so any sustained
		// rate here means customers are paying and not being paid.
		metrics.ForwardsTotal.WithLabelValues("unknown_memo").Inc()
		slog.Warn("forwarder: unknown memo_id, sweeping to recovery", "tx_hash", txHash, "memo_id", memoID)
		f.startSweep(ctx, txHash, poolAddress, amount, asset, "unknown memo_id")
		return
	}
	if err != nil {
		// A failed lookup says nothing about the memo. Sweeping on it would send a
		// valid deposit to recovery; leave it for the retry worker instead.
		_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusPendingRetry, fmt.Sprintf("look up intent: %v", err))
		slog.Error("forwarder: intent lookup failed, queued for retry", "tx_hash", txHash, "err", err)
		return
	}

	// Two ways to be past the window: the retry worker already flipped the status,
	// or the TTL elapsed since its last tick. Testing the status alone makes
	// crediting depend on when that worker last ran, so a deposit landing in the gap
	// is forwarded against an intent the client already shows as dead — the mobile
	// app's isDepositIntentExpired keys off expires_at directly.
	if intent.Status == store.IntentExpired || time.Now().After(intent.ExpiresAt) {
		metrics.ForwardsTotal.WithLabelValues("expired").Inc()
		slog.Warn("forwarder: intent expired, sweeping to recovery",
			"tx_hash", txHash, "memo_id", memoID,
			"status", intent.Status, "expires_at", intent.ExpiresAt)
		f.startSweep(ctx, txHash, poolAddress, amount, asset, "intent expired")
		return
	}

	// expected_amt is advisory and deliberately does not gate the forward. An
	// on-ramp deposit legitimately lands off-quote — provider fees, FX movement
	// between quote and settlement, partial fills — and by the time we see it the
	// money is already on-chain. Sweeping or holding over a mismatch would burn a
	// real user's funds to enforce a number nothing validates at mint time. Log it
	// so reconciliation has a signal; credit what actually arrived.
	if intent.ExpectedAmt != nil && mismatchesExpected(*intent.ExpectedAmt, amount) {
		slog.Warn("forwarder: deposit differs from expected amount",
			"tx_hash", txHash, "memo_id", memoID,
			"expected", *intent.ExpectedAmt, "received", amount)
	}

	// A depositor can pay a different pool than the one their intent named (a
	// copied address, a stale session). The money is in the pool it was sent
	// to, so forward from there; the mismatch is only worth flagging for
	// reconciliation.
	if intent.PoolAddress != poolAddress {
		slog.Warn("forwarder: deposit arrived at a different pool than its intent named; forwarding from the receiving pool",
			"tx_hash", txHash, "memo_id", memoID,
			"intent_pool", intent.PoolAddress, "receiving_pool", poolAddress)
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

		outboundHash, err := f.submit(ctx, txHash, intent.CAddress, poolAddress, amount, asset)
		if err == nil {
			_ = f.store.MarkForwardDone(ctx, txHash, outboundHash)
			_ = f.store.CompleteIntent(ctx, memoID)
			metrics.ForwardsTotal.WithLabelValues("done").Inc()
			metrics.DepositToCreditSeconds.Observe(time.Since(started).Seconds())
			slog.Info("forwarder: forwarded",
				"inbound", txHash, "outbound", outboundHash,
				"memo_id", memoID, "amount", amount)
			return
		}

		// The transfer may be in flight, so another attempt here could pay twice.
		// Hand it to the retry worker, which resolves the recorded transaction
		// before it will build a new one. Not charged: nothing has failed yet.
		if isUnconfirmed(err) {
			_ = f.store.RequeueForContention(ctx, txHash, err.Error())
			slog.Warn("forwarder: outcome unknown, left for the retry worker to resolve",
				"tx_hash", txHash, "err", err)
			return
		}

		// Permanent errors cannot succeed on retry — fail immediately.
		if isPermanent(err) {
			_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusFailed, err.Error())
			metrics.ForwardsTotal.WithLabelValues("permanent_failure").Inc()
			slog.Error("forwarder: permanent failure, not retrying", "tx_hash", txHash, "err", err)
			return
		}

		lastErr = err
		slog.Warn("forwarder: attempt failed", "tx_hash", txHash, "attempt", attempt+1, "err", err)
	}

	// Same rule as Retry: losing the race for the pool's ledger slot is not this
	// deposit failing, so it must not consume the budget that decides whether a
	// deposit is abandoned.
	if isContention(lastErr) {
		metrics.ContentionTotal.Inc()
		_ = f.store.RequeueForContention(ctx, txHash, lastErr.Error())
		slog.Warn("forwarder: lost the pool slot, queued for retry without charge",
			"tx_hash", txHash, "err", lastErr)
		return
	}

	_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusPendingRetry, lastErr.Error())
	slog.Error("forwarder: all attempts failed, queued for retry", "tx_hash", txHash, "err", lastErr)
}

// amountTolerance is how far a received amount may drift from the intent's
// expected_amt before it is logged as a mismatch. Loose on purpose: provider fees
// and FX are applied after the quote the caller minted the intent with.
const amountTolerance = 0.05

// mismatchesExpected reports whether received drifts from expected by more than
// amountTolerance. Both are Stellar amount strings denominated in the deposit's
// own asset — expected_amt is NOT a fiat figure, and comparing one against an XLM
// amount would warn on every deposit.
//
// Returns false when either value fails to parse. expected_amt is free-form and
// nothing validates it at mint time, so an unparseable field is a caller bug, not
// evidence of a bad deposit.
func mismatchesExpected(expected, received string) bool {
	exp, err := sdkamount.ParseInt64(expected)
	if err != nil || exp <= 0 {
		return false
	}
	got, err := sdkamount.ParseInt64(received)
	if err != nil {
		return false
	}
	return math.Abs(float64(got-exp))/float64(exp) > amountTolerance
}

// ── Retry ─────────────────────────────────────────────────────────────────────

// Retry is called by the background retry worker for pending_retry forwards.
func (f *Forwarder) Retry(ctx context.Context, fwd store.Forward) {
	// A transfer already put on the network is resolved before anything else —
	// ahead of the ceiling too, since one that landed must be recorded as done,
	// not failed.
	if fwd.SubmittedTx != nil {
		if settled := f.resolveSubmitted(ctx, fwd); settled {
			return
		}
	}

	// enforce retry ceiling — a forward that has already failed maxRetries
	// times will never succeed and must be permanently closed.
	if fwd.Retries >= maxRetries {
		if fwd.Sweep {
			_ = f.store.PermanentlyFail(ctx, fwd.TxHash,
				fmt.Sprintf("not swept after %d retries, funds remain in pool", maxRetries))
		} else {
			_ = f.store.PermanentlyFail(ctx, fwd.TxHash, fmt.Sprintf("exceeded max retries (%d)", maxRetries))
			_ = f.store.FailIntent(ctx, fwd.MemoID)
		}
		slog.Error("forwarder: retry ceiling reached, permanently failing",
			"tx_hash", fwd.TxHash, "retries", fwd.Retries, "sweep", fwd.Sweep)
		return
	}

	if fwd.Sweep {
		if fwd.PoolAddress == "" {
			_ = f.store.PermanentlyFail(ctx, fwd.TxHash, "not swept: receiving pool unknown, funds remain in pool")
			return
		}
		f.sweepDeposit(ctx, fwd.TxHash, fwd.PoolAddress, fwd.Amount, fwd.Asset)
		return
	}

	intent, err := f.store.GetIntentByMemoID(ctx, fwd.MemoID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Only reachable for a row whose first attempt died before deciding. The
		// deposit carries a memo this relayer never issued: return it.
		if fwd.PoolAddress == "" {
			_ = f.store.PermanentlyFail(ctx, fwd.TxHash, "intent not found and receiving pool unknown")
			return
		}
		f.startSweep(ctx, fwd.TxHash, fwd.PoolAddress, fwd.Amount, fwd.Asset, "unknown memo_id")
		return
	}
	if err != nil {
		_ = f.store.MarkForwardFailed(ctx, fwd.TxHash, store.StatusPendingRetry, fmt.Sprintf("look up intent: %v", err))
		slog.Error("forwarder: retry — intent lookup failed", "tx_hash", fwd.TxHash, "err", err)
		return
	}

	// Deliberately no expiry check here, unlike Forward. This deposit arrived while
	// the intent was live; only our submission failed. Sweeping it now would punish
	// the depositor for the relayer's own retry latency.

	// Pay out of the pool that received the deposit. Rows recorded before the
	// receiving pool was tracked fall back to the intent's pool, which is what
	// every deposit used when intents were all pinned to one pool.
	poolAddress := fwd.PoolAddress
	if poolAddress == "" {
		poolAddress = intent.PoolAddress
	}

	outboundHash, err := f.submit(ctx, fwd.TxHash, intent.CAddress, poolAddress, fwd.Amount, fwd.Asset)
	if err != nil {
		// May be in flight — the next tick resolves it. Not charged.
		if isUnconfirmed(err) {
			_ = f.store.RequeueForContention(ctx, fwd.TxHash, err.Error())
			slog.Warn("forwarder: retry outcome unknown, will resolve next tick",
				"tx_hash", fwd.TxHash, "err", err)
			return
		}
		// Permanent error — do not re-queue, close the forward for good.
		if isPermanent(err) {
			_ = f.store.PermanentlyFail(ctx, fwd.TxHash, err.Error())
			_ = f.store.FailIntent(ctx, fwd.MemoID)
			slog.Error("forwarder: retry permanent failure", "tx_hash", fwd.TxHash, "err", err)
			return
		}
		// Contention — the pool account was busy. Re-queue without charging the
		// budget, or a busy period would permanently fail deposits that never
		// had anything wrong with them.
		if isContention(err) {
			metrics.ContentionTotal.Inc()
			_ = f.store.RequeueForContention(ctx, fwd.TxHash, err.Error())
			slog.Warn("forwarder: retry lost the pool slot, re-queued without charge",
				"tx_hash", fwd.TxHash, "retries", fwd.Retries, "err", err)
			return
		}

		// Transient — increment retries and put back in the queue.
		_ = f.store.MarkForwardFailed(ctx, fwd.TxHash, store.StatusPendingRetry, err.Error())
		slog.Error("forwarder: retry failed (transient), re-queued",
			"tx_hash", fwd.TxHash, "retries", fwd.Retries+1, "err", err)
		return
	}

	f.completeRetried(ctx, fwd, outboundHash)
	slog.Info("forwarder: retry succeeded", "inbound", fwd.TxHash, "outbound", outboundHash)
}

func (f *Forwarder) completeRetried(ctx context.Context, fwd store.Forward, outboundHash string) {
	_ = f.store.MarkForwardDone(ctx, fwd.TxHash, outboundHash)
	_ = f.store.CompleteIntent(ctx, fwd.MemoID)
	metrics.ForwardsTotal.WithLabelValues("done").Inc()
	metrics.DepositToCreditSeconds.Observe(time.Since(fwd.CreatedAt).Seconds())
}

// resolveSubmitted settles a forward whose earlier transfer was put on the
// network with its outcome unknown. It reports true when the forward is settled
// or must keep waiting, and false only when that transfer is known never to have
// moved funds — the one case in which building a new transfer is safe.
func (f *Forwarder) resolveSubmitted(ctx context.Context, fwd store.Forward) bool {
	hash := *fwd.SubmittedTx
	resp, err := f.rpc.GetTransaction(ctx, rpcprotocol.GetTransactionRequest{Hash: hash})
	if err != nil {
		_ = f.store.RequeueForContention(ctx, fwd.TxHash, fmt.Sprintf("resolve %s: %v", hash, err))
		slog.Warn("forwarder: cannot resolve in-flight transfer yet",
			"tx_hash", fwd.TxHash, "submitted_tx", hash, "err", err)
		return true
	}

	switch resp.Status {
	case rpcprotocol.TransactionStatusSuccess:
		if fwd.Sweep {
			_ = f.store.MarkSwept(ctx, fwd.TxHash, hash)
			metrics.ForwardsTotal.WithLabelValues("swept").Inc()
		} else {
			f.completeRetried(ctx, fwd, hash)
		}
		slog.Info("forwarder: in-flight transfer had landed",
			"inbound", fwd.TxHash, "outbound", hash, "sweep", fwd.Sweep)
		return true

	case rpcprotocol.TransactionStatusFailed:
		// Included but failed: the transfer did not happen. A permanent cause
		// would fail again, so close it; otherwise it is safe to build anew.
		cause := classifyResultXDR(resp.ResultXDR)
		if isPermanent(cause) {
			if fwd.Sweep {
				_ = f.store.PermanentlyFail(ctx, fwd.TxHash, "not swept, funds remain in pool: "+cause.Error())
			} else {
				_ = f.store.PermanentlyFail(ctx, fwd.TxHash, cause.Error())
				_ = f.store.FailIntent(ctx, fwd.MemoID)
			}
			slog.Error("forwarder: in-flight transfer failed on-chain, permanent",
				"tx_hash", fwd.TxHash, "submitted_tx", hash, "err", cause)
			return true
		}
		return !f.forgetSubmission(ctx, fwd.TxHash)

	default: // NOT_FOUND
		// Not seen yet. It can still land until its max time has passed, so wait
		// that out rather than risk a second transfer.
		until := fwd.UpdatedAt.Add(txValidity)
		if fwd.SubmittedUntil != nil {
			until = *fwd.SubmittedUntil
		}
		if !pastMaxTime(resp, until) {
			_ = f.store.RequeueForContention(ctx, fwd.TxHash,
				fmt.Sprintf("awaiting %s (valid until %s)", hash, until.UTC().Format(time.RFC3339)))
			return true
		}

		// "Not found" only proves it never landed if RPC still holds every ledger
		// it could have landed in. After an outage longer than RPC's retention a
		// transfer that did land also reads as not found, and rebuilding would pay
		// twice. Close it for a human to reconcile instead; the intent is left as
		// is, since the deposit may well have been credited.
		sent := until.Add(-txValidity)
		if resp.OldestLedgerCloseTime != 0 && time.Unix(resp.OldestLedgerCloseTime, 0).After(sent) {
			_ = f.store.PermanentlyFail(ctx, fwd.TxHash, fmt.Sprintf(
				"outcome of %s unknown: older than RPC history — reconcile on-chain before re-forwarding", hash))
			slog.Error("forwarder: in-flight transfer outside RPC history, needs manual reconciliation",
				"tx_hash", fwd.TxHash, "submitted_tx", hash, "sent", sent,
				"rpc_oldest", time.Unix(resp.OldestLedgerCloseTime, 0))
			return true
		}

		slog.Warn("forwarder: in-flight transfer expired without landing, rebuilding",
			"tx_hash", fwd.TxHash, "submitted_tx", hash)
		return !f.forgetSubmission(ctx, fwd.TxHash)
	}
}

// pastMaxTime reports whether a transaction valid until `until` can no longer be
// included. The network's own clock decides that — no ledger closing after the
// max time can include it — so it uses the latest ledger RPC has ingested,
// which also absorbs RPC ingestion lag. Only when RPC does not report one does
// it fall back to the local clock plus landingGrace.
func pastMaxTime(resp rpcprotocol.GetTransactionResponse, until time.Time) bool {
	if resp.LatestLedgerCloseTime != 0 {
		return time.Unix(resp.LatestLedgerCloseTime, 0).After(until)
	}
	return time.Now().After(until.Add(landingGrace))
}

// forgetSubmission clears a forward's in-flight transaction once it is known
// never to transfer funds. It reports whether that succeeded; if not, the hash
// stays recorded and the next resolution simply finds it expired again.
func (f *Forwarder) forgetSubmission(ctx context.Context, txHash string) bool {
	if err := f.store.ClearSubmission(ctx, txHash); err != nil {
		slog.Error("forwarder: clear submission", "tx_hash", txHash, "err", err)
		return false
	}
	return true
}

// ── Submit pipeline ───────────────────────────────────────────────────────────

// submit builds, simulates, signs, and submits one outbound Soroban payment.
// Errors are tagged permanent or transient so callers can route accordingly.
// parseAsset converts the watcher's compact asset identifier back into an asset
// the SDK can build operations with. The identifier is produced by
// watcher.assetID: "native" for XLM, "CODE:ISSUER" for everything else.
//
// Issued assets (USDC in particular) reach the pool whenever an on-ramp
// delivers something other than XLM, so refusing them here strands real funds:
// the forward fails permanently and the sweep cannot move them either, leaving
// the balance in the pool until someone signs for it by hand.
func parseAsset(asset string) (txnbuild.Asset, error) {
	if asset == "native" {
		return txnbuild.NativeAsset{}, nil
	}
	code, issuer, found := strings.Cut(asset, ":")
	if !found || code == "" || issuer == "" {
		return nil, fmt.Errorf("unparseable asset %q", asset)
	}
	return txnbuild.CreditAsset{Code: code, Issuer: issuer}, nil
}

// submit pays amount of asset from poolAddress to the C-address cAddress via
// the asset's SAC transfer. inboundHash identifies the forward row the transfer
// is recorded against before it is sent; see RecordSubmission.
func (f *Forwarder) submit(ctx context.Context, inboundHash, cAddress, poolAddress, amount, asset string) (string, error) {
	return f.transfer(ctx, inboundHash, poolAddress, func(src *txnbuild.SimpleAccount, validUntil time.Time) (prepared, error) {
		return f.prepareContractPayment(ctx, src, cAddress, amount, asset, validUntil)
	})
}

// prepared is a built, unsigned transaction and how to price it for a given
// inclusion fee.
type prepared struct {
	env    xdr.TransactionEnvelope
	setFee func(env *xdr.TransactionEnvelope, inclusionFee uint32)
}

// transfer runs one outbound payment out of poolAddress end to end: draw the
// pool's next sequence number, build the transaction with prepare, then sign,
// record, send and confirm it, bumping the fee on tx_insufficient_fee. Forwards
// and recovery sweeps share it, so both get the same guarantee: the transaction
// is recorded against inboundHash before it is sent, and an unknown outcome is
// returned as unconfirmed rather than retried.
func (f *Forwarder) transfer(
	ctx context.Context,
	inboundHash, poolAddress string,
	prepare func(src *txnbuild.SimpleAccount, validUntil time.Time) (prepared, error),
) (hash string, err error) {
	kp, err := f.keypairFor(poolAddress)
	if err != nil {
		return "", permanent(err)
	}

	// Sequence comes from the shared sequencer, not a per-attempt Horizon read.
	// Horizon reports the account as of the last closed ledger, so concurrent
	// forwards used to read one number, build one sequence, and lose all but one
	// to txBadSeq. Consecutive numbers let a burst settle together instead.
	//
	// Any failure below must resync: the number handed out here goes unused, and
	// everything queued behind the gap is invalid until the account is re-read.
	// Serialise the ordered window: everything from drawing the sequence to the
	// network accepting the transaction. Released before the confirmation poll.
	unlockSend := f.seq().lockSend(poolAddress)
	sendDone := false
	defer func() {
		if !sendDone {
			unlockSend()
		}
	}()

	seq, err := f.seq().next(poolAddress)
	if err != nil {
		return "", transient(err)
	}
	sourceAccount := &txnbuild.SimpleAccount{AccountID: poolAddress, Sequence: seq}
	defer func() {
		// Resync only when the send itself failed, leaving this number unused and
		// everything queued behind it stranded. A failure *after* the network
		// accepted the transaction is a confirmation problem: the sequence is
		// spent, and re-reading Horizon while the transaction is still settling
		// would report the older value and hand the number out a second time.
		if err != nil && !sendDone {
			f.seq().resync(poolAddress)
		}
	}()

	// Whole seconds, so the recorded deadline is exactly the max time on-chain.
	validUntil := time.Now().Add(txValidity).Truncate(time.Second)

	p, err := prepare(sourceAccount, validUntil)
	if err != nil {
		return "", err
	}

	// ── Submit with fee-bump retry ────────────────────────────────────────────
	// Only the inclusion fee changes between attempts (a Soroban transaction
	// keeps its simulated footprint). Detect tx_insufficient_fee and double it.
	inclusionFee := uint32(txnbuild.MinBaseFee)
	for feeAttempt := range maxFeeRetries + 1 {
		p.setFee(&p.env, inclusionFee)
		hash, err = f.signAndSend(ctx, inboundHash, kp, p.env, validUntil)
		if err == nil {
			// Accepted by the network, so this sequence number is spent and the
			// next forward may proceed. Confirmation is polled without the lock.
			unlockSend()
			sendDone = true
			hash, err = f.pollResult(ctx, hash)
			if err != nil && !isUnconfirmed(err) {
				// Settled as failed: this transaction moved nothing.
				f.forgetSubmission(ctx, inboundHash)
			}
			return hash, err
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

// prepareContractPayment builds the SAC transfer to a C-address and simulates
// it for its footprint and resource fee.
func (f *Forwarder) prepareContractPayment(
	ctx context.Context,
	src *txnbuild.SimpleAccount,
	cAddress, amount, asset string,
	validUntil time.Time,
) (prepared, error) {
	parsedAsset, err := parseAsset(asset)
	if err != nil {
		return prepared{}, permanent(err)
	}

	// Classic Payment only accepts G-addresses. C-addresses (Soroban contracts)
	// must be paid via the asset's SAC transfer function instead.
	op, err := txnbuild.NewPaymentToContract(txnbuild.PaymentToContractParams{
		NetworkPassphrase: f.config.NetworkPassphrase,
		Destination:       cAddress,
		Amount:            amount,
		Asset:             parsedAsset,
		SourceAccount:     src.AccountID,
	})
	if err != nil {
		return prepared{}, permanent(fmt.Errorf("build payment-to-contract: %w", err))
	}

	// Build with placeholder fee for simulation — Stellar RPC ignores the fee value
	// during simulation and returns the true minimum resource fee in the response.
	tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
		SourceAccount: src,
		// The sequencer already returned the exact number this transaction must
		// carry, so txnbuild must not advance it again.
		IncrementSequenceNum: false,
		Operations:           []txnbuild.Operation{&op},
		BaseFee:              txnbuild.MinBaseFee,
		Preconditions: txnbuild.Preconditions{
			TimeBounds: txnbuild.NewTimebounds(0, validUntil.Unix()),
		},
	})
	if err != nil {
		return prepared{}, permanent(fmt.Errorf("build tx for simulation: %w", err))
	}

	// ── Simulate via Stellar RPC ──────────────────────────────────────────────
	// Soroban transactions are rejected if the footprint or resource fee doesn't
	// match the network's actual usage. Simulation gives us the ground truth once.
	env := tx.ToXDR()
	envBytes, err := env.MarshalBinary()
	if err != nil {
		return prepared{}, permanent(fmt.Errorf("marshal tx for simulation: %w", err))
	}

	simResp, err := f.rpc.SimulateTransaction(ctx, rpcprotocol.SimulateTransactionRequest{
		Transaction: base64.StdEncoding.EncodeToString(envBytes),
	})
	if err != nil {
		return prepared{}, transient(fmt.Errorf("simulate tx: %w", err))
	}
	if simResp.Error != "" {
		// Simulation errors are permanent — the same transaction will always fail
		// simulation (e.g., contract not found, invalid arguments).
		return prepared{}, permanent(fmt.Errorf("simulate tx: %s", simResp.Error))
	}

	simDataBytes, err := base64.StdEncoding.DecodeString(simResp.TransactionDataXDR)
	if err != nil {
		return prepared{}, permanent(fmt.Errorf("decode simulation data: %w", err))
	}
	var sorobanData xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshal(simDataBytes, &sorobanData); err != nil {
		return prepared{}, permanent(fmt.Errorf("unmarshal soroban data: %w", err))
	}

	return prepared{
		env: env,
		// Apply the simulation footprint and total fee.
		// total = inclusion (per-op base fee) + resource (from simulation) + buffer.
		setFee: func(env *xdr.TransactionEnvelope, inclusionFee uint32) {
			env.V1.Tx.Ext = xdr.TransactionExt{V: 1, SorobanData: &sorobanData}
			env.V1.Tx.Fee = xdr.Uint32(inclusionFee + uint32(simResp.MinResourceFee) + 10_000)
		},
	}, nil
}

// signAndSend signs a fully priced envelope and submits it via Stellar RPC.
//
// submits via rpc.SendTransaction (not horizon.SubmitTransaction) so we receive
// Stellar-native status codes including TRY_AGAIN_LATER and ERROR result codes.
//
// retries on TRY_AGAIN_LATER up to maxTryAgain times with a one-ledger sleep.
//
// The signed transaction's hash is recorded against the forward before the first
// send, so a crash or a lost response after this point is resolved by looking
// that hash up rather than by paying again. Every return that means "never
// queued" clears it again; every return that means "may be queued" is
// unconfirmed and leaves it in place.
func (f *Forwarder) signAndSend(
	ctx context.Context,
	inboundHash string,
	kp *keypair.Full,
	env xdr.TransactionEnvelope,
	validUntil time.Time,
) (string, error) {
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

	txHash, err := tx.HashHex(f.config.NetworkPassphrase)
	if err != nil {
		return "", permanent(fmt.Errorf("hash tx: %w", err))
	}
	if err := f.store.RecordSubmission(ctx, inboundHash, txHash, validUntil); err != nil {
		// Nothing sent: without the record, a lost response could not be told
		// apart from a transfer that never happened.
		return "", transient(err)
	}

	// TRY_AGAIN_LATER retry loop — Stellar Core's mempool can be temporarily full.
	// The correct response is to wait one ledger cycle (~5s) and resubmit the same
	// signed envelope. We do not count these as failures.
	for attempt := range maxTryAgain + 1 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				// An earlier attempt got TRY_AGAIN_LATER, so nothing is queued.
				f.forgetSubmission(ctx, inboundHash)
				return "", transient(ctx.Err())
			case <-time.After(ledgerTime):
			}
		}

		resp, err := f.rpc.SendTransaction(ctx, rpcprotocol.SendTransactionRequest{
			Transaction: signedB64,
		})
		if err != nil {
			// The request may have reached the network before the error.
			return "", unconfirmed(fmt.Errorf("send transaction %s: %w", txHash, err))
		}

		switch resp.Status {
		case "PENDING":
			// Accepted by Stellar Core. Return without polling so the caller can
			// release the send lock first — confirmation takes far longer than
			// submission and holding the lock through it would serialise the
			// whole pipeline on ledger close.
			return resp.Hash, nil
		case "DUPLICATE":
			// Already submitted (e.g. a previous attempt that timed out on our side
			// but succeeded on the network). Poll for the existing result.
			return resp.Hash, nil
		case "TRY_AGAIN_LATER":
			slog.Warn("forwarder: TRY_AGAIN_LATER, sleeping one ledger",
				"attempt", attempt+1, "max", maxTryAgain)
			continue
		case "ERROR":
			// Rejected before entering the queue: it can never land.
			f.forgetSubmission(ctx, inboundHash)
			return "", classifyResultXDR(resp.ErrorResultXDR)
		default:
			return "", unconfirmed(fmt.Errorf("unknown send status %s for %s", resp.Status, txHash))
		}
	}

	// TRY_AGAIN_LATER means not queued, every time.
	f.forgetSubmission(ctx, inboundHash)
	return "", transient(fmt.Errorf("TRY_AGAIN_LATER after %d retries", maxTryAgain))
}

// pollResult waits for a PENDING transaction to reach SUCCESS or FAILED on-chain.
// Uses the SDK's built-in exponential-backoff poller (500ms → 3.5s intervals).
func (f *Forwarder) pollResult(ctx context.Context, hash string) (string, error) {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	result, err := f.rpc.PollTransaction(pollCtx, hash)
	if err != nil {
		// Accepted but not seen to settle; it may still land.
		return "", unconfirmed(fmt.Errorf("poll transaction %s: %w", hash, err))
	}

	switch result.Status {
	case rpcprotocol.TransactionStatusSuccess:
		return hash, nil
	case rpcprotocol.TransactionStatusFailed:
		// Classify the on-chain failure to decide whether retry makes sense.
		return "", classifyResultXDR(result.ResultXDR)
	default:
		return "", unconfirmed(fmt.Errorf("unexpected poll status %s for %s", result.Status, hash))
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

	// ── Contention — the account was busy, this transaction was fine ───────────
	// An account can only land one Soroban transaction per ledger, so under load
	// a perfectly good transaction loses the race. Retry it without charging the
	// budget.
	case xdr.TransactionResultCodeTxBadSeq:
		return contention(fmt.Errorf("transaction rejected with contention code %s", code.String()))

	// ── Transient — a fresh attempt may succeed ────────────────────────────────
	case xdr.TransactionResultCodeTxTooEarly,
		xdr.TransactionResultCodeTxTooLate, // time bounds expired; retry builds a fresh tx
		xdr.TransactionResultCodeTxInternalError:
		return transient(fmt.Errorf("transaction rejected with transient code %s", code.String()))

	// ── Permanent — do not retry ───────────────────────────────────────────────
	default:
		return permanent(fmt.Errorf("transaction rejected with permanent code %s", code.String()))
	}
}

// ── Sweep (classic payment to the G-address recovery account) ──────────────────

// errAwaitingTrustline means the recovery account cannot yet receive the swept
// asset. Nothing is sent — a payment would only fail on-chain and burn its fee —
// and the sweep is retried each tick, so adding the trustline is enough to let
// it through.
var errAwaitingTrustline = errors.New("recovery account has no authorized trustline for the asset")

// startSweep returns a deposit that cannot be credited to the recovery account.
// The decision is persisted before anything is sent, so a retry after a crash
// finishes the sweep instead of deciding again.
func (f *Forwarder) startSweep(ctx context.Context, txHash, poolAddress, amount, asset, reason string) {
	if err := f.store.MarkSweep(ctx, txHash, reason); err != nil {
		_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusPendingRetry, err.Error())
		slog.Error("forwarder: cannot record sweep decision, queued for retry", "tx_hash", txHash, "err", err)
		return
	}
	f.sweepDeposit(ctx, txHash, poolAddress, amount, asset)
}

// sweepDeposit attempts the recovery payment once and records the outcome only
// as far as it is known: swept once the payment has landed, and otherwise left
// queued or failed with the funds still in the pool — never claimed as swept.
func (f *Forwarder) sweepDeposit(ctx context.Context, txHash, poolAddress, amount, asset string) {
	hash, err := f.sweep(ctx, txHash, poolAddress, amount, asset)
	switch {
	case err == nil:
		_ = f.store.MarkSwept(ctx, txHash, hash)
		metrics.ForwardsTotal.WithLabelValues("swept").Inc()
		slog.Info("forwarder: swept to recovery", "inbound", txHash, "pool", poolAddress, "sweep_tx", hash)

	case errors.Is(err, errAwaitingTrustline), isUnconfirmed(err), isContention(err):
		// Waiting on configuration, on confirmation or for the pool's slot —
		// none of which is this sweep failing, so none is charged.
		_ = f.store.RequeueForContention(ctx, txHash, "not swept yet: "+err.Error())
		slog.Warn("forwarder: sweep deferred", "tx_hash", txHash, "err", err)

	case isPermanent(err):
		_ = f.store.PermanentlyFail(ctx, txHash, "not swept, funds remain in pool: "+err.Error())
		metrics.ForwardsTotal.WithLabelValues("sweep_failed").Inc()
		slog.Error("forwarder: sweep failed permanently, funds remain in pool",
			"tx_hash", txHash, "pool", poolAddress, "err", err)

	default:
		_ = f.store.MarkForwardFailed(ctx, txHash, store.StatusPendingRetry, "not swept yet: "+err.Error())
		slog.Warn("forwarder: sweep failed, queued for retry", "tx_hash", txHash, "err", err)
	}
}

// sweep pays an unroutable deposit to the recovery account out of poolAddress —
// the pool that actually received it. With intents spread across pools,
// sweeping from a fixed pool would refund one pool's deposit out of another
// pool's balance and strand the original funds.
//
// It goes through the same transfer pipeline as a forward (same sequencer and
// send lock, recorded before it is sent), so a lost response or a crash is
// resolved by looking the payment up rather than paying the recovery account
// twice.
func (f *Forwarder) sweep(ctx context.Context, inboundHash, poolAddress, amount, asset string) (string, error) {
	if f.config.RecoveryAddress == "" {
		return "", permanent(errors.New("RECOVERY_ADDRESS is not configured"))
	}
	parsedAsset, err := parseAsset(asset)
	if err != nil {
		return "", permanent(err)
	}
	if err := f.checkRecoveryTrustline(parsedAsset); err != nil {
		return "", err
	}

	// Tie the recovery payment to the deposit it returns, on-chain: whoever
	// reconciles the recovery account can find the original payment by hash.
	var memo txnbuild.Memo
	var h txnbuild.MemoHash
	if len(inboundHash) == 2*len(h) {
		if _, err := hex.Decode(h[:], []byte(inboundHash)); err == nil {
			memo = h
		}
	}

	return f.transfer(ctx, inboundHash, poolAddress, func(src *txnbuild.SimpleAccount, validUntil time.Time) (prepared, error) {
		tx, err := txnbuild.NewTransaction(txnbuild.TransactionParams{
			SourceAccount:        src,
			IncrementSequenceNum: false,
			Operations: []txnbuild.Operation{
				&txnbuild.Payment{
					Destination: f.config.RecoveryAddress,
					Amount:      amount,
					Asset:       parsedAsset,
				},
			},
			Memo:    memo,
			BaseFee: txnbuild.MinBaseFee,
			Preconditions: txnbuild.Preconditions{
				TimeBounds: txnbuild.NewTimebounds(0, validUntil.Unix()),
			},
		})
		if err != nil {
			return prepared{}, permanent(fmt.Errorf("build sweep tx: %w", err))
		}
		return prepared{
			env: tx.ToXDR(),
			// One classic operation: the fee is the inclusion fee.
			setFee: func(env *xdr.TransactionEnvelope, inclusionFee uint32) {
				env.V1.Tx.Fee = xdr.Uint32(inclusionFee)
			},
		}, nil
	})
}

// checkRecoveryTrustline confirms the recovery account can receive an issued
// asset before any payment is sent. Without an authorized trustline the payment
// is included and fails (op_no_trust / op_not_authorized), which costs a fee and
// proves nothing; checking first lets the sweep wait for the trustline instead.
func (f *Forwarder) checkRecoveryTrustline(asset txnbuild.Asset) error {
	if asset.IsNative() {
		return nil
	}
	acct, err := f.horizon.AccountDetail(horizonclient.AccountRequest{AccountID: f.config.RecoveryAddress})
	if err != nil {
		return transient(fmt.Errorf("load recovery account: %w", err))
	}
	for _, b := range acct.Balances {
		if b.Code == asset.GetCode() && b.Issuer == asset.GetIssuer() {
			if b.IsAuthorized != nil && !*b.IsAuthorized {
				break
			}
			return nil
		}
	}
	return fmt.Errorf("%w (%s:%s)", errAwaitingTrustline, asset.GetCode(), asset.GetIssuer())
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
