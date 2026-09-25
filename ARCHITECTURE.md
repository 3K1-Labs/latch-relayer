# Latch Relayer — Architecture Decisions

## What It Is
An off-chain Go service that bridges the standard Stellar deposit UX (G-address + memo) to Soroban smart contract addresses (C-addresses). It watches a single pooled G-address, parses incoming payment memos, looks up the funding intent, and forwards funds to the correct C-address via Soroban SAC transfer.

---

## Decisions Made

### Language
Go — consistent with wallet-backend and stellar-freighter-backend-v2.

### Memo ID Generation
- **Per-intent random uint64**, generated server-side via `math/rand/v2` (auto-seeded from `crypto/rand` since Go 1.22)
- One memo_id per funding session — not permanent, not derived from the C-address
- Stored as `BIGINT` in PostgreSQL; Go casts `uint64 ↔ int64` preserving bits
- Collision retry: up to 5 attempts with `ON CONFLICT (memo_id) DO NOTHING` — statistically irrelevant in practice
- Previous approach (deterministic `sha256(c_address)[:8]`) was superseded — see ADR in checklist.md

### Intent Lifecycle
Each call to `POST /intents` creates one intent:
```
pending → completed   (deposit arrived and was forwarded successfully)
        → expired     (TTL passed before deposit arrived)
        → failed      (all retries exhausted)
```
Intents expire via `ExpireStaleIntents`, called by the retry worker on every tick (every 30s).

Whether a deposit is in time is judged by **when it landed on-chain**, not when the relayer processes it, and not by the intent's status:
- A payment made inside the window is credited even if it is processed late, for example when the stream replays after a restart or a pool is backlogged.
- In that case an intent already flipped to `expired` becomes `completed`.
- A payment that lands after `expires_at` is swept to recovery.

**TTL: 1 hour by default, on purpose for launch.** Callers set `expires_in` (seconds) per flow. One hour may be too short for exchange withdrawals, which can be held for review for hours, and for some on-ramps. Revisit the default once real settlement times per integrated provider are known. A longer TTL is safe because memo_ids are random and unguessable.

### Database Model
Three tables:

**`intents`** — one row per funding session
```
id           uuid        (primary key)
memo_id      bigint      (unique — the on-chain tag)
c_address    text        (Soroban C-address to forward to)
pool_address text        (which pool G-address to watch)
expected_amt text        (optional — for reconciliation)
expires_at   timestamptz (default 1 hour from creation)
status       text        (pending | completed | expired | failed)
external_id  text        (optional — e.g. MoonPay transaction ID)
created_at   timestamptz
updated_at   timestamptz
```

**`forwards`** — one row per inbound payment event (audit log + retry queue)
```
id           bigserial
tx_hash      text        (idempotency key — ON CONFLICT DO NOTHING)
memo_id      bigint
from_address text
amount       text
asset        text
forward_tx   text        (outbound tx hash on success)
status       text        (pending | done | failed | pending_retry)
retries      int
error        text
created_at   timestamptz
updated_at   timestamptz
```

**`cursors`** — one row per pool address
```
pool_address text        (primary key)
cursor       text        (last processed Horizon paging_token)
updated_at   timestamptz
```

### API Surface
- `POST /intents` — called by latch-api to create a funding session. Accepts `c_address`, optional `expected_amt`, `external_id`, and `expires_in` (seconds). Returns `intent_id`, `memo_id`, `pool_address`, `expires_at`.
- `GET /deposit/status/{memo_id}` — returns the intent (status, expires_at, c_address) plus all forward records for that memo_id. Polled by latch-api to surface deposit state to the user.
- `GET /health` — liveness probe

### Who Calls the Relayer
- **latch-api** calls `POST /intents` when a user initiates a funding session
- **latch-api** calls `GET /deposit/status/{memo_id}` to check forward state
- No other service calls the relayer directly

### Forwarding: Soroban SAC Transfer (not classic Payment)
Classic `txnbuild.Payment` validates the destination has a G-address version byte — it rejects C-addresses. C-addresses are Soroban contracts and must be paid via the native XLM Stellar Asset Contract's `transfer` function.

Implementation: `txnbuild.NewPaymentToContract` builds an `InvokeHostFunction` operation that calls `SAC.transfer(from, to, amount)`.

**Soroban fee model**: Soroban transactions are rejected if `tx.fee < inclusionFee + resourceFee`. `MinBaseFee` alone is far too low (100 stroops vs ~5,000,000 resource fee). The relay uses simulate → apply → sign → submit:
1. Build tx with placeholder fee
2. `SimulateTransaction` via Stellar RPC → get real footprint + `MinResourceFee`
3. Apply simulation result to XDR (`SorobanData` ext + updated fee)
4. Re-parse, sign, record the hash on the forward row, submit via Stellar RPC `sendTransaction`, then poll `getTransaction` (see Idempotency)

**Soroban transactions do not support memos** — the outbound forwarding tx carries no memo. Traceability is via the `forwards` table (`tx_hash` → `forward_tx`).

### Fee Bump
Not needed. The relay controls the pooled G-address and is both the signer and fee payer. Fees come from the pool's operational balance (which already holds the deposited funds). The "zero pre-balance" goal is met — the user's C-address receives the full amount.

### Watcher
- **Horizon SSE stream** — one goroutine per pooled address
- `StreamPayments` with `join=transactions` embeds memo data per event (no extra HTTP call per payment)
- On startup: read last saved cursor from DB, reconnect from that point — no missed events on restart
- On any stream error: 5-second backoff then reconnect from last cursor
- Each deposit is written to `forwards` before its cursor is saved; if that write fails the stream stops and replays from the last saved cursor, so a deposit is never skipped without a record
- wallet-backend is NOT used for the deposit hot path

