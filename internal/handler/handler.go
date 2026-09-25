package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/memo"
	"github.com/latch/relayer/internal/store"
)

// Handler holds the dependencies every HTTP handler needs.
type Handler struct {
	store  *store.Store
	config *config.Config

	// nextPool round-robins intents across the configured pool accounts.
	//
	// Stellar Core admits at most one Soroban transaction per source account per
	// ledger, so a pool account can forward roughly one deposit every five
	// seconds no matter how much concurrency the relayer has internally.
	// Measured on testnet that is ~12 credits/minute for a single pool. Spreading
	// intents across pools is therefore the only thing that raises the ceiling:
	// throughput scales with the number of pools.
	//
	// Which pool a deposit lands on is fixed at intent creation, because the
	// address is what the depositor is told to pay.
	nextPool atomic.Uint64
}

func New(st *store.Store, cfg *config.Config) *Handler {
	return &Handler{store: st, config: cfg}
}

// pickPool returns the next pool account in rotation. Even distribution matters
// more than stickiness: each pool is an independent per-ledger slot, so an
// unbalanced spread wastes the slots on the idle ones.
func (h *Handler) pickPool() config.PoolAccount {
	n := h.nextPool.Add(1) - 1
	return h.config.PoolAccounts[n%uint64(len(h.config.PoolAccounts))]
}

// RegisterRoutes wires all routes onto mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /intents", h.CreateIntent)
	mux.HandleFunc("PATCH /intents/{memo_id}", h.SetExternalID)
	mux.HandleFunc("GET /deposit/status/{memo_id}", h.DepositStatus)
	mux.HandleFunc("GET /health", h.Health)
	// GET /metrics is registered in cmd/serve/main.go, from the service's
	// metrics registry (which the deposit metrics are registered on). It sits
	// behind the API key, unlike /health: the counters describe deposit volume
	// and failure rates. Prometheus sends the bearer token in its scrape config.
}

// ── Request / Response types ─────────────────────────────────────────────────

type createIntentRequest struct {
	CAddress    string `json:"c_address"`
	ExpectedAmt string `json:"expected_amt"` // optional, e.g. "5.0000000"
	ExternalID  string `json:"external_id"`  // optional, e.g. MoonPay transaction ID
	ExpiresIn   int    `json:"expires_in"`   // seconds until expiry; default 3600
}

type createIntentResponse struct {
	IntentID    string `json:"intent_id"`
	MemoID      string `json:"memo_id"`
	PoolAddress string `json:"pool_address"`
	ExpiresAt   string `json:"expires_at"`
}

type depositStatusResponse struct {
	IntentID    string `json:"intent_id"`
	MemoID      string `json:"memo_id"`
	CAddress    string `json:"c_address"`
	PoolAddress string `json:"pool_address"`
	Status      string `json:"status"`
	ExpiresAt   string `json:"expires_at"`
	// Reconciliation fields. Both are advisory and may be null: expected_amt is
	// whatever the caller quoted at mint time, external_id is attached later via
	// PATCH. Surfaced so support can compare them against the forwards below
	// without a database session.
	ExpectedAmt *string          `json:"expected_amt"`
	ExternalID  *string          `json:"external_id"`
	Forwards    []forwardSummary `json:"forwards"`
}

type setExternalIDRequest struct {
	ExternalID string `json:"external_id"`
}

type forwardSummary struct {
	TxHash    string  `json:"tx_hash"`
	Amount    string  `json:"amount"`
	Asset     string  `json:"asset"`
	Status    string  `json:"status"`
	ForwardTx *string `json:"forward_tx"`
	CreatedAt string  `json:"created_at"`
}

