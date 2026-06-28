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
// Mirrors the ApiServer pattern from freighter-backend-v2: a struct that owns its
// services and registers its own routes, rather than scattered global functions.
type Handler struct {
	store  *store.Store
	config *config.Config
}

func New(st *store.Store, cfg *config.Config) *Handler {
	return &Handler{store: st, config: cfg}
}

// RegisterRoutes wires all routes onto mux.
// Go 1.22+ ServeMux supports "METHOD /path/{param}" patterns natively.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /register", h.Register)
	mux.HandleFunc("GET /deposit/status/{memo_id}", h.DepositStatus)
	mux.HandleFunc("GET /health", h.Health)
}

// ── Request / Response types ─────────────────────────────────────────────────

type registerRequest struct {
	CAddress string `json:"c_address"`
}

type registerResponse struct {
	MemoID      string `json:"memo_id"`
	PoolAddress string `json:"pool_address"`
}

type depositStatusResponse struct {
	MemoID   string          `json:"memo_id"`
	CAddress string          `json:"c_address"`
	Forwards []forwardSummary `json:"forwards"`
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

// Health is a simple liveness probe — returns 200 immediately.
// No DB check; if the process is up, it's healthy. Readiness (DB reachable)
// is checked at startup before the server opens.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Register links a Soroban C-address to a memo_id and pool account.
// Idempotent: calling it twice with the same C-address returns the same result.
//
//	POST /register
//	{ "c_address": "C..." }
//	→ 201 { "memo_id": "17540...", "pool_address": "GB3..." }
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
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

	memoID := memo.DeriveID(req.CAddress)

	// Idempotency check — return existing registration if present.
	// memo_id is deterministic so the response is identical every time.
	existing, err := h.store.GetRegistration(r.Context(), memoID)
	if err == nil {
		writeJSON(w, http.StatusOK, registerResponse{
			MemoID:      strconv.FormatUint(existing.MemoID, 10),
			PoolAddress: existing.PoolAddress,
		})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		slog.Error("register: get registration", "err", err)
		writeError(w, http.StatusInternalServerError, "database error")
		return
	}

	// Pick a pool account. Currently round-robin is just the first one.
	// When multiple pools exist the watcher will spread load automatically.
	pool := h.config.PoolAccounts[0]

	if err := h.store.RegisterAccount(r.Context(), memoID, req.CAddress, pool.Address); err != nil {
		slog.Error("register: insert registration", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to register account")
		return
	}

	writeJSON(w, http.StatusCreated, registerResponse{
		MemoID:      strconv.FormatUint(memoID, 10),
		PoolAddress: pool.Address,
	})
}

// DepositStatus returns all forwarding records for a given memo_id.
// The caller (latch-api) uses this to show the user whether their deposit landed.
//
//	GET /deposit/status/{memo_id}
//	→ 200 { "memo_id": "...", "c_address": "...", "forwards": [...] }
func (h *Handler) DepositStatus(w http.ResponseWriter, r *http.Request) {
	rawID := r.PathValue("memo_id")
	memoID, err := strconv.ParseUint(rawID, 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "memo_id must be a valid integer")
		return
	}

	reg, err := h.store.GetRegistration(r.Context(), memoID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "memo_id not registered")
		return
	}
	if err != nil {
		slog.Error("deposit status: get registration", "memo_id", memoID, "err", err)
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
		MemoID:   strconv.FormatUint(memoID, 10),
		CAddress: reg.CAddress,
		Forwards: summaries,
	})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

type errResponse struct {
	Error string `json:"error"`
}

// writeJSON sets Content-Type, writes the status code, and encodes v as JSON.
// Matches the httpresponse pattern from freighter-backend-v2.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errResponse{Error: message})
}
