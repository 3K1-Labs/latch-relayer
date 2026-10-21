# latch-relayer — Production Gap Tracker

Gaps between what ships today and a production relayer on mainnet. Every entry was checked against the code on 2026-10-20 (`main` at 85cc0dd), including a pass over OpenZeppelin relayer commits up to 2026-10-15. Fixed items are listed under [Resolved](#resolved) with the code that fixes them; their full write-ups are in git history.

## Priority legend

| Level | Meaning |
|-------|---------|
| **Launch** | Must ship before mainnet launch. |
| **P1** | Reliability. Affects how well the service survives real conditions. |
| **P2** | Operational maturity. Important but won't cause immediate failures. |
| **P3** | Polish. Low-risk improvement or long-term hygiene. |

---

## 1 · Launch blockers

### [Launch] Executor and funder keys loaded from env

**File:** [internal/signer/signer.go](internal/signer/signer.go)

**What we have:** All signing goes through the `Signer` interface, but the only implementation is an in-memory keypair loaded from env.

**What we need:** A KMS-backed `Signer` (GCP KMS or Vault Transit; confirm Ed25519 support before choosing AWS KMS) for the executor and funder. Channels can stay seed-derived: they hold ~1.5 XLM and no role. OZ ships AWS, GCP, Azure Key Vault, Vault and Turnkey signers.

### [Launch] Deposit forwarder bids a fixed inclusion fee

**File:** [internal/service/forwarder/forwarder.go](internal/service/forwarder/forwarder.go)

**What we have:** Every forward starts at `txnbuild.MinBaseFee` (100 stroops) and doubles on `tx_insufficient_fee` at most `maxFeeRetries = 2` times, so the highest bid is 400 stroops. Under surge pricing a forward exhausts that and drops to `pending_retry`. The gasless submitter already bids the `getFeeStats` p90.

**What we need:** The same p90 bid with a configured cap for the forwarder. OZ also raised its fee-retry ceiling from 2 to 6 and resubmits after a 5s × attempt delay (OZ #822).

### [Launch] Mainnet deployment steps

- Call `enable_fee_token` on FeeForwarder for the native XLM SAC and the Circle USDC SAC. Until the first token is enabled, the contract accepts **any** token as the fee.
- Point `RPC_URL` at the paid provider chosen for mainnet. Never put that key in a client build.

---

## 2 · Deposit bridge

### [P1] No per-forward exponential backoff

**File:** [internal/service/retry/retry.go](internal/service/retry/retry.go)

**What we have:** The retry worker ticks on a flat interval and picks up every `pending_retry` forward. Retries are capped at `maxRetries = 5`, but a forward that failed seconds ago is retried on the next tick.

**What we need:** A `next_retry_at` column (migration) set to `NOW() + backoff` on each transient failure, with `GetPendingRetries` filtering on it. OZ's backoff: base 10s, factor 1.5×, cap 120s.

### [P1] Sequence resync can reuse an in-flight sequence

**File:** [internal/service/forwarder/sequence.go](internal/service/forwarder/sequence.go)

**What we have:** After a failed submission, `resync` reloads the pool's sequence from Horizon, which reports the last closed ledger. Forwards that already left the send lock may still be pending, so the next forward can draw a number one of them holds, get `txBadSeq`, and resync again. It heals once the pending forwards land, but thrashes under bursts. Inferred from the code; not reproduced.

**What we need:** Reseed to the larger of the chain's next sequence and the highest in-flight sequence + 1, as OZ does for its counter (OZ #832).

### [P2] Watcher retries a failed poll after a flat 5s

**File:** [internal/service/watcher/watcher.go](internal/service/watcher/watcher.go)

**What we have:** The watcher polls Horizon pages. After a poll error it waits exactly 5 seconds and resumes from the saved cursor, indefinitely.

**What we need:** Exponential backoff with ±20% jitter (5s → 60s cap), reset on a successful page.

### [P2] Deposit watcher depends on Horizon

**What we have:** Pool payments are read from Horizon `/accounts/{pool}/payments`.

**What we need:** Nothing before launch: the pool is a classic account, and Horizon stays for it. Longer term, since protocol 23 classic payments emit `transfer` events, so the watcher can move to RPC `getEvents` filtered on the pool address, fetching the memo with `getTransaction`.

### [P3] Forward failures are free text

**What we have:** `forwards.error` holds the error string. The gasless table already records an `error_code`.

**What we need:** A structured failure code on `forwards` (for example the transaction result code), so latch-api and alerts can branch on it. OZ did the same for Stellar (OZ #765).

---

## 3 · Gasless sponsorship

### [P3] Auth-entry check misses two Protocol 28 cases

**File:** [internal/gasless/sponsor/request.go](internal/gasless/sponsor/request.go)

**What we have:** `authAddress` refuses source-account credentials and rejects auth entries that claim a relayer account. `AddressWithDelegates` credentials are rejected outright (fails closed), and a muxed M-address for a relayer account is compared as-is rather than reduced to its G-key.

**What we need:** Resolve muxed addresses to the underlying account before the relayer-account check. Accept delegates only if Latch wallets start using them, recursing through nested delegates as OZ does (OZ #873). Low risk: the relayer never signs those entries.

---

## 4 · Observability & operations

### [P2] No Grafana / monitoring stack or alert rules

**What we have:** `GET /metrics` on both services. No dashboards or alert rules.

**What we need:** Scrape config, a dashboard, and alerts on at least: `latch_gasless_funder_balance_stroops` near its floor, `latch_gasless_sponsorship_available == 0`, `latch_gasless_channels{state="disabled"} > 0`, and `relayer_pending_retry_depth` growing. Alert on the deposit pool key and the gasless funder separately: they are different accounts.

### [P2] No webhook notifications for forward state changes

**What we have:** latch-api polls `GET /deposit/status/{memo_id}`.

**What we need:** Optional `WEBHOOK_URL` + `WEBHOOK_SECRET`. On `done` or `failed`, POST a JSON event signed with HMAC-SHA256 in an `X-Signature` header.

### [P2] No key rotation without restart

**What we need:** Covered by the KMS `Signer` above for the executor and funder. Pool keys still need a redeploy to rotate.

---

## 5 · Testing

### [P1] No unit tests for the retry worker

**What we have:** Tests cover the forwarder, watcher, handler, store, signer, httpx, lifecycle, metrics and every gasless package. `internal/service/retry` has none.

**What we need:** Test that `tick()` expires intents, picks up `pending_retry` rows and respects the retry ceiling.

### [P2] No fault injection for Stellar RPC errors

**What we need:** A WireMock (or equivalent) setup that produces `TRY_AGAIN_LATER`, `tx_insufficient_fee`, RPC 500s and timeouts on demand. GitHub Issue #4 has the spec; OZ's timing-matrix harness in OZ #822 is a model.

---

## 6 · Database

### [P3] No down migrations

**What we have:** `migrations/deposit` and `migrations/gasless` contain only `.up.sql` files. #66 adds them.

**What we need:** A `.down.sql` for each migration.

### [P3] Migrations run at startup

**What we have:** Both `cmd/serve` and `cmd/gasless` call `migrations.Run` in `main()`.

**What we need:** A separate migrate step in the deploy pipeline before the service starts.

---

## Not needed (checked against OZ, 2026-10-20)

| OZ change | Why we don't need it |
|-----------|----------------------|
| Idempotency-Key header (OZ #893, #917) | Gasless `request_id` plus payload hash already does this (409 `request_id_conflict` on mismatch); deposits are keyed by memo |
| Protocol 28 XDR upgrade (OZ #873) | go-stellar-sdk v0.7.3 already includes Protocol 28 (CAP-83, CAP-85) |
| Redis status-index repair, Redis worker IDs, RabbitMQ and Pub/Sub queues (OZ #823, #892, #798, #791) | Our state is in Postgres; no Redis or external queue |
| Masking provider URLs in RPC logs (OZ #916) | Neither service logs the RPC or Horizon URL |

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
| No sponsor endpoint | `POST` sponsored submit through leased channels with a funder fee-bump (#64, `internal/gasless/api/submit.go`) |
| No sponsored (Latch-pays) mode | `SPONSORED_CALLS` allowlist with per-wallet caps recorded in Postgres (#64, `internal/gasless/sponsor/store.go`) |
| No fee-token selection or USDC pricing | Forward mode in XLM or USDC, priced from the enforce-mode simulation with a bounded, last-good XLM price (#67, #68) |
| Gasless inclusion fee fixed | p90 from `getFeeStats`, capped by `MAX_INCLUSION_FEE_STROOPS` (`internal/gasless/sponsor/submitter.go`) |
