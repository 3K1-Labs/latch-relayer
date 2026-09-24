// Package metrics defines the Prometheus metrics the relayer exposes at
// /metrics.
//
// This service moves customer money and, until now, was observable only through
// log lines. The metrics here are chosen to answer the three questions that
// actually come up in an incident: is money reaching people, is it reaching them
// fast enough, and is the backlog growing.
//
// Metrics are package-level and registered once via promauto against the default
// registry, matching latch-api's internal/metrics package.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// ForwardsTotal counts every completed forward attempt by outcome.
	//
	// The outcome worth alerting on is "unknown_memo": it means a deposit
	// arrived carrying a reference this relayer never issued, so it was swept to
	// recovery instead of credited. That is the exact production signature of
	// the on-ramp registration bug, and a non-zero rate here would have caught
	// it on the first day rather than during a load test.
	ForwardsTotal = promauto.NewCounterVec(
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
	ContentionTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "relayer_pool_contention_total",
			Help: "Forward attempts rejected because the pool account's ledger slot was taken.",
		},
	)

	// PendingRetryDepth is the size of the retry backlog, sampled each sweep.
	// Flat or falling is healthy; a rising line means deposits are arriving
	// faster than the pools can forward them.
	PendingRetryDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "relayer_pending_retry_depth",
			Help: "Forwards waiting in the retry queue at the last sweep.",
		},
	)

	// DepositToCreditSeconds measures the span a customer actually feels: the
	// deposit landing in the pool through to the forward confirming on-chain.
	//
	// Buckets stretch to twenty minutes because the tail is the interesting
	// part. Under contention the median stays healthy while the slowest deposits
	// drift into minutes, and an average would hide exactly that.
	DepositToCreditSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "relayer_deposit_to_credit_seconds",
			Help:    "Seconds from inbound deposit recorded to outbound forward confirmed.",
			Buckets: []float64{5, 10, 20, 30, 60, 120, 300, 600, 1200},
		},
	)
)
