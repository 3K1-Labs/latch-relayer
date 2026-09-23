// Package api is the gasless service's HTTP surface. P1 serves health and
// operational status; quote/submit/status endpoints arrive in later phases.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/latch/relayer/internal/gasless/balance"
)

// Pinger is satisfied by *pgxpool.Pool.
type Pinger interface {
	Ping(ctx context.Context) error
}

// StatusSource is satisfied by *balance.Monitor.
type StatusSource interface {
	Snapshot() balance.Snapshot
}

type API struct {
	DB             Pinger
	Status         StatusSource
	FeeForwarderID string
	Executor       string
	Funder         string
}

func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", a.Health)
	mux.HandleFunc("GET /gasless/status", a.OpsStatus)
}

// Health is unauthenticated (orchestrator probe), so it reveals only
// up/degraded. 503 only when the database is unreachable: a low funder is
// "degraded", not "restart me" — restarting wouldn't top it up.
func (a *API) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.DB.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "down", "db": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status(a.Status.Snapshot()), "db": "ok"})
}

// OpsStatus (authenticated) is the operator's view: balances, channel counts,
// and which accounts the service is running as.
func (a *API) OpsStatus(w http.ResponseWriter, r *http.Request) {
	snap := a.Status.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         status(snap),
		"fee_forwarder":  a.FeeForwarderID,
		"executor":       a.Executor,
		"funder":         a.Funder,
		"balances_as_of": snap,
	})
}

// status is "degraded" when the service can't sponsor right now: funder under
// its floor, no balance check yet, or no channel in rotation.
func status(s balance.Snapshot) string {
	if !s.SponsorshipOK || s.Channels.Active == 0 {
		return "degraded"
	}
	return "ok"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
