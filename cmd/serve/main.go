package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"
	"github.com/stellar/go-stellar-sdk/clients/rpcclient"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/db"
	"github.com/latch/relayer/internal/handler"
	"github.com/latch/relayer/internal/httpx"
	"github.com/latch/relayer/internal/lifecycle"
	"github.com/latch/relayer/internal/metrics"
	"github.com/latch/relayer/internal/service/forwarder"
	"github.com/latch/relayer/internal/service/retry"
	"github.com/latch/relayer/internal/service/watcher"
	"github.com/latch/relayer/internal/store"
	"github.com/latch/relayer/migrations"
)

func main() {
	// ── 1. Config ─────────────────────────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded",
		"pools", len(cfg.PoolAccounts),
		"horizon", cfg.HorizonURL,
		"port", cfg.Port,
		"db_max_conns", cfg.DBMaxConns,
		"max_inflight", cfg.MaxInflight,
	)

	// ── 2. Contexts ───────────────────────────────────────────────────────────
	// ctx stops intake (SSE streams, retry ticks) on the shutdown signal.
	// Work already started — a forward mid-submission — runs on `work` instead
	// and gets until the drain deadline to finish.
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

	if err := migrations.Run(ctx, pool, migrations.Deposit); err != nil {
		slog.Error("database: migrations", "err", err)
		os.Exit(1)
	}
	slog.Info("database ready")

	// ── 4. Core services ──────────────────────────────────────────────────────
	st := store.New(pool)

	// Shared transport (connection pool) for all outbound Stellar calls.
	stellarTransport := httpx.OutboundTransport()
	horizonHTTP := &http.Client{Timeout: 10 * time.Second, Transport: stellarTransport}
	// RPC gets its own timeout: simulating a Soroban call that runs a smart
	// account's __check_auth can legitimately take longer than a Horizon read.
	rpcHTTP := &http.Client{Timeout: cfg.RPCTimeout, Transport: stellarTransport}

	hz := &horizonclient.Client{HorizonURL: cfg.HorizonURL, HTTP: horizonHTTP}
	rpc := rpcclient.NewClient(cfg.RPCURL, rpcHTTP)
	defer rpc.Close()
	fwd := forwarder.New(st, cfg, hz, rpc)

	// Horizon's SSE stream is long-lived and idles between payments, so it can't
	// share horizonHTTP's 10s Timeout — that applies to the whole request,
	// including reading the streaming body, and would abort a healthy stream
	// after 10s of inactivity. Reuse the transport (dial/keep-alive settings)
	// but rely on ctx cancellation, not a fixed deadline, to bound the stream.
	hzStream := &horizonclient.Client{
		HorizonURL: cfg.HorizonURL,
		HTTP:       &http.Client{Transport: stellarTransport},
	}

	// ── 5. Background workers ─────────────────────────────────────────────────
	var workers sync.WaitGroup

	// Retry worker polls every RETRY_INTERVAL_SEC (default 10s) for pending_retry forwards.
	workers.Go(func() { retry.NewWorker(st, fwd, cfg.RetryInterval).Run(ctx) })

	// One SSE watcher goroutine per pool address.
	for _, pa := range cfg.PoolAccounts {
		workers.Go(func() { watcher.New(pa, st, fwd, hzStream, work).Run(ctx) })
	}

	// ── 6. HTTP server ────────────────────────────────────────────────────────
	m := metrics.New(cfg.MetricsNamespace)
	m.RegisterDeposit()
	draining := &httpx.Draining{}
	limiter := httpx.NewRateLimiter(cfg.RateLimitRPS, cfg.RateLimitBurst)

	mux := http.NewServeMux()
	h := handler.New(st, cfg)
	h.RegisterRoutes(mux)
	mux.Handle("GET /metrics", m.Handler())

	// Outermost first. Auth wraps the whole mux rather than individual routes,
	// so a route added later is authenticated by default. Rate limiting sits
	// after auth so only a valid caller ever gets a bucket. Metrics must reach
	// the mux without a request copy in between to see the route pattern.
	var root http.Handler = mux
	root = limiter.Middleware(httpx.CallerKey, m.Rejected)(root)
	root = h.RequireAPIKey(root)
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
			cancel() // treat a server crash as a shutdown signal
		}
	}()

	// ── 7. Wait for shutdown signal ───────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("shutdown signal received", "signal", sig)
	case <-ctx.Done():
	}

	// Drain: refuse new requests, stop intake, then give in-flight HTTP
	// requests and forwards until the deadline to finish. Anything cut off is
	// picked up again by the retry worker after restart.
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

	// Watchers and the retry ticker hold no in-flight work (forwards run on
	// `work`), but Horizon's SSE client can take a minute to notice a cancelled
	// context, so don't let it hold the process past the deadline.
	stopped := make(chan struct{})
	go func() { workers.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Until(deadline)):
		slog.Warn("shutdown: background workers still stopping at deadline; exiting anyway")
	}

	slog.Info("shutdown complete")
}
