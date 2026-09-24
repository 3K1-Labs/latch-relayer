// Package balance watches the XLM balances the gasless service depends on.
//
// The funder pays every sponsored transaction's fee, so running it dry is a
// silent outage; channels need their base reserve to exist at all. The
// monitor turns both into state the rest of the service acts on (refuse
// sponsorship, take a channel out of rotation) and into metrics to alert on.
package balance

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/latch/relayer/internal/gasless/chain"
	"github.com/latch/relayer/internal/gasless/channels"
	"github.com/latch/relayer/internal/gasless/keys"
)

// ChannelStore is what the monitor needs from the channel pool.
type ChannelStore interface {
	RecordBalance(ctx context.Context, index int, balanceStroops, minStroops int64) error
	Stats(ctx context.Context) (channels.Stats, error)
}

type Config struct {
	Executor, Funder  string
	Channels          []keys.Channel
	FunderMinStroops  int64
	ChannelMinStroops int64
	Interval          time.Duration
}

// Snapshot is the latest observed state, served on /health.
type Snapshot struct {
	CheckedAt       time.Time      `json:"checked_at"`
	FunderStroops   int64          `json:"funder_balance_stroops"`
	ExecutorStroops int64          `json:"executor_balance_stroops"`
	SponsorshipOK   bool           `json:"sponsorship_available"`
	Channels        channels.Stats `json:"channels"`
}

type Monitor struct {
	rpc   chain.RPC
	store ChannelStore
	cfg   Config

	mu   sync.RWMutex
	snap Snapshot

	funderGauge, executorGauge, availableGauge prometheus.Gauge
	channelGauge                               *prometheus.GaugeVec
}

func NewMonitor(rpc chain.RPC, store ChannelStore, cfg Config, reg prometheus.Registerer, namespace string) *Monitor {
	m := &Monitor{
		rpc: rpc, store: store, cfg: cfg,
		funderGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "funder_balance_stroops",
			Help: "XLM balance of the fee-bump funder, in stroops. Alert well above FUNDER_MIN_XLM.",
		}),
		executorGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "executor_balance_stroops",
			Help: "XLM balance of the executor, in stroops.",
		}),
		availableGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "sponsorship_available",
			Help: "1 when the funder is above its floor and sponsorship is accepted, else 0.",
		}),
		channelGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "channels",
			Help: "Channel accounts by state (active includes leased).",
		}, []string{"state"}),
	}
	reg.MustRegister(m.funderGauge, m.executorGauge, m.availableGauge, m.channelGauge)
	return m
}

// Snapshot returns the latest check. Before the first check SponsorshipOK is
// false: the service doesn't sponsor on balances it hasn't seen.
func (m *Monitor) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snap
}

// SponsorshipOK reports whether the funder can cover more transactions.
func (m *Monitor) SponsorshipOK() bool { return m.Snapshot().SponsorshipOK }

// Run checks once immediately, then every Interval until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) {
	m.Check(ctx)
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Check(ctx)
		}
	}
}

// Check reads every balance once and updates state, channel rotation and
// metrics. A failed read keeps the previous snapshot — an RPC blip shouldn't
// flip sponsorship off — but is logged.
func (m *Monitor) Check(ctx context.Context) {
	addrs := make([]string, 0, len(m.cfg.Channels)+2)
	addrs = append(addrs, m.cfg.Funder, m.cfg.Executor)
	for _, ch := range m.cfg.Channels {
		addrs = append(addrs, ch.Address())
	}

	accts, err := chain.Accounts(ctx, m.rpc, addrs)
	if err != nil {
		slog.Error("balance monitor: read balances", "err", err)
		return
	}

	for _, ch := range m.cfg.Channels {
		bal := int64(-1) // not found on network
		if a := accts[ch.Address()]; a.Exists {
			bal = a.BalanceStroops
		}
		if err := m.store.RecordBalance(ctx, ch.Index, bal, m.cfg.ChannelMinStroops); err != nil {
			slog.Error("balance monitor: record channel", "index", ch.Index, "err", err)
		}
	}
	stats, err := m.store.Stats(ctx)
	if err != nil {
		slog.Error("balance monitor: channel stats", "err", err)
	}

	funder := accts[m.cfg.Funder].BalanceStroops
	snap := Snapshot{
		CheckedAt:       time.Now(),
		FunderStroops:   funder,
		ExecutorStroops: accts[m.cfg.Executor].BalanceStroops,
		SponsorshipOK:   funder >= m.cfg.FunderMinStroops,
		Channels:        stats,
	}
	if !snap.SponsorshipOK {
		slog.Error("balance monitor: funder below floor — sponsorship refused until topped up",
			"funder", m.cfg.Funder, "balance_stroops", funder, "floor_stroops", m.cfg.FunderMinStroops)
	}
	if stats.Disabled > 0 {
		slog.Warn("balance monitor: channels out of rotation", "disabled", stats.Disabled, "active", stats.Active)
	}

	m.mu.Lock()
	m.snap = snap
	m.mu.Unlock()

	m.funderGauge.Set(float64(snap.FunderStroops))
	m.executorGauge.Set(float64(snap.ExecutorStroops))
	m.availableGauge.Set(boolFloat(snap.SponsorshipOK))
	m.channelGauge.WithLabelValues("active").Set(float64(stats.Active))
	m.channelGauge.WithLabelValues("leased").Set(float64(stats.Leased))
	m.channelGauge.WithLabelValues("disabled").Set(float64(stats.Disabled))
	m.channelGauge.WithLabelValues("retired").Set(float64(stats.Retired))
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