### Multiple Pooled Addresses
Config supports `POOL_ADDRESS_N` / `POOL_PRIVATE_KEY_N` for N pool accounts. One watcher goroutine per pool. Intent creation round-robins across the pools (`internal/handler/handler.go`). Each pool account still lands at most one transaction per ledger (Stellar Core holds one pending transaction per source account), so scaling throughput with channel accounts rather than more pool keys is tracked in #48.

### Idempotency
Incoming deposit `tx_hash` is unique on Stellar. `INSERT ... ON CONFLICT (tx_hash) DO NOTHING` ensures replaying the same SSE event is safe.

The outbound transfer is guarded separately. The signed transaction's hash and max time are written to `forwards.submitted_tx` and `submitted_until` **before** it is sent.

After that point, an unknown outcome is never retried by building a new transfer. That covers a send error, a poll timeout and a crash. The retry worker looks the recorded hash up first:
- `SUCCESS` marks the forward done.
- `FAILED` closes the forward if the cause is permanent, otherwise rebuilds it.
- `NOT_FOUND` waits until RPC has ingested a ledger past the max time, then rebuilds. If RPC's history no longer covers the submission, the forward is failed for manual reconciliation instead.

Forwards are valid for 2 minutes, so an unresolved one is settled within a few minutes. See #50.

### Retry Strategy
```
SSE stream → dispatch goroutine (non-blocking)
                 ↓
            3 quick attempts (0.5s → 1s → 2s backoff)
                 ↓ (if all fail)
            mark forward pending_retry, mark intent remains pending
                 ↓
            retry worker (every 30s) → one final attempt
                 ↓
            success: mark forward done + intent completed
            failure: mark forward failed + intent failed
```

### Accepted Assets
XLM and Circle's USDC only, by default for the configured network (`ACCEPTED_ASSETS` overrides). A deposit in any other asset is swept to recovery with reason `unsupported asset …`. The pools and the recovery account need an authorized trustline for each accepted issued asset. The relayer logs any that are missing at startup.

### What Counts as a Deposit
- `payment`, `path_payment_strict_receive` and `path_payment_strict_send` into a pool. For path payments the credited amount and asset are the destination side: what the pool actually received.
- Anything else that credits a pool (an `account_merge` into it, or a Soroban transfer to it) cannot be attributed. It is logged as an error and counted in `relayer_unhandled_credits_total{kind}`, never skipped silently.
- Deposits are keyed by transaction hash. A transaction with several operations keys each payment `hash:position`, so a batched transaction paying the pool more than once credits every payment.

### Routing Tag: Muxed Address or Memo
A payment to a muxed address `M…(pool, id)` is routed by its muxed id, which is the protocol-native form of "address + memo". Intents are still issued as pool G-address + memo, because many exchanges reject M-addresses.
- If both a muxed id and a numeric memo are present and they disagree, the deposit is swept rather than guessed.
- A non-numeric memo alongside a muxed id (an exchange's own reference) is ignored.

### Accepted Memo Types
`memo.ParseID` accepts **both `MEMO_ID` and `MEMO_TEXT`**, provided the value parses as a `uint64`.

Text is accepted because on-ramps and wallet UIs routinely send the tag as text even when the digits are a valid uint64 — MoonPay documents the XLM tag only as "alpha-numeric" — and a memo refused here is swept to recovery instead of credited to the depositor. The same applies to a human picking "Text" rather than "ID" in their sending wallet, which is the likeliest way a real deposit is lost.

Verified: Horizon renders a text memo as the raw UTF-8 string in `memo` (the base64 form is a separate `memo_bytes` field), so the digits reach `ParseUint` verbatim. Memo IDs are random uint64s, so a text memo colliding with a live intent is not a realistic concern.

`hash` and `return` memos are still rejected — those are binary, and treating base64 as a tag would be meaningless.

### Missing / Invalid Memo Handling
- No memo, or a `hash`/`return` memo → sweep to recovery G-address (`ErrUnsupportedMemoType`)
- `id`/`text` memo whose value is not a `uint64` → sweep to recovery (`ErrInvalidMemoID`)
- Unknown `memo_id` (not in intents table) → sweep to recovery
- Expired intent → sweep to recovery
- A failed intent lookup (database error) is **not** treated as an unknown memo; the forward is retried instead of swept.

How a sweep is recorded (#32):
- The decision is persisted first (`forwards.sweep = true`), so a retry after a crash finishes the sweep instead of deciding again.
- The recovery payment goes through the same pipeline as a forward: the pool's sequencer and send lock, recorded before sending, sent via Stellar RPC and resolved by hash. Its memo is the inbound transaction hash (`MEMO_HASH`), so the recovery account can be reconciled against deposits.
- Status becomes `swept`, with `forward_tx` set to the sweep's hash, **only once the payment has landed**.
- For issued assets, the recovery account's trustline is checked first. Without an authorized trustline nothing is sent (so no fee is burned); the sweep waits in `pending_retry` and goes through on the first tick after the trustline is added.
- Transient failures retry through the retry worker. Permanent ones fail with `not swept, funds remain in pool: …`, never with a message claiming the funds were swept.

### Architecture Split
- **Relayer** owns the deposit hot path: Horizon SSE → memo parse → intent lookup → forward
- **wallet-backend** owns the query path: balances, transaction history
- **latch-api** owns intent creation and user-facing deposit status
