package sponsor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/stellar/go-stellar-sdk/keypair"
	protocol "github.com/stellar/go-stellar-sdk/protocols/rpc"
	"github.com/stellar/go-stellar-sdk/txnbuild"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/latch/relayer/internal/feebump"
	"github.com/latch/relayer/internal/gasless/chain"
	"github.com/latch/relayer/internal/gasless/channels"
	"github.com/latch/relayer/internal/signer"
)

const (
	// leaseTTL covers build, the TRY_AGAIN_LATER loop, fee retries and the
	// confirmation poll. The channel is held until its transaction settles:
	// Stellar Core queues one transaction per source account.
	leaseTTL = 3 * time.Minute
	// leaseWait is how long a submission waits for a free channel. The
	// user's signatures expire, so a busy pool answers "retry", not a queue.
	leaseWait = 10 * time.Second
	// txValidity bounds each transaction's time bounds, and so how long an
	// unconfirmed one can still land.
	txValidity = 2 * time.Minute
	// pollTimeout is how long a PENDING transaction is followed before it is
	// left to the resolver.
	pollTimeout = 60 * time.Second

	maxTryAgain     = 5 // TRY_AGAIN_LATER resends, one ledger apart
	maxFeeRetries   = 2 // tx_insufficient_fee doublings
	maxLeaseRetries = 3 // fresh leases after txBadSeq
)

// ledgerTime is roughly one ledger: the wait before resending after
// TRY_AGAIN_LATER. A variable so tests can shorten it.
var ledgerTime = 5 * time.Second

// RPC is the subset of rpcclient.Client the submitter calls.
type RPC interface {
	chain.RPC
	GetFeeStats(ctx context.Context) (protocol.GetFeeStatsResponse, error)
	SendTransaction(ctx context.Context, req protocol.SendTransactionRequest) (protocol.SendTransactionResponse, error)
	GetTransaction(ctx context.Context, req protocol.GetTransactionRequest) (protocol.GetTransactionResponse, error)
	PollTransaction(ctx context.Context, hash string) (protocol.GetTransactionResponse, error)
}

// Leaser is satisfied by *channels.Pool.
type Leaser interface {
	AcquireWait(ctx context.Context, ttl, wait time.Duration) (*channels.Lease, error)
	Release(ctx context.Context, l *channels.Lease, seq *int64, resync bool) error
}

// Records is satisfied by *Store.
type Records interface {
	Get(ctx context.Context, requestID string) (Record, bool, error)
	Reserve(ctx context.Context, res Reservation, lim Limits) (Record, bool, error)
	Finish(ctx context.Context, requestID string, o Outcome) (Record, error)
	SetHash(ctx context.Context, requestID, hash string) error
	Stale(ctx context.Context, olderThan time.Duration) ([]StaleRecord, error)
}

// Submitter pays for and submits sponsored transactions.
type Submitter struct {
	RPC         RPC
	Channels    Leaser
	Records     Records
	ChannelKeys map[string]*keypair.Full // by address
	Funder      signer.Signer
	Passphrase  string
	Policy      Policy
	Limits      Limits
	// MaxInclusionFee caps the per-operation inclusion fee bid, in stroops,
	// including fee-retry doublings.
	MaxInclusionFee int64
	// Available reports whether the funder can sponsor right now
	// (balance.Monitor.SponsorshipOK).
	Available func() bool
	// RelayerAccounts are this service's own addresses: no auth entry may
	// claim to act for them.
	RelayerAccounts map[string]bool
}

// Prepare-time errors. Validation errors (ErrInvalid, ErrNotSponsorable,
// ErrForwardNotBuilt) and store errors (ErrConflict, ErrWalletCap,
// ErrDailyBudget) also come back from Prepare.
var (
	ErrUnavailable = errors.New("sponsorship is unavailable: funder below its floor or not yet checked")
	ErrSimulation  = errors.New("simulation failed")
)

// Prepared is a validated, simulated, reserved submission ready to Run.
type Prepared struct {
	Record      Record
	call        Call
	sorobanData xdr.SorobanTransactionData
	inclusion   int64
}

