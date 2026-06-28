package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stellar/go-stellar-sdk/clients/horizonclient"

	"github.com/latch/relayer/internal/config"
	"github.com/latch/relayer/internal/db"
	"github.com/latch/relayer/internal/handler"
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
	)

	// ── 2. Root context — cancelled on shutdown to stop all goroutines ────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── 3. Database ───────────────────────────────────────────────────────────
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		slog.Error("database: connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := migrations.Run(ctx, pool); err != nil {
		slog.Error("database: migrations", "err", err)
		os.Exit(1)
	}
	slog.Info("database ready")

	// ── 4. Core services ──────────────────────────────────────────────────────
	st := store.New(pool)
	hz := &horizonclient.Client{HorizonURL: cfg.HorizonURL}
	fwd := forwarder.New(st, cfg, hz)

	// ── 5. Background workers ─────────────────────────────────────────────────
	// Retry worker polls every 30s for pending_retry forwards.
	go retry.NewWorker(st, fwd, 30*time.Second).Run(ctx)

	// One SSE watcher goroutine per pool address.
	for _, pa := range cfg.PoolAccounts {
		go watcher.New(pa, st, fwd, hz).Run(ctx)
	}

	// ── 6. HTTP server ────────────────────────────────────────────────────────
	mux := http.NewServeMux()
	handler.New(st, cfg).RegisterRoutes(mux)

	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
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

	cancel() // propagate cancellation to watchers and retry worker

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("http server shutdown", "err", err)
	}

	slog.Info("shutdown complete")
}
