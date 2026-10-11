# latch-relayer — Production Gap Tracker

Gaps between what ships today and a production relayer on mainnet. Every entry was checked against the code on 2026-10-08. Fixed items are listed under [Resolved](#resolved) with the code that fixes them; their full write-ups are in git history.

## Priority legend

| Level | Meaning |
|-------|---------|
| **Launch** | Must ship before mainnet launch. |
| **P1** | Reliability. Affects how well the service survives real conditions. |
| **P2** | Operational maturity. Important but won't cause immediate failures. |
| **P3** | Polish. Low-risk improvement or long-term hygiene. |

---

## 1 · Gasless sponsorship (launch blockers)

latch-relayer becomes the only submitter for wallet transactions: latch-api validates and simulates, then hands the relayer a validated transaction, and the relayer submits it through a leased channel account with a fee-bump from the funder. The fee model is in [docs/gasless-keys.md](docs/gasless-keys.md#fee-model).

### [Launch] No sponsor endpoint

**What we have:** `cmd/gasless` exposes only `GET /health`, `GET /gasless/status` and `GET /metrics`. The channel lease pool, funder, executor check and balance monitor exist, but nothing submits a wallet transaction.

**What we need:** An API-key-authenticated endpoint that latch-api calls with the validated host function, the user's signed auth entries and the simulated resources (no source account, no sequence). The relayer leases a channel as the source, signs with it, wraps it in a fee-bump from the funder, sends via RPC, polls, and retries `tx_bad_seq` / `TRY_AGAIN_LATER` on a fresh lease. It returns the hash and final status. It re-checks the target contract against an allowlist as defence in depth. latch-api keeps the full validation (one invoke-host-function op, allowed contracts, enforcing-mode simulation).

**Why it matters:** latch-api's `submitWithBundler` uses one bundler account per network with no lock and no bad-sequence retry. Concurrent sends collide, and one source account caps throughput at about one transaction per ledger for all users.

### [Launch] No sponsored (Latch-pays) mode

**What we have:** The gasless design covers only FeeForwarder `forward()`, where the user reimburses the fee in a token.

**What we need:** A mode where the funder pays and the user is charged nothing, limited to a new wallet's setup transactions: wallet deployment and the setup-send-rules / setup-swap-rules steps. Enforce that list on the relayer side (target contract and function), cap it per wallet (count and total XLM), and record each sponsored transaction so the cap survives restarts. Every other transaction is paid by the user; a wallet with neither XLM nor USDC is refused with a clear "add XLM or USDC to pay network fees" error.

### [Launch] Fee-token selection and USDC pricing

**What we have:** Nothing chooses the fee token or computes `fee_amount`.

**What we need:** The relayer fills in `fee_amount` (≤ the user's signed `max_fee_amount`):
- **XLM** (native SAC): the actual network fee plus a small margin.
- **USDC:** the network fee converted at a trusted XLM/USDC price keyed by contract ID, never by token code. latch-api `/v1/prices` currently looks up by code and must be fixed first.

latch-api picks the fee token when it builds the authorization (XLM if the wallet can cover the fee plus the action, else USDC, else the sponsored mode) and shows the maximum fee to the user.

### [Launch] FeeForwarder fee-token allowlist not set on mainnet

**What we need:** Call `enable_fee_token` for the native XLM SAC and the Circle USDC SAC as part of the mainnet deployment. Until the first token is enabled, the contract accepts **any** token as the fee.

### [Launch] Executor and funder keys loaded from env

**File:** [internal/signer/signer.go](internal/signer/signer.go)

**What we have:** All signing goes through the `Signer` interface, but the only implementation is an in-memory keypair loaded from env.

**What we need:** A KMS-backed `Signer` (GCP KMS or Vault Transit; confirm Ed25519 support before choosing AWS KMS) for the executor and funder. Channels can stay seed-derived: they hold ~1.5 XLM and no role.

### [Launch] Inclusion fee is not taken from the network

**What we need:** Set the inclusion fee from RPC `getFeeStats` with a configured cap, instead of a fixed value. Keep the existing `tx_insufficient_fee` doubling (max 2) as the backstop.

### [Launch] Mainnet RPC is the public endpoint

**What we need:** Point `RPC_URL` at the paid provider chosen for mainnet. Never put that key in a client build.

---

## 2 · Deposit bridge

### [P1] No per-forward exponential backoff

**File:** [internal/service/retry/retry.go](internal/service/retry/retry.go)

**What we have:** The retry worker ticks on a flat interval and picks up every `pending_retry` forward. Retries are capped at `maxRetries = 5`, but a forward that failed seconds ago is retried on the next tick.

**What we need:** A `next_retry_at` column (migration) set to `NOW() + backoff` on each transient failure, with `GetPendingRetries` filtering on it. OZ's backoff: base 10s, factor 1.5×, cap 120s.

### [P2] Watcher retries a failed poll after a flat 5s

**File:** [internal/service/watcher/watcher.go](internal/service/watcher/watcher.go)

**What we have:** The watcher polls Horizon pages (no longer SSE). After a poll error it waits exactly 5 seconds and resumes from the saved cursor, indefinitely.

**What we need:** Exponential backoff with ±20% jitter (5s → 60s cap), reset on a successful page.

### [P2] Deposit watcher depends on Horizon

**What we have:** Pool payments are read from Horizon `/accounts/{pool}/payments`.

**What we need:** Nothing before launch: the pool is a classic account, and Horizon stays for it. Longer term, since protocol 23 classic payments emit `transfer` events, so the watcher can move to RPC `getEvents` filtered on the pool address, fetching the memo with `getTransaction`.

---

## 3 · Observability & operations

### [P2] No Grafana / monitoring stack or alert rules

**What we have:** `GET /metrics` on both services. No dashboards or alert rules.

**What we need:** Scrape config, a dashboard, and alerts on at least: `latch_gasless_funder_balance_stroops` near its floor, `latch_gasless_sponsorship_available == 0`, `latch_gasless_channels{state="disabled"} > 0`, and `relayer_pending_retry_depth` growing. Alert on the deposit pool key and the gasless funder separately: they are different accounts.

### [P2] No webhook notifications for forward state changes

**What we have:** latch-api polls `GET /deposit/status/{memo_id}`.

**What we need:** Optional `WEBHOOK_URL` + `WEBHOOK_SECRET`. On `done` or `failed`, POST a JSON event signed with HMAC-SHA256 in an `X-Signature` header.

### [P2] No key rotation without restart

**What we need:** Covered by the KMS `Signer` above for the executor and funder. Pool keys still need a redeploy to rotate.

---

## 4 · Testing

### [P1] No unit tests for the retry worker

**What we have:** Tests cover the forwarder, watcher, handler, store, signer, httpx, lifecycle, metrics and every gasless package. `internal/service/retry` has none.

**What we need:** Test that `tick()` expires intents, picks up `pending_retry` rows and respects the retry ceiling.

### [P2] No fault injection for Stellar RPC errors

**What we need:** A WireMock (or equivalent) setup that produces `TRY_AGAIN_LATER`, `tx_insufficient_fee`, RPC 500s and timeouts on demand. GitHub Issue #4 has the spec.

---

## 5 · Database

### [P3] No down migrations

**What we have:** `migrations/deposit` and `migrations/gasless` contain only `.up.sql` files.

**What we need:** A `.down.sql` for each migration.

### [P3] Migrations run at startup

**What we have:** Both `cmd/serve` and `cmd/gasless` call `migrations.Run` in `main()`.

**What we need:** A separate migrate step in the deploy pipeline before the service starts.

---

## Resolved

| Gap | Fixed in |
|-----|----------|
| Retries column never read | `maxRetries = 5` enforced on entry to retry (`internal/service/forwarder/forwarder.go`) |
| No permanent vs transient error classification | `permanent()` / `isPermanent()` short-circuit retries (`forwarder.go`) |
| Submitted via Horizon instead of RPC | `rpc.SendTransaction`, then poll `getTransaction` (`forwarder.go`) |
| `TRY_AGAIN_LATER` and insufficient fee not detected | `maxTryAgain = 5` one-ledger waits; `maxFeeRetries = 2` fee doubling (`forwarder.go`) |
| Stuck `pending` forwards after a crash | Row inserted before the cursor is saved; workers claim `pending` rows (`internal/service/watcher`, `internal/service/retry`) |
| Health always 200 | `Health()` pings the DB and returns 503 on failure (`internal/handler/handler.go`) |
| No HTTP client timeouts | Horizon 10s, RPC `RPC_TIMEOUT_MS` (`cmd/serve/main.go`) |
| No `/metrics` endpoint | Bearer-authenticated `GET /metrics` on both services (#34, #46) |
| No request ID | `internal/httpx/requestid.go` |
| No HTTP concurrency limit | `MAX_INFLIGHT_REQUESTS` (503) and per-caller rate limit (429), `internal/httpx/limit.go` |
| Watcher spawns unlimited goroutines | Bounded worker queue with back-pressure (#34) |
| No graceful shutdown | `internal/lifecycle/tracker.go`, `SHUTDOWN_DRAIN_SECONDS` |
| DB pool on defaults | `DB_MAX_CONNS` / `DB_MIN_CONNS`, lifetimes and health checks |
| One transaction per ledger from a single pool | Deposit channel accounts with leases and pool fee-bump (#48) |
| Most core packages untested | Tests in forwarder, watcher, handler, store, signer and gasless packages |