// Prepare validates req, simulates it with authorization enforced, and
// reserves its maximum cost against the caps. It sends nothing. When the
// request_id already exists with the same body, it returns that record and
// existing=true; the caller must not Run it.
func (s *Submitter) Prepare(ctx context.Context, req Request) (p Prepared, existing bool, err error) {
	call, err := Validate(req, s.Policy, s.RelayerAccounts)
	if err != nil {
		return Prepared{}, false, err
	}
	hash, err := payloadHash(req)
	if err != nil {
		return Prepared{}, false, err
	}

	// A repeat of a known request: answer from the record without simulating.
	if rec, found, err := s.Records.Get(ctx, req.RequestID); err != nil {
		return Prepared{}, false, err
	} else if found {
		if rec.payloadHash != hash {
			return Prepared{}, false, ErrConflict
		}
		return Prepared{Record: rec}, true, nil
	}

	if s.Available != nil && !s.Available() {
		return Prepared{}, false, ErrUnavailable
	}

	sd, err := s.simulate(ctx, call)
	if err != nil {
		return Prepared{}, false, err
	}
	inclusion := s.inclusionFee(ctx)

	rec, existing, err := s.Records.Reserve(ctx, Reservation{
		RequestID:     req.RequestID,
		PayloadHash:   hash,
		Wallet:        req.Wallet,
		Mode:          req.Mode,
		Contract:      call.Contract,
		Function:      call.Function,
		MaxFeeStroops: s.maxBid(int64(sd.ResourceFee)),
	}, s.Limits)
	if err != nil {
		return Prepared{}, false, err
	}
	return Prepared{Record: rec, call: call, sorobanData: sd, inclusion: inclusion}, existing, nil
}

