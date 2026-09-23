// Command gasless is the gasless sponsorship service: latch-relayer's executor
// for the FeeForwarder contract. It runs as its own process, with its own keys
// and database, separate from the deposit bridge (cmd/serve) — pool keys never
// enter this process.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/rpcclient"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/db"
	"github.com/latch/relayer/internal/gasless/api"
	"github.com/latch/relayer/internal/gasless/balance"
	"github.com/latch/relayer/internal/gasless/channels"
	"github.com/latch/relayer/internal/gasless/startup"
	"github.com/latch/relayer/internal/httpx"
	"github.com/latch/relayer/internal/lifecycle"
	"github.com/latch/relayer/internal/metrics"
	"github.com/latch/relayer/migrations"
)

func main() {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg, err := config.LoadGasless()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded",
		"fee_forwarder", cfg.FeeForwarderID,
		"executor", cfg.Executor.Address(),
		"funder", cfg.Funder.Address(),
		"channels", len(cfg.Channels),
		"instance", cfg.InstanceID,
		"port", cfg.Port,
	)

	// ── 2. Contexts ───────────────────────────────────────────────────────────
	// ctx stops intake on shutdown; `work` carries in-flight sponsored
	// transactions (from P4) until the drain deadline.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work := lifecycle.NewTracker()

	// ── 3. Database ───────────────────────────────────────────────────────────
	pool, err := db.Connect(ctx, cfg.DatabaseURL, db.PoolOptions{MaxConns: cfg.DBMaxConns, MinConns: cfg.DBMinConns})
	if err != nil {
		slog.Error("database: connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := migrations.Run(ctx, pool, migrations.Gasless); err != nil {
		slog.Error("database: migrations", "err", err)
		os.Exit(1)
	}

	chanPool := channels.NewPool(pool, cfg.InstanceID)
	if err := chanPool.Sync(ctx, cfg.Channels); err != nil {
		slog.Error("channels: sync", "err", err)
		os.Exit(1)
	}
	slog.Info("database ready")

	// ── 4. Stellar RPC + preflight ────────────────────────────────────────────
	rpc := rpcclient.NewClient(cfg.RPCURL, &http.Client{Timeout: cfg.RPCTimeout, Transport: httpx.OutboundTransport()})
	defer rpc.Close()

	// Refuse to start on a wrong network, a missing account, or an executor
	// without the FeeForwarder role — a failed boot keeps the last good deploy
	// serving instead of running a service that can't sponsor anything.
	if err := startup.Preflight(ctx, rpc, startup.Check{
		Passphrase:     cfg.NetworkPassphrase,
		FeeForwarderID: cfg.FeeForwarderID,
		Executor:       cfg.Executor.Address(),
		Funder:         cfg.Funder.Address(),
	}, time.Minute); err != nil {
		slog.Error("gasless preflight failed", "err", err)
		os.Exit(1)
	}
	slog.Info("preflight passed: executor holds the FeeForwarder role")

	// ── 5. Background workers ─────────────────────────────────────────────────
	m := metrics.New(cfg.MetricsNamespace)
	monitor := balance.NewMonitor(rpc, chanPool, balance.Config{
		Executor:          cfg.Executor.Address(),
		Funder:            cfg.Funder.Address(),
		Channels:          cfg.Channels,
		FunderMinStroops:  cfg.FunderMinStroops,
		ChannelMinStroops: cfg.ChannelMinStroops,
		Interval:          cfg.BalanceCheckInterval,
	}, m.Registry, cfg.MetricsNamespace)
	monitorDone := make(chan struct{})
	go func() { monitor.Run(ctx); close(monitorDone) }()

	// ── 6. HTTP server ────────────────────────────────────────────────────────
	draining := &httpx.Draining{}
	limiter := httpx.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)

	mux := http.NewServeMux()
	(&api.API{
		DB:             pool,
		Status:         monitor,
		FeeForwarderID: cfg.FeeForwarderID,
		Executor:       cfg.Executor.Address(),
		Funder:         cfg.Funder.Address(),
	}).Register(mux)
	mux.Handle("GET /metrics", m.Handler())

	// Same middleware order as the deposit service (see cmd/serve/main.go).
	var root http.Handler = mux
	root = limiter.Middleware(httpx.CallerKey, m.Rejected)(root)
	root = httpx.RequireBearer(cfg.APIKey)(root)
	root = httpx.LimitInflight(cfg.MaxInflight, draining, m.Rejected)(root)
	root = m.Middleware(root)
	root = httpx.WithRequestID(root)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      root,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	go func() {
		slog.Info("http server listening", "port", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server", "err", err)
			cancel()
		}
	}()

	// ── 7. Wait for shutdown signal, then drain ───────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	case <-ctx.Done():
	}

	deadline := time.Now().Add(cfg.ShutdownDrain)
	draining.Set()
	cancel()

	shutdownCtx, shutdownCancel := context.WithDeadline(context.Background(), deadline)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown", "err", err)
	}
	if !work.Drain(time.Until(deadline)) {
		slog.Warn("shutdown: drain deadline passed, cancelled remaining in-flight work")
	}
	select {
	case <-monitorDone:
	case <-time.After(time.Until(deadline)):
	}
	slog.Info("shutdown complete")
}
