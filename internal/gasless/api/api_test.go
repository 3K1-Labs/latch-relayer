package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/latch/relayer/internal/gasless/balance"
	"github.com/latch/relayer/internal/gasless/channels"
)

type pinger struct{ err error }

func (p pinger) Ping(context.Context) error { return p.err }

type snap balance.Snapshot

func (s snap) Snapshot() balance.Snapshot { return balance.Snapshot(s) }

func get(t *testing.T, a *API, path string) (int, map[string]any, string) {
	t.Helper()
	mux := http.NewServeMux()
	a.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Body.String()
}

func TestHealth(t *testing.T) {
	healthy := snap{SponsorshipOK: true, Channels: channels.Stats{Active: 3}}
	cases := map[string]struct {
		a          *API
		wantCode   int
		wantStatus string
	}{
		"ok":             {&API{DB: pinger{}, Status: healthy}, 200, "ok"},
		"funder low":     {&API{DB: pinger{}, Status: snap{Channels: channels.Stats{Active: 3}}}, 200, "degraded"},
		"no channels":    {&API{DB: pinger{}, Status: snap{SponsorshipOK: true}}, 200, "degraded"},
		"db unreachable": {&API{DB: pinger{errors.New("down")}, Status: healthy}, 503, "down"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			code, body, _ := get(t, tc.a, "/health")
			if code != tc.wantCode || body["status"] != tc.wantStatus {
				t.Fatalf("got %d %v, want %d %s", code, body["status"], tc.wantCode, tc.wantStatus)
			}
		})
	}
}

// /health is public, so it must not leak which accounts or balances we run.
func TestHealthRevealsNoAccounts(t *testing.T) {
	a := &API{DB: pinger{}, Status: snap{SponsorshipOK: true, FunderStroops: 123456789},
		Executor: "GEXECUTOR", Funder: "GFUNDER", FeeForwarderID: "CFORWARDER"}
	_, _, raw := get(t, a, "/health")
	for _, secretish := range []string{"GEXECUTOR", "GFUNDER", "CFORWARDER", "123456789"} {
		if strings.Contains(raw, secretish) {
			t.Fatalf("/health leaked %q: %s", secretish, raw)
		}
	}
	_, _, ops := get(t, a, "/gasless/status")
	if !strings.Contains(ops, "GEXECUTOR") || !strings.Contains(ops, "123456789") {
		t.Fatalf("/gasless/status missing operator detail: %s", ops)
	}
}