// payloadHash identifies a request body, so a reused request_id with a
// different transaction is caught.
func payloadHash(req Request) (string, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("hash request: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// simulate runs the call with the user's signed auth entries in enforce
// mode: an invalid or expired signature fails here, before the funder pays.
// The funder is the simulation's source; the footprint and resources don't
// depend on the source because source-account credentials are refused.
func (s *Submitter) simulate(ctx context.Context, call Call) (xdr.SorobanTransactionData, error) {
	env, err := s.envelope(s.Funder.Address(), 0, call, nil, txnbuild.MinBaseFee, time.Now().Add(txValidity))
	if err != nil {
		return xdr.SorobanTransactionData{}, err
	}
	b64, err := xdr.MarshalBase64(env)
	if err != nil {
		return xdr.SorobanTransactionData{}, fmt.Errorf("encode simulation: %w", err)
	}
	resp, err := s.RPC.SimulateTransaction(ctx, protocol.SimulateTransactionRequest{
		Transaction: b64,
		AuthMode:    protocol.AuthModeEnforce,
	})
	if err != nil {
		return xdr.SorobanTransactionData{}, fmt.Errorf("simulate: %w", err)
	}
	if resp.Error != "" {
		return xdr.SorobanTransactionData{}, fmt.Errorf("%w: %s", ErrSimulation, resp.Error)
	}
	if resp.RestorePreamble != nil {
		return xdr.SorobanTransactionData{}, fmt.Errorf("%w: contract state is archived and must be restored first", ErrSimulation)
	}
	var sd xdr.SorobanTransactionData
	if err := xdr.SafeUnmarshalBase64(resp.TransactionDataXDR, &sd); err != nil {
		return xdr.SorobanTransactionData{}, fmt.Errorf("%w: decode transaction data: %v", ErrSimulation, err)
	}
	return sd, nil
}

// inclusionFee bids the network's recent p90 Soroban inclusion fee, at least
// the protocol minimum and at most MaxInclusionFee. If fee stats are
// unavailable it bids the minimum; a tx_insufficient_fee rejection doubles it.
func (s *Submitter) inclusionFee(ctx context.Context) int64 {
	fee := int64(txnbuild.MinBaseFee)
	if stats, err := s.RPC.GetFeeStats(ctx); err != nil {
		slog.Warn("sponsor: fee stats unavailable, bidding the minimum", "err", err)
	} else {
		fee = max(fee, int64(stats.SorobanInclusionFee.P90))
	}
	return min(fee, s.MaxInclusionFee)
}

// maxBid is the most a submission can cost: the fee-bump's bid at the
// highest inclusion fee the retries can reach.
func (s *Submitter) maxBid(resourceFee int64) int64 {
	// A one-operation inner transaction fee-bumped at its own rate bids the
	// inclusion fee twice (ops+1) plus the resource fee once.
	return 2*s.MaxInclusionFee + resourceFee
}

// envelope builds the inner transaction. sd may be nil (simulation).
func (s *Submitter) envelope(source string, seq int64, call Call, sd *xdr.SorobanTransactionData, inclusion int64, validUntil time.Time) (xdr.TransactionEnvelope, error) {
	var src xdr.MuxedAccount
	if err := src.SetAddress(source); err != nil {
		return xdr.TransactionEnvelope{}, fmt.Errorf("source account: %w", err)
	}
	fee := inclusion
	ext := xdr.TransactionExt{V: 0}
	if sd != nil {
		fee += int64(sd.ResourceFee)
		ext = xdr.TransactionExt{V: 1, SorobanData: sd}
	}
	if fee > int64(^uint32(0)) {
		return xdr.TransactionEnvelope{}, fmt.Errorf("fee %d overflows a transaction fee", fee)
	}
	return xdr.TransactionEnvelope{
		Type: xdr.EnvelopeTypeEnvelopeTypeTx,
		V1: &xdr.TransactionV1Envelope{Tx: xdr.Transaction{
			SourceAccount: src,
			Fee:           xdr.Uint32(fee),
			SeqNum:        xdr.SequenceNumber(seq),
			Cond: xdr.Preconditions{
				Type:       xdr.PreconditionTypePrecondTime,
				TimeBounds: &xdr.TimeBounds{MinTime: 0, MaxTime: xdr.TimePoint(validUntil.Unix())},
			},
			Operations: []xdr.Operation{{Body: xdr.OperationBody{
				Type: xdr.OperationTypeInvokeHostFunction,
				InvokeHostFunctionOp: &xdr.InvokeHostFunctionOp{
					HostFunction: call.HostFunction,
					Auth:         call.Auth,
				},
			}}},
			Ext: ext,
		}},
	}, nil
}

// Run submits a prepared request and records how it ended. It never returns
// an error: every outcome, including failure, is a stored status.
func (s *Submitter) Run(ctx context.Context, p Prepared) Record {
	id := p.Record.RequestID
	inclusion := p.inclusion
	for range maxLeaseRetries {
		o, retry, nextInclusion := s.attempt(ctx, p, inclusion)
		inclusion = nextInclusion
		if !retry {
			return s.finish(ctx, id, o)
		}
		slog.Warn("sponsor: channel sequence was stale, retrying on a fresh lease", "request_id", id)
	}
	return s.finish(ctx, id, Outcome{
		Status: StatusRejected, ErrorCode: "sequence_contention",
		ErrorMessage: fmt.Sprintf("channel sequence was stale %d times; nothing was charged", maxLeaseRetries),
	})
}

func (s *Submitter) finish(ctx context.Context, id string, o Outcome) Record {
	// Not the request context: the outcome must be stored even if the caller
	// has gone, or the cap would hold a reservation forever.
	rec, err := s.Records.Finish(context.WithoutCancel(ctx), id, o)
	if err != nil {
		slog.Error("sponsor: record outcome", "request_id", id, "status", o.Status, "err", err)
		return Record{RequestID: id, Status: o.Status, TxHash: o.TxHash,
			FeeChargedStroops: o.FeeChargedStroops, ErrorCode: o.ErrorCode, ErrorMessage: o.ErrorMessage}
	}
	return rec
}

// attempt runs one lease: build, sign, fee-bump, send, poll. retry=true means
// the channel's sequence was stale and nothing was sent that can land, so a
// fresh lease may succeed.
func (s *Submitter) attempt(ctx context.Context, p Prepared, inclusion int64) (o Outcome, retry bool, nextInclusion int64) {
	nextInclusion = inclusion
	lease, err := s.Channels.AcquireWait(ctx, leaseTTL, leaseWait)
	if errors.Is(err, channels.ErrPoolCapacity) {
		return Outcome{Status: StatusRejected, ErrorCode: "channels_busy", ErrorMessage: "every channel is busy; retry shortly"}, false, inclusion
	}
	if err != nil {
		return Outcome{Status: StatusRejected, ErrorCode: "lease_failed", ErrorMessage: err.Error()}, false, inclusion
	}

	released := false
	release := func(consumed *int64, resync bool) {
		if released {
			return
		}
		released = true
		if err := s.Channels.Release(context.WithoutCancel(ctx), lease, consumed, resync); err != nil {
			slog.Warn("sponsor: release channel", "channel", lease.Address, "err", err)
		}
	}
	defer release(nil, true)

	key := s.ChannelKeys[lease.Address]
	if key == nil {
		return Outcome{Status: StatusRejected, ErrorCode: "lease_failed", ErrorMessage: "no key for leased channel " + lease.Address}, false, inclusion
	}

	last := lease.Seq
	if lease.NeedsResync {
		accts, err := chain.Accounts(ctx, s.RPC, []string{lease.Address})
		if err != nil || !accts[lease.Address].Exists {
			return Outcome{Status: StatusRejected, ErrorCode: "lease_failed", ErrorMessage: fmt.Sprintf("load channel %s: %v", lease.Address, err)}, false, inclusion
		}
		last = accts[lease.Address].Seq
	}
	seq := last + 1
	validUntil := time.Now().Add(txValidity).Truncate(time.Second)

	for feeAttempt := 0; ; feeAttempt++ {
		b64, hash, err := s.seal(ctx, key, lease.Address, seq, p, inclusion, validUntil)
		if err != nil {
			release(&last, false)
			return Outcome{Status: StatusRejected, ErrorCode: "build_failed", ErrorMessage: err.Error()}, false, inclusion
		}
		if err := s.Records.SetHash(ctx, p.Record.RequestID, hash); err != nil {
			release(&last, false)
			return Outcome{Status: StatusRejected, ErrorCode: "store_failed", ErrorMessage: err.Error()}, false, inclusion
		}

		res := s.send(ctx, b64, hash)
		switch res.kind {
		case sentQueued:
			// Hold the channel until the transaction settles.
			o := s.poll(ctx, hash)
			if o.Status == StatusUnconfirmed {
				release(nil, true)
			} else {
				release(&seq, false) // landed: the sequence number is consumed
			}
			return o, false, inclusion
		case sentUnknown:
			release(nil, true)
			return Outcome{Status: StatusUnconfirmed, TxHash: hash, ErrorCode: "send_unconfirmed", ErrorMessage: res.msg}, false, inclusion
		case rejectedBadSeq:
			release(nil, true)
			return Outcome{}, true, inclusion
		case rejectedFee:
			if feeAttempt < maxFeeRetries && inclusion*2 <= s.MaxInclusionFee {
				inclusion *= 2
				nextInclusion = inclusion
				slog.Warn("sponsor: tx_insufficient_fee, raising the inclusion fee", "request_id", p.Record.RequestID, "inclusion_fee", inclusion)
				continue
			}
			// Out of fee headroom. The sequence may be queued at a lower fee.
			release(nil, true)
			return Outcome{Status: StatusRejected, ErrorCode: "insufficient_fee", ErrorMessage: "network fee exceeds the configured maximum"}, false, inclusion
		case rejectedBusy:
			release(&last, false)
			return Outcome{Status: StatusRejected, ErrorCode: "network_busy", ErrorMessage: res.msg}, false, inclusion
		default: // rejectedOther
			release(&last, false)
			return Outcome{Status: StatusRejected, ErrorCode: res.code, ErrorMessage: res.msg}, false, inclusion
		}
	}
}

// seal builds the inner transaction on the channel, signs it with the
// channel, wraps it in a fee-bump from the funder, and signs that. The
// fee-bump's hash is the one sent and looked up.
func (s *Submitter) seal(ctx context.Context, channel *keypair.Full, channelAddr string, seq int64, p Prepared, inclusion int64, validUntil time.Time) (b64, hash string, err error) {
	sd := p.sorobanData
	env, err := s.envelope(channelAddr, seq, p.call, &sd, inclusion, validUntil)
	if err != nil {
		return "", "", err
	}
	raw, err := xdr.MarshalBase64(env)
	if err != nil {
		return "", "", fmt.Errorf("encode inner: %w", err)
	}
	generic, err := txnbuild.TransactionFromXDR(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse inner: %w", err)
	}
	inner, ok := generic.Transaction()
	if !ok {
		return "", "", errors.New("inner envelope is not a transaction")
	}
	if inner, err = inner.Sign(s.Passphrase, channel); err != nil {
		return "", "", fmt.Errorf("sign inner: %w", err)
	}
	outer, err := feebump.AtInnerRate(inner.ToXDR(), s.Funder.Address())
	if err != nil {
		return "", "", err
	}
	if outer, err = signer.SignFeeBump(ctx, s.Funder, outer, s.Passphrase); err != nil {
		return "", "", fmt.Errorf("sign fee-bump: %w", err)
	}
	if b64, err = outer.Base64(); err != nil {
		return "", "", fmt.Errorf("encode fee-bump: %w", err)
	}
	if hash, err = outer.HashHex(s.Passphrase); err != nil {
		return "", "", fmt.Errorf("hash fee-bump: %w", err)
	}
	return b64, hash, nil
}

type sendKind int

const (
	sentQueued     sendKind = iota // PENDING or DUPLICATE
	sentUnknown                    // transport error: it may have reached the network
	rejectedBadSeq                 // the channel's sequence was stale
	rejectedFee                    // tx_insufficient_fee
	rejectedBusy                   // TRY_AGAIN_LATER every time
	rejectedOther                  // any other ERROR: it can never land
)

type sendResult struct {
	kind sendKind
	code string
	msg  string
}

// send submits the signed envelope, resending the same bytes one ledger
// later on TRY_AGAIN_LATER.
func (s *Submitter) send(ctx context.Context, b64, hash string) sendResult {
	for attempt := range maxTryAgain + 1 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return sendResult{kind: rejectedBusy, msg: "cancelled while the network was busy"}
			case <-time.After(ledgerTime):
			}
		}
		resp, err := s.RPC.SendTransaction(ctx, protocol.SendTransactionRequest{Transaction: b64})
		if err != nil {
			return sendResult{kind: sentUnknown, msg: fmt.Sprintf("send %s: %v", hash, err)}
		}
		switch resp.Status {
		case "PENDING", "DUPLICATE":
			return sendResult{kind: sentQueued}
		case "TRY_AGAIN_LATER":
			continue
		case "ERROR":
			return classifyRejection(resp.ErrorResultXDR)
		default:
			return sendResult{kind: sentUnknown, msg: fmt.Sprintf("unknown send status %s for %s", resp.Status, hash)}
		}
	}
	return sendResult{kind: rejectedBusy, msg: fmt.Sprintf("TRY_AGAIN_LATER after %d resends", maxTryAgain)}
}

