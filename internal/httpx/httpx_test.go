package httpx

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestWithRequestID_KeepsValidCallerID(t *testing.T) {
	var seen string
	h := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/intents", nil)
	req.Header.Set(RequestIDHeader, "api-7f3a.1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "api-7f3a.1" || rec.Header().Get(RequestIDHeader) != "api-7f3a.1" {
		t.Fatalf("caller id not propagated: ctx=%q header=%q", seen, rec.Header().Get(RequestIDHeader))
	}
}

func TestWithRequestID_ReplacesUnsafeID(t *testing.T) {
	for _, bad := range []string{"", "has space", "new\nline", string(make([]byte, 65))} {
		var seen string
		h := WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = RequestID(r.Context())
		}))
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set(RequestIDHeader, bad)
		h.ServeHTTP(httptest.NewRecorder(), req)

		if seen == bad || len(seen) != 32 {
			t.Fatalf("id %q should be replaced with a generated one, got %q", bad, seen)
		}
	}
}

func TestLimitInflight_ShedsPastCap(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var rejected []string
	var mu sync.Mutex
	onReject := func(reason string) { mu.Lock(); rejected = append(rejected, reason); mu.Unlock() }

	h := LimitInflight(2, nil, onReject)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		entered <- struct{}{}
		<-release
	}))

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/intents", nil))
		}()
	}
	<-entered
	<-entered

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/intents", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("third request: status=%d retry-after=%q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}

	// /health must still answer while every slot is taken.
	health := httptest.NewRecorder()
	h.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("/health while saturated: status %d, want 200", health.Code)
	}

	close(release)
	wg.Wait()
	if len(rejected) != 1 || rejected[0] != "inflight" {
		t.Fatalf("rejections = %v, want [inflight]", rejected)
	}
}

func TestLimitInflight_Draining(t *testing.T) {
	d := &Draining{}
	d.Set()
	h := LimitInflight(10, d, func(string) {})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("handler must not run while draining")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/intents", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestRateLimiter_BurstThenRefill(t *testing.T) {
	now := time.Unix(0, 0)
	l := NewRateLimiter(2, 3) // 2 tokens/s, burst 3
	l.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d within burst was rejected", i+1)
		}
	}
	ok, wait := l.Allow("k")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("4th request: ok=%v wait=%v, want rejected with 500ms wait", ok, wait)
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("keys must have independent buckets")
	}

	now = now.Add(500 * time.Millisecond)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("token should have refilled after 500ms")
	}
}

func TestRateLimiter_Middleware429(t *testing.T) {
	l := NewRateLimiter(1, 1)
	var reasons []string
	h := l.Middleware(func(r *http.Request) string { return r.Header.Get("Authorization") },
		func(reason string) { reasons = append(reasons, reason) },
	)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/intents", nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(); code != http.StatusOK {
		t.Fatalf("first request %d", code)
	}
	if code := send(); code != http.StatusTooManyRequests {
		t.Fatalf("second request %d, want 429", code)
	}
	if len(reasons) != 1 || reasons[0] != "rate" {
		t.Fatalf("reasons = %v", reasons)
	}
}
