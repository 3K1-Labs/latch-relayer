package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMiddlewareLabelsByPatternNotPath(t *testing.T) {
	m := New("test")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /deposit/status/{memo_id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	h := m.Middleware(mux)

	for _, id := range []string{"1", "2", "3"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/deposit/status/"+id, nil))
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))
	m.Rejected("inflight")

	body := scrape(t, m)
	for _, want := range []string{
		`test_http_requests_total{method="GET",route="GET /deposit/status/{memo_id}",status="404"} 3`,
		`test_http_requests_total{method="GET",route="unmatched",status="404"} 1`,
		`test_http_rejections_total{reason="inflight"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	if strings.Contains(body, "/deposit/status/1") {
		t.Error("raw path leaked into labels")
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

// The pool metrics report whatever the pool reports at scrape time, so a
// burst's connection waits are visible on /metrics as they happen.
func TestRegisterDBPoolReadsPoolOnScrape(t *testing.T) {
	m := New("test")
	stats := DBPoolStats{MaxConns: 20, AcquiredConns: 7, WaitedAcquires: 1007, WaitTime: 1921 * time.Millisecond}
	m.RegisterDBPool(func() DBPoolStats { return stats })

	scrape := func() string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body, _ := io.ReadAll(rec.Body)
		return string(body)
	}
	body := scrape()
	for _, want := range []string{
		"relayer_db_pool_max_conns 20",
		"relayer_db_pool_acquired_conns 7",
		"relayer_db_pool_waited_acquires_total 1007",
		"relayer_db_pool_wait_seconds_total 1.921",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape missing %q", want)
		}
	}

	stats.WaitedAcquires = 1010
	if body = scrape(); !strings.Contains(body, "relayer_db_pool_waited_acquires_total 1010") {
		t.Error("second scrape did not re-read the pool")
	}
}