// classifyRejection reads an ERROR result. For a fee-bump, the inner
// transaction's code is what failed.
func classifyRejection(resultXDR string) sendResult {
	code, ok := resultCode(resultXDR)
	if !ok {
		return sendResult{kind: rejectedOther, code: "rejected", msg: "transaction rejected without a readable result"}
	}
	switch code {
	case xdr.TransactionResultCodeTxBadSeq:
		return sendResult{kind: rejectedBadSeq}
	case xdr.TransactionResultCodeTxInsufficientFee:
		return sendResult{kind: rejectedFee}
	default:
		return sendResult{kind: rejectedOther, code: code.String(), msg: "transaction rejected: " + code.String()}
	}
}

// resultCode decodes a TransactionResult and returns the inner code for a
// fee-bump.
func resultCode(resultXDR string) (xdr.TransactionResultCode, bool) {
	var r xdr.TransactionResult
	if resultXDR == "" || xdr.SafeUnmarshalBase64(resultXDR, &r) != nil {
		return 0, false
	}
	code := r.Result.Code
	if (code == xdr.TransactionResultCodeTxFeeBumpInnerFailed || code == xdr.TransactionResultCodeTxFeeBumpInnerSuccess) &&
		r.Result.InnerResultPair != nil {
		code = r.Result.InnerResultPair.Result.Result.Code
	}
	return code, true
}