// ── Handlers ─────────────────────────────────────────────────────────────────

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := h.store.Ping(ctx); err != nil {
		slog.Error("health: db ping failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unhealthy",
			"db":     err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// CreateIntent creates a new funding intent: a unique memo_id tied to a C-address
// with a TTL. The caller (latch-api) passes this memo_id to the on-ramp as the
// wallet tag, then polls /deposit/status to track the forwarding.
//
//	POST /intents
//	{ "c_address": "C...", "expected_amt": "5.0000000", "expires_in": 3600 }
//	→ 201 { "intent_id": "uuid", "memo_id": "...", "pool_address": "GB3...", "expires_at": "..." }
func (h *Handler) CreateIntent(w http.ResponseWriter, r *http.Request) {
	var req createIntentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.CAddress == "" {
		writeError(w, http.StatusBadRequest, "c_address is required")
		return
	}
	if err := memo.ValidateCAddress(req.CAddress); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid c_address: %s", err))
		return
	}

	ttl := req.ExpiresIn
	if ttl <= 0 {
		// Default 1 hour — kept deliberately until real provider data says
		// otherwise. It may be too short for some senders: exchange withdrawals
		// can sit in review or compliance holds for hours, and on-ramps settle on
		// their own schedule. A deposit that lands after the intent expires is
		// swept to recovery instead of credited, which means a manual refund.
		// Revisit once we see real settlement times per integrated provider;
		// callers can already pass a longer expires_in per flow. Longer is safe:
		// memo_ids are random uint64s, so an open intent cannot be guessed.
		// Expiry is judged by when the deposit landed on-chain, not when the
		// relayer processes it (see forwarder.Forward).
		ttl = 3600
	}
	expiresAt := time.Now().Add(time.Duration(ttl) * time.Second)

	var expectedAmt *string
	if req.ExpectedAmt != "" {
		expectedAmt = &req.ExpectedAmt
	}
	var externalID *string
	if req.ExternalID != "" {
		externalID = &req.ExternalID
	}

	pool := h.pickPool()

	intent, err := h.store.CreateIntent(r.Context(), req.CAddress, pool.Address, expectedAmt, expiresAt, externalID)
	if err != nil {
		slog.Error("create intent: insert", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create intent")
		return
	}

	writeJSON(w, http.StatusCreated, createIntentResponse{
		IntentID:    intent.ID,
		MemoID:      strconv.FormatUint(intent.MemoID, 10),
		PoolAddress: pool.Address,
		ExpiresAt:   intent.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// DepositStatus returns the intent and all forwarding records for a given memo_id.
//
//	GET /deposit/status/{memo_id}
//	→ 200 { "intent_id": "...", "memo_id": "...", "c_address": "...", "status": "...", "forwards": [...] }
func (h *Handler) DepositStatus(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("memo_id")
	memoID, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "memo_id must be a valid integer")
		return
	}

	intent, err := h.store.GetIntentByMemoID(r.Context(), memoID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "memo_id not found")
		return
	}
	if err != nil {
		slog.Error("deposit status: get intent", "memo_id", memoID, "err", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	forwards, err := h.store.GetForwardByMemoID(r.Context(), memoID)
	if err != nil {
		slog.Error("deposit status: get forwards", "memo_id", memoID, "err", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	summaries := make([]forwardSummary, len(forwards))
	for i, f := range forwards {
		summaries[i] = forwardSummary{
			TxHash:    f.TxHash,
			Amount:    f.Amount,
			Asset:     f.Asset,
			Status:    f.Status,
			ForwardTx: f.ForwardTx,
			CreatedAt: f.CreatedAt.UTC().Format(time.RFC3339),
		}
	}

	writeJSON(w, http.StatusOK, depositStatusResponse{
		IntentID:    intent.ID,
		MemoID:      strconv.FormatUint(intent.MemoID, 10),
		CAddress:    intent.CAddress,
		PoolAddress: intent.PoolAddress,
		Status:      intent.Status,
		ExpiresAt:   intent.ExpiresAt.UTC().Format(time.RFC3339),
		ExpectedAmt: intent.ExpectedAmt,
		ExternalID:  intent.ExternalID,
		Forwards:    summaries,
	})
}

// SetExternalID binds an on-ramp provider's order ID to an existing intent.
//
// Split from CreateIntent because the ID does not exist yet at mint time: MoonPay
// issues one only after the user completes checkout. The webhook receiver calls
// this to close the loop between our memo and the provider's order.
//
//	PATCH /intents/{memo_id}
//	{ "external_id": "moonpay-tx-uuid" }
//	→ 200 { "memo_id": "...", "external_id": "..." }
//	→ 409 when the intent is already bound to a different order
func (h *Handler) SetExternalID(w http.ResponseWriter, r *http.Request) {
	memoID, err := strconv.ParseUint(r.PathValue("memo_id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "memo_id must be a valid integer")
		return
	}

	var req setExternalIDRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.ExternalID == "" {
		writeError(w, http.StatusBadRequest, "external_id is required")
		return
	}

	switch err := h.store.SetIntentExternalID(r.Context(), memoID, req.ExternalID); {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "memo_id not found")
		return
	case errors.Is(err, store.ErrExternalIDConflict):
		writeError(w, http.StatusConflict, "intent is already bound to a different external_id")
		return
	default:
		slog.Error("set external_id", "memo_id", memoID, "err", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"memo_id":     strconv.FormatUint(memoID, 10),
		"external_id": req.ExternalID,
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

type errResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errResponse{Error: message})
}
