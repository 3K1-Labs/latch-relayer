package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
}

func New(st *store.Store, cfg *config.Config) *Handler {
	return &Handler{store: st, config: cfg}
}

// RegisterRoutes wires all routes onto mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /intents", h.CreateIntent)
	mux.HandleFunc("GET /deposit/status/{memo_id}", h.DepositStatus)
	mux.HandleFunc("GET /health", h.Health)
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
	IntentID    string          `json:"intent_id"`
	MemoID      string          `json:"memo_id"`
	CAddress    string          `json:"c_address"`
	PoolAddress string          `json:"pool_address"`
	Status      string          `json:"status"`
	ExpiresAt   string          `json:"expires_at"`
	Forwards    []forwardSummary `json:"forwards"`
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
		ttl = 3600 // default 1 hour
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

	pool := h.config.PoolAccounts[0]

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
		Forwards:    summaries,
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
