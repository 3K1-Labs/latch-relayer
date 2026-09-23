// Package metrics exposes Prometheus metrics on /metrics.
//
// Each service builds its own Metrics (a private registry): the HTTP metrics
// every binary shares live on it, and the deposit bridge additionally
// registers the deposit metrics below with RegisterDeposit.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics owns a private registry so tests can build independent instances.
type Metrics struct {
	Registry   *prometheus.Registry
	requests   *prometheus.CounterVec
	duration   *prometheus.HistogramVec
	rejections *prometheus.CounterVec
}

func New(namespace string) *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_requests_total",
			Help:      "HTTP requests by route pattern, method and status.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request latency by route pattern.",
			Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"route"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "http_rejections_total",
			Help:      "Requests shed before reaching a handler, by reason (inflight, rate, draining).",
		}, []string{"reason"}),
	}
	m.Registry.MustRegister(
		m.requests, m.duration, m.rejections,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
}

// Rejected counts a request shed by a limiter.
func (m *Metrics) Rejected(reason string) {
	m.rejections.WithLabelValues(reason).Inc()
}

// Middleware records request count and latency. It labels by the ServeMux
// pattern ("GET /deposit/status/{memo_id}"), never the raw path, so label
// cardinality stays bounded. It must sit outside the mux with no middleware
// in between that copies the request, or the pattern isn't visible here.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		m.requests.WithLabelValues(route, r.Method, strconv.Itoa(rec.status)).Inc()
		m.duration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ── Deposit bridge metrics ────────────────────────────────────────────────────
//
// This service moves customer money. These are chosen to answer the three
// questions that come up in an incident: is money reaching people, is it
// reaching them fast enough, and is the backlog growing.
//
// They are package-level so the forwarder and retry worker can record without
// plumbing a handle through; they only appear on /metrics once the deposit
// binary registers them with RegisterDeposit (the gasless binary doesn't).

var (
	// ForwardsTotal counts every completed forward attempt by outcome.
	//
	// The outcome worth alerting on is "unknown_memo": a deposit arrived
	// carrying a reference this relayer never issued, so it was swept to
	// recovery instead of credited — the production signature of an on-ramp
	// registration bug.
	ForwardsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "relayer_forwards_total",
			Help: "Forward attempts by outcome (done, unknown_memo, expired, permanent_failure).",
		},
		[]string{"outcome"},
	)

	// ContentionTotal counts attempts that lost the race for a pool account's
	// ledger slot. A sustained rate means the pools are saturated and the fix is
	// more pool accounts, not more concurrency — an account can only land one
	// Soroban transaction per ledger.
	ContentionTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "relayer_pool_contention_total",
			Help: "Forward attempts rejected because the pool account's ledger slot was taken.",
		},
	)

	// PendingRetryDepth is the size of the retry backlog, sampled each sweep.
	// Flat or falling is healthy; a rising line means deposits are arriving
	// faster than the pools can forward them.
	PendingRetryDepth = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "relayer_pending_retry_depth",
			Help: "Forwards waiting in the retry queue at the last sweep.",
		},
	)

	// DepositToCreditSeconds measures the span a customer actually feels: the
	// deposit landing in the pool through to the forward confirming on-chain.
	// Buckets stretch to twenty minutes because the tail is the interesting part.
	DepositToCreditSeconds = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "relayer_deposit_to_credit_seconds",
			Help:    "Seconds from inbound deposit recorded to outbound forward confirmed.",
			Buckets: []float64{5, 10, 20, 30, 60, 120, 300, 600, 1200},
		},
	)
)

// RegisterDeposit adds the deposit metrics to the service's registry. Call once.
func (m *Metrics) RegisterDeposit() {
	m.Registry.MustRegister(ForwardsTotal, ContentionTotal, PendingRetryDepth, DepositToCreditSeconds)
}
