package httpx

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// exempt reports paths that must answer even when the service is saturated:
// an orchestrator that sees /health fail under load restarts the instance,
// turning an overload into an outage.
func exempt(r *http.Request) bool {
	return r.URL.Path == "/health" || r.URL.Path == "/metrics"
}

// Draining is flipped on at shutdown so new work is refused while in-flight
// work finishes.
type Draining struct{ on atomic.Bool }

func (d *Draining) Set()         { d.on.Store(true) }
func (d *Draining) Active() bool { return d.on.Load() }

// LimitInflight caps concurrently executing requests. Past the cap it answers
// 503 immediately instead of queueing: a sponsored transaction's user
// signature expires within minutes, so a request that waits in line is worth
// less than a fast "retry later".
func LimitInflight(max int, draining *Draining, onReject func(reason string)) func(http.Handler) http.Handler {
	slots := make(chan struct{}, max)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			if draining != nil && draining.Active() {
				onReject("draining")
				reject(w, http.StatusServiceUnavailable, "shutting_down", 5*time.Second)
				return
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				next.ServeHTTP(w, r)
			default:
				onReject("inflight")
				reject(w, http.StatusServiceUnavailable, "over_capacity", time.Second)
			}
		})
	}
}

// RateLimiter is a per-key token bucket: each key refills at rps tokens per
// second up to burst.
type RateLimiter struct {
	rps   float64
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewRateLimiter(rps float64, burst int) *RateLimiter {
	return &RateLimiter{rps: rps, burst: float64(burst), now: time.Now, buckets: map[string]*bucket{}}
}

// Allow takes one token for key. When none is left it returns how long until
// the next token.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
		if len(l.buckets) > 10_000 {
			l.evictIdle(now)
		}
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rps)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rps * float64(time.Second))
	return false, wait
}

// evictIdle drops buckets that have refilled completely; they carry no state
// a fresh bucket wouldn't.
func (l *RateLimiter) evictIdle(now time.Time) {
	full := time.Duration(l.burst / l.rps * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) > full {
			delete(l.buckets, k)
		}
	}
}

// Middleware rate-limits by keyFn(r). Requests whose key is "" are not limited.
func (l *RateLimiter) Middleware(keyFn func(*http.Request) string, onReject func(reason string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := keyFn(r)
			if exempt(r) || key == "" {
				next.ServeHTTP(w, r)
				return
			}
			if ok, wait := l.Allow(key); !ok {
				onReject("rate")
				reject(w, http.StatusTooManyRequests, "rate_limited", wait)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func reject(w http.ResponseWriter, status int, code string, retryAfter time.Duration) {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
