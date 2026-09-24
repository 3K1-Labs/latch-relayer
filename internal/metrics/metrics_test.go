package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
