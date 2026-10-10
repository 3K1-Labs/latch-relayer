package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/latch/relayer/internal/gasless/sponsor"
)

// Sponsor is satisfied by *sponsor.Submitter.
type Sponsor interface {
	Prepare(ctx context.Context, req sponsor.Request) (sponsor.Prepared, bool, error)
	Run(ctx context.Context, p sponsor.Prepared) sponsor.Record
}

// RecordReader is satisfied by *sponsor.Store.
type RecordReader interface {
	Get(ctx context.Context, requestID string) (sponsor.Record, bool, error)
}

// Background is satisfied by *lifecycle.Tracker: submissions keep running
// after the HTTP response and through a shutdown drain.
type Background interface {
	Go(f func(ctx context.Context))
}

// Quoter is satisfied by *sponsor.Submitter.
type Quoter interface {
	Quote(ctx context.Context, token string, resourceFee int64) (sponsor.Quote, error)
	FeeTokens() []sponsor.FeeToken
}

// Submissions serves POST /gasless/submit, GET /gasless/requests/{id}, and
// forward mode's POST /gasless/quote and GET /gasless/fee-tokens.
type Submissions struct {
	Sponsor  Sponsor
	Records  RecordReader
	Quoter   Quoter
	Work     Background
	SyncWait time.Duration
}

const maxSubmitBody = 128 << 10

func (s *Submissions) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /gasless/submit", s.Submit)
	mux.HandleFunc("GET /gasless/requests/{id}", s.Get)
	mux.HandleFunc("POST /gasless/quote", s.Quote)
	mux.HandleFunc("GET /gasless/fee-tokens", s.FeeTokens)
}

type quoteRequest struct {
	FeeToken           string `json:"fee_token"`
	ResourceFeeStroops int64  `json:"resource_fee_stroops"`
}

// Quote returns the max_fee_amount a user should sign for a forward() whose
// simulation reported resource_fee_stroops, plus the FeeForwarder and relayer
// addresses to build it with.
func (s *Submissions) Quote(w http.ResponseWriter, r *http.Request) {
	var req quoteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be {fee_token, resource_fee_stroops}: "+err.Error())
		return
	}
	q, err := s.Quoter.Quote(r.Context(), req.FeeToken, req.ResourceFeeStroops)
	if err != nil {
		status, code := prepareError(err)
		if status == http.StatusInternalServerError {
			slog.Error("gasless quote", "err", err)
			writeError(w, status, code, "internal error")
			return
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, q)
}

// FeeTokens lists the tokens users may pay fees in.
func (s *Submissions) FeeTokens(w http.ResponseWriter, _ *http.Request) {
	tokens := s.Quoter.FeeTokens()
	if tokens == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented", sponsor.ErrForwardNotBuilt.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fee_tokens": tokens})
}

// Submit validates, simulates and reserves a submission, then sends it in
// the background. It answers with the outcome if one arrives within
// SyncWait, else 202 with the pending record; the caller polls
// GET /gasless/requests/{id}. Repeating a request_id with the same body
// returns the same record and never pays twice.
func (s *Submissions) Submit(w http.ResponseWriter, r *http.Request) {
	var req sponsor.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSubmitBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "body must be a JSON submit request: "+err.Error())
		return
	}

	p, existing, err := s.Sponsor.Prepare(r.Context(), req)
	if err != nil {
		status, code := prepareError(err)
		if status == http.StatusInternalServerError {
			slog.Error("gasless submit: prepare", "request_id", req.RequestID, "err", err)
			writeError(w, status, code, "internal error")
			return
		}
		writeError(w, status, code, err.Error())
		return
	}
	if existing {
		writeRecord(w, p.Record)
		return
	}

	done := make(chan sponsor.Record, 1)
	s.Work.Go(func(ctx context.Context) { done <- s.Sponsor.Run(ctx, p) })

	select {
	case rec := <-done:
		writeRecord(w, rec)
	case <-time.After(s.SyncWait):
		writeRecord(w, p.Record) // still pending; the caller polls
	case <-r.Context().Done():
		// The caller left; the submission carries on and is stored.
	}
}

// Get returns a submission's stored outcome.
func (s *Submissions) Get(w http.ResponseWriter, r *http.Request) {
	rec, found, err := s.Records.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		slog.Error("gasless get request", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no submission with that request_id")
		return
	}
	writeRecord(w, rec)
}

// writeRecord answers 200 for a final outcome (success, failed, rejected:
// read status and error_code) and 202 while it may still change.
func writeRecord(w http.ResponseWriter, rec sponsor.Record) {
	status := http.StatusAccepted
	if rec.Status.Final() {
		status = http.StatusOK
	}
	writeJSON(w, status, rec)
}

// prepareError maps a refusal before anything was reserved or sent.
func prepareError(err error) (int, string) {
	switch {
	case errors.Is(err, sponsor.ErrInvalid):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, sponsor.ErrNotSponsorable):
		return http.StatusForbidden, "not_sponsorable"
	case errors.Is(err, sponsor.ErrForwardNotBuilt):
		return http.StatusNotImplemented, "not_implemented"
	case errors.Is(err, sponsor.ErrConflict):
		return http.StatusConflict, "request_id_conflict"
	case errors.Is(err, sponsor.ErrWalletCap):
		return http.StatusTooManyRequests, "wallet_cap_reached"
	case errors.Is(err, sponsor.ErrDailyBudget):
		return http.StatusTooManyRequests, "daily_budget_reached"
	case errors.Is(err, sponsor.ErrSimulation):
		return http.StatusUnprocessableEntity, "simulation_failed"
	case errors.Is(err, sponsor.ErrFeeTooLow):
		return http.StatusUnprocessableEntity, "max_fee_too_low"
	case errors.Is(err, sponsor.ErrPriceUnavailable):
		return http.StatusServiceUnavailable, "price_unavailable"
	case errors.Is(err, sponsor.ErrUnavailable):
		return http.StatusServiceUnavailable, "sponsorship_unavailable"
	default:
		return http.StatusInternalServerError, "internal"
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}