// feeCharged is what the network took for a landed transaction.
func feeCharged(resultXDR string) *int64 {
	var r xdr.TransactionResult
	if resultXDR == "" || xdr.SafeUnmarshalBase64(resultXDR, &r) != nil {
		return nil
	}
	fee := int64(r.FeeCharged)
	return &fee
}

// poll follows a queued transaction to SUCCESS or FAILED.
func (s *Submitter) poll(ctx context.Context, hash string) Outcome {
	pollCtx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	resp, err := s.RPC.PollTransaction(pollCtx, hash)
	if err != nil {
		return Outcome{Status: StatusUnconfirmed, TxHash: hash, ErrorCode: "poll_unconfirmed", ErrorMessage: err.Error()}
	}
	return landed(hash, resp)
}

// landed maps a getTransaction response to an outcome.
func landed(hash string, resp protocol.GetTransactionResponse) Outcome {
	switch resp.Status {
	case protocol.TransactionStatusSuccess:
		return Outcome{Status: StatusSuccess, TxHash: hash, FeeChargedStroops: feeCharged(resp.ResultXDR)}
	case protocol.TransactionStatusFailed:
		o := Outcome{Status: StatusFailed, TxHash: hash, FeeChargedStroops: feeCharged(resp.ResultXDR), ErrorCode: "tx_failed"}
		if code, ok := resultCode(resp.ResultXDR); ok {
			o.ErrorCode = code.String()
		}
		o.ErrorMessage = "transaction failed on-chain: " + o.ErrorCode
		return o
	default:
		return Outcome{Status: StatusUnconfirmed, TxHash: hash, ErrorCode: "poll_unconfirmed",
			ErrorMessage: fmt.Sprintf("status %s", resp.Status)}
	}
}
