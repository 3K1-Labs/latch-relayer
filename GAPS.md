# latch-relayer — Production Gap Tracker

Every gap between what we have now and a production-grade relayer, found by reading the OZ relayer source code and auditing our own codebase. Each entry states what we have, what we need, and why it matters.

## Priority legend

| Level | Meaning |
|-------|---------|
| **P0** | Correctness / safety. Causes data loss, infinite loops, or silent failures in production. |
| **P1** | Reliability. Affects how well the service survives real conditions. |
| **P2** | Operational maturity. Important but won't cause immediate failures. |
| **P3** | Polish. Low-risk improvement or long-term hygiene. |

---

## 1 · Core forward reliability

### [P0] Retries column is incremented but never read

**File:** [internal/store/store.go:199](internal/store/store.go#L199), [internal/service/forwarder/forwarder.go:98](internal/service/forwarder/forwarder.go#L98)

**What we have:** `MarkForwardFailed` runs `retries = retries + 1` in the DB. But `Retry()` receives the `Forward` struct — which includes `Retries` — and ignores it. There is no ceiling. A permanently broken forward gets retried every 30 seconds forever.

**What we need:** In `Retry()`, read `fwd.Retries`. If it has exceeded a threshold (e.g. 5), mark it `failed` immediately without calling `submit()`. Do not call `MarkForwardFailed` again (which would increment again); call a dedicated `PermanentlyFail` that sets status to `failed` without touching the counter.

---

### [P0] No error classification — permanent vs transient

**File:** [internal/service/forwarder/forwarder.go:120](internal/service/forwarder/forwarder.go#L120)

**What we have:** `submit()` returns `fmt.Errorf(...)` for every failure. All errors look identical. A bad C-address (contract doesn't exist), an XDR build failure, and a network timeout all go down the same path: queue for retry.

**What we need:** A typed error or a `isPermanent bool` sentinel from `submit()`. Errors from `keypairFor`, XDR build steps, and simulation (`simResp.Error != ""`) are permanent — the same input will always produce the same failure. Network errors, Stellar RPC timeouts, and fee errors are transient.

In `Forward()`: if `submit()` returns a permanent error, call `MarkForwardFailed(StatusFailed)` immediately. Do not queue for retry. Same in `Retry()`.

OZ's pattern: `TransactionError` enum with `is_transient() bool` that switches on variant. Permanent variants: `ValidationError`, `SignerError`, `InsufficientBalance`, `SimulationFailed`. Transient: `UnexpectedError`, `JobProducerError`.

---

### [P0] Transaction submitted via Horizon instead of Stellar RPC

**File:** [internal/service/forwarder/forwarder.go:219](internal/service/forwarder/forwarder.go#L219)

**What we have:** We simulate via `rpc.SimulateTransaction` (correct), then submit via `horizon.SubmitTransaction`. Horizon wraps and hides the Stellar RPC response codes.

**What we need:** Submit via `rpc.SendTransaction`. Stellar RPC returns one of: `PENDING`, `DUPLICATE`, `TRY_AGAIN_LATER`, `ERROR`. Only `PENDING` means the transaction entered the network. Only with `rpc.SendTransaction` can we detect `TRY_AGAIN_LATER` and handle it correctly (see gap below). After a `PENDING` response, poll `rpc.GetTransaction(hash)` until `SUCCESS` or `FAILED` (or timeout). This is the correct Soroban submit path.

---

### [P0] TRY_AGAIN_LATER and insufficient fee not detected

**File:** [internal/service/forwarder/forwarder.go:219](internal/service/forwarder/forwarder.go#L219)

**What we have:** Once we fix the submit path to use RPC (gap above), we can read the RPC status codes. Right now we can't.

**What we need:**
- `TRY_AGAIN_LATER`: Stellar Core's mempool is full. Do not count as a failure. Delay and resubmit (at least one Stellar ledger cycle, ~5s). OZ tracks `try_again_later_retries` separately with a max of its own. For us: on `TRY_AGAIN_LATER`, sleep briefly and retry within the same attempt loop rather than going to `pending_retry`.
- `ERROR` with `tx_insufficient_fee` result code: bump the fee and resubmit. OZ allows up to 2 insufficient-fee retries (`STELLAR_INSUFFICIENT_FEE_MAX_RETRIES = 2`). For us: parse the error XDR or error string, double the fee, rebuild and resubmit.

---

### [P0] Stuck `pending` forwards never recovered after crash

**File:** [internal/service/watcher/watcher.go:116](internal/service/watcher/watcher.go#L116), [internal/service/retry/retry.go:49](internal/service/retry/retry.go#L49)

**What we have:** When the watcher dispatches `go w.forwarder.Forward(ctx, ...)` and then calls `w.saveCursor()`, the cursor advances immediately. If the process crashes after the cursor is saved but before `InsertForward` runs, that event is gone. More importantly, if `InsertForward` succeeds (so a row exists in `pending`) but the process crashes before any attempt is made, the row stays in `pending` forever — the retry worker only queries `WHERE status = 'pending_retry'`.

**What we need:** Add a recovery path in the retry worker: query forwards in `pending` status older than N minutes (e.g. 5 min) and treat them as `pending_retry`. This covers crash-recovery without changing the submit flow.

SQL to add to `GetPendingRetries`:
```sql
SELECT ... FROM forwards
WHERE status = 'pending_retry'
   OR (status = 'pending' AND created_at < NOW() - INTERVAL '5 minutes')
ORDER BY created_at ASC
```

---

### [P1] No per-forward exponential backoff

**File:** [internal/service/retry/retry.go](internal/service/retry/retry.go)

**What we have:** The retry worker polls every 30 seconds flat and picks up ALL `pending_retry` forwards unconditionally. A forward that failed 2 seconds ago gets retried in the next tick.

**What we need:** A `next_retry_at TIMESTAMPTZ` column on `forwards`. When a transient failure occurs, set `next_retry_at = NOW() + backoff`. The backoff follows OZ's pattern: base 10s, factor 1.5×, cap 120s (10s → 15s → 22s → 33s → 50s → 75s → 113s → 120s). `GetPendingRetries` filters `WHERE next_retry_at IS NULL OR next_retry_at <= NOW()`.

This requires a migration. Without it, every transient RPC failure hammers the network on the next 30s tick.

---

## 2 · Health endpoint

### [P0] Health always returns 200, never checks DB

**File:** [internal/handler/handler.go:73](internal/handler/handler.go#L73)

**What we have:** `Health()` returns `{"status": "ok"}` unconditionally. A load balancer, health check, or on-call engineer cannot use this endpoint to determine if the relayer is actually functional.

**What we need:**
```go
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
    defer cancel()
    if err := h.store.Ping(ctx); err != nil {
        writeJSON(w, http.StatusServiceUnavailable, map[string]string{
            "status": "unhealthy", "db": err.Error(),
        })
        return
    }
    writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
```

Add `Store.Ping(ctx)` that calls `s.pool.Ping(ctx)`. OZ returns `Healthy` (200), `Degraded` (200), `Unhealthy` (503) — at minimum we need the 503 path.

---

## 3 · Observability

### [P1] No Prometheus /metrics endpoint

**Status: resolved.** `GET /metrics` (bearer-authenticated) exposes HTTP request count/latency by route, load-shed rejections and Go/process metrics (#46), plus the deposit metrics from #34: `relayer_forwards_total{outcome}`, `relayer_pool_contention_total`, `relayer_pending_retry_depth` and `relayer_deposit_to_credit_seconds`. Dashboards and alerts (below) remain.

**What we have:** Structured logging via `slog`. No counters, histograms, or gauges. No way to answer: "how many forwards processed per hour?", "what's the P95 forward duration?", "how many are sitting in pending_retry right now?"

**What we need:** GitHub Issue #3 has the full implementation spec. Summary:
- Add `prometheus/client_golang` dependency
- Define 6 metrics: `payments_received_total`, `forwards_success_total`, `forwards_failed_total`, `forwards_pending_retry_gauge`, `recovery_sent_total`, `forward_duration_seconds`
- Expose `GET /metrics` endpoint via `promhttp.Handler()`
- Instrument `Forward()`, `Retry()`, `sweep()` with counter/histogram calls

---

### [P2] No Grafana / monitoring stack

**What we have:** Nothing.

**What we need:** GitHub Issue #3 has the full spec. Summary:
- `cmd/prometheus/prometheus.yml` — scrape config (interval 10s, scrapes `/metrics`)
- `cmd/prometheus/grafana.ini` — anonymous viewer access
- `cmd/prometheus/datasources/prometheus.yml` — Grafana data source
- `cmd/prometheus/dashboards/` — pre-built JSON dashboard
- Extend `docker-compose.yaml` with prometheus + grafana services

---

## 4 · HTTP server

### [P1] No request ID / correlation ID

**Status: resolved** for HTTP requests (`internal/httpx/requestid.go`: honours a well-formed caller `X-Request-ID`, else generates one; echoed back and logged per request). Deposit forwards start from the SSE stream, not a request, so they have no request ID to carry.

**What we have:** Requests are logged with their path and status, but there is no correlation ID linking an incoming HTTP request to the downstream `slog` output from `forwarder.Forward()`.

**What we need:** Middleware that generates a UUID per request, injects it into the context, logs it at request start, and returns it as `X-Request-ID` in the response header. Pass the context through to `Forward()` calls so all logs for one request share the same ID.

---

### [P2] No HTTP concurrency limit

**Status: resolved** (`internal/httpx/limit.go`). `MAX_INFLIGHT_REQUESTS` caps concurrent requests with **503** + `Retry-After` (server-side overload, not the caller's fault), and a per-caller token bucket (`RATE_LIMIT_RPS`/`RATE_LIMIT_BURST`) returns **429**. `/health` and `/metrics` are exempt.

**What we have:** `http.ServeMux` with no bound on concurrent handlers. `CreateIntent` hits the DB — a burst of requests can exhaust the connection pool.

**What we need:** A semaphore middleware that returns 429 when the concurrent request count exceeds a threshold (e.g. 100). OZ's implementation: `semaphore.try_acquire()` — if no permits, return 429 with `{"error": "Too many concurrent requests", "max_concurrent": N}`.

---

## 5 · Security & key management

### [P0] Horizon and RPC clients have no HTTP timeout

**File:** [cmd/serve/main.go:58-59](cmd/serve/main.go#L58)

**What we have:**
```go
hz := &horizonclient.Client{HorizonURL: cfg.HorizonURL}
rpc := rpcclient.NewClient(cfg.RPCURL, nil)  // nil = default http.Client
```
Neither client has a connect or request timeout. `AccountDetail()` inside `submit()` can hang indefinitely if Horizon is slow — blocking the goroutine and any worker threads behind it.

**What we need:** Pass a custom `http.Client` with explicit timeouts to both clients:
- Connect timeout: 2s (OZ: `DEFAULT_HTTP_CLIENT_CONNECT_TIMEOUT_SECONDS = 2`)
- Request timeout: 10s (OZ: `DEFAULT_HTTP_CLIENT_TIMEOUT_SECONDS = 10`)
- Keep-alive: 30s

---

### [P1] Private keys loaded into plain heap memory

**File:** [internal/config/config.go:79](internal/config/config.go#L79)

**What we have:** `keypair.ParseFull(seed)` stores the raw secret seed in a regular Go string in heap memory. The OS can swap it to disk. A heap dump exposes it.

**What we need (short term):** `runtime.SetFinalizer` or `memguard` to zero the key material on GC. Avoid logging anything that touches the keypair.

**What we need (mainnet):** Replace the seed-in-env pattern with a KMS backend. OZ supports AWS KMS, GCP KMS, Azure Key Vault, HashiCorp Vault, and Turnkey. For us: at minimum, support `POOL_KMS_ARN_1` as an alternative to `POOL_PRIVATE_KEY_1`, with the key signing happening inside AWS KMS (key never leaves the HSM).

---

### [P2] No key rotation without restart

**What we have:** Pool keys are loaded once at startup from env vars. To rotate a key, you must redeploy.

**What we need:** A SIGHUP handler that reloads config and re-initialises keypairs (and optionally Horizon/RPC clients) without dropping the SSE stream. Or, preferably, move to KMS — key rotation is then managed in the KMS console without any relayer change.

---

## 6 · SSE watcher reliability

### [P1] Watcher dispatches unlimited goroutines — no concurrency cap

**File:** [internal/service/watcher/watcher.go:116](internal/service/watcher/watcher.go#L116)

**Status: resolved** (#34). Each watcher runs 32 forward workers over a 256-deep queue that back-pressures the stream. Workers run on the lifecycle tracker, so on shutdown they finish queued payments (whose cursors are already saved) within `SHUTDOWN_DRAIN_SECONDS`.

**What we have:** Every inbound payment spawns `go w.forwarder.Forward(ctx, ...)`. Under a burst of deposits (or after a reconnect that replays events), an unbounded number of goroutines run simultaneously, each submitting a Soroban transaction to Stellar RPC. Soroban transactions from the same source account need ordered sequence numbers — concurrent submissions will cause `bad_seq` errors.

**What we need:** A buffered channel (semaphore) that caps the number of concurrent `Forward()` calls. Size: 1 for now (sequence numbers must be serial) or N with per-account sequence locking. OZ uses `DEFAULT_CONCURRENCY = 100` for their multi-account setup but serialises per-account.

For us, serialising all forwards from the same pool account is correct:
```go
w.sem <- struct{}{}   // acquire
go func() {
    defer func() { <-w.sem }()
    w.forwarder.Forward(ctx, ...)
}()
```

---

### [P1] No graceful shutdown for in-flight forward goroutines

**File:** [cmd/serve/main.go:102](cmd/serve/main.go#L102)

**Status: resolved** (`internal/lifecycle/tracker.go`). Forwards run on a tracked context that the shutdown signal does not cancel; on SIGTERM the relayer refuses new requests, stops intake, and gives in-flight work `SHUTDOWN_DRAIN_SECONDS` before cancelling it.

**What we have:** On SIGTERM, `cancel()` is called, which signals all goroutines via context. But goroutines dispatched by the watcher are untracked — there is no `sync.WaitGroup`. The process exits while forwards may be mid-flight (between `InsertForward` and `MarkForwardDone`). The forward will be in `pending` status (covered by the crash recovery gap above), but ideally we drain cleanly.

**What we need:** A `sync.WaitGroup` that each `go Forward()` goroutine increments on start and decrements on finish. After cancel, `main()` waits: `wg.Wait()` with a timeout (e.g., 30s) before exiting.

---

### [P2] Watcher reconnects with flat 5s delay — no exponential backoff

**File:** [internal/service/watcher/watcher.go:38](internal/service/watcher/watcher.go#L38)

**What we have:** On stream error, the watcher waits exactly 5 seconds and reconnects. If Horizon is down, we send a connection attempt every 5 seconds indefinitely — unhelpful load on a recovering service.

**What we need:** Exponential backoff with jitter: 5s → 10s → 20s → 40s → 60s (capped), with ±20% jitter per attempt. Reset to 5s on a successful event. OZ uses `RETRY_JITTER_PERCENT = 0.2`.

---

## 7 · Database & infrastructure

### [P2] DB connection pool uses all defaults

**File:** [internal/db/db.go](internal/db/db.go)

**Status: resolved.** `DB_MAX_CONNS` (default 20) / `DB_MIN_CONNS` (default 2), plus connection lifetime, idle time and health-check period.

**What we have:** `pgxpool.New(ctx, databaseURL)` — all pool parameters come from defaults (max 4 connections, or `PGPOOL_MAX_CONNS` env var). Under load, 4 connections can be a bottleneck. Under idle, stale connections accumulate.

**What we need:** Configure the pool explicitly:
```go
config, _ := pgxpool.ParseConfig(databaseURL)
config.MaxConns = 10
config.MinConns = 2
config.MaxConnIdleTime = 30 * time.Second
config.HealthCheckPeriod = 60 * time.Second
```

---

### [P3] No migration rollback (no down migrations)

**What we have:** Only `001_init.up.sql`. No corresponding `001_init.down.sql`.

**What we need:** A `down.sql` for each migration, so a bad schema change can be rolled back without manually writing SQL. Low urgency while we have a single migration.

---

## 8 · Testing

### [P1] No unit tests for forwarder, retry worker, watcher, or handlers

**What we have:** `internal/memo/memo_test.go` — tests for memo parsing and C-address validation. That's it.

**What we need:** Unit tests for every package that contains logic:
- `forwarder`: test `Forward()` happy path, permanent error short-circuits, retry ceiling, TRY_AGAIN_LATER path. Mock the Stellar clients with interfaces.
- `retry`: test `Worker.tick()` expires intents, picks up pending_retry rows, invokes Retry().
- `watcher`: test `handle()` dispatches for valid payments, skips non-payments, handles nil transaction data.
- `handler`: test `CreateIntent` validates C-address, `DepositStatus` returns 404 for unknown memo, `Health` returns 503 on DB failure.

---

### [P2] No fault injection / WireMock for Stellar RPC errors

**What we have:** Nothing — retry logic can only be verified against the live testnet, which can't be controlled to produce `TRY_AGAIN_LATER` or `tx_insufficient_fee` on demand.

**What we need:** GitHub Issue #4 has the full spec. Summary:
- WireMock container in docker-compose
- Mappings: `stellar-try-again-later.json`, `stellar-insufficient-fee.json`, `rpc-500.json`, `rpc-timeout.json`
- State-machine pattern: arm → fire once → reset to proxy mode
- Helper scripts: trigger, reset, check status

---

## 9 · Operations

### [P2] No webhook notifications for forward state changes

**What we have:** Forward state is only observable by polling `GET /deposit/status/{memo_id}`. `latch-api` must poll.

**What we need:** Optional webhook URL (`WEBHOOK_URL` env var). When a forward transitions to `done` or `failed`, POST a JSON event to the URL. Sign the payload with HMAC-SHA256 using a shared secret (`WEBHOOK_SECRET`), send as `X-Signature` header so the receiver can verify authenticity. OZ's pattern:
```
POST WEBHOOK_URL
X-Signature: base64(HMAC-SHA256(payload, secret))
Content-Type: application/json

{"event": "forward.done", "tx_hash": "...", "forward_tx": "...", "amount": "..."}
```

---

### [P3] Migrations run at startup — should be a separate step in production

**What we have:** `migrations.Run(ctx, pool)` is called in `main()`. If a migration is broken, the service won't start.

**What we need (long term):** A dedicated `cmd/migrate` binary that runs migrations separately from the service start. The deploy pipeline runs migrate first, then starts the service. This is standard practice for any service with managed downtime or rolling deploys.

---

## Summary

| # | Gap | Priority | Location |
|---|-----|----------|----------|
| 1 | Retry ceiling — `retries` column never read | P0 | forwarder.go, store.go |
| 2 | No error classification (permanent vs transient) | P0 | forwarder.go |
| 3 | Submit uses Horizon instead of RPC SendTransaction | P0 | forwarder.go |
| 4 | TRY_AGAIN_LATER and insufficient fee not detected | P0 | forwarder.go |
| 5 | Stuck `pending` forwards never recovered after crash | P0 | retry.go, store.go |
| 6 | Health endpoint always returns 200 | P0 | handler.go |
| 7 | Horizon / RPC HTTP clients have no timeout | P0 | main.go |
| 8 | No per-forward exponential backoff | P1 | retry.go, store.go |
| 9 | No Prometheus /metrics endpoint | P1 | (new) |
| 10 | No request ID / correlation ID | P1 | (new middleware) |
| 11 | Private keys in plain heap memory | P1 | config.go |
| 12 | Watcher dispatches unlimited goroutines | P1 | watcher.go |
| 13 | No graceful shutdown for forward goroutines | P1 | main.go |
| 14 | No unit tests for core packages | P1 | (all packages) |
| 15 | No HTTP concurrency limit | P2 | (new middleware) |
| 16 | No Grafana / monitoring stack | P2 | (new: cmd/prometheus/) |
| 17 | Watcher reconnect: flat 5s, should be exponential | P2 | watcher.go |
| 18 | DB connection pool uses all defaults | P2 | db.go |
| 19 | No webhook notifications | P2 | (new service) |
| 20 | No WireMock fault injection | P2 | (Issue #4) |
| 21 | No key rotation without restart | P2 | config.go |
| 22 | No migration rollback (no down migrations) | P3 | migrations/ |
| 23 | Migrations run at startup | P3 | main.go |
