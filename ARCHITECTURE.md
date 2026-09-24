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
4. Re-parse, sign, submit via Horizon

**Soroban transactions do not support memos** — the outbound forwarding tx carries no memo. Traceability is via the `forwards` table (`tx_hash` → `forward_tx`).

### Fee Bump
Not needed. The relay controls the pooled G-address and is both the signer and fee payer. Fees come from the pool's operational balance (which already holds the deposited funds). The "zero pre-balance" goal is met — the user's C-address receives the full amount.

### Watcher
- **Horizon SSE stream** — one goroutine per pooled address
- `StreamPayments` with `join=transactions` embeds memo data per event (no extra HTTP call per payment)
- On startup: read last saved cursor from DB, reconnect from that point — no missed events on restart
- On any stream error: 5-second backoff then reconnect from last cursor
- wallet-backend is NOT used for the deposit hot path

### Multiple Pooled Addresses
Config supports `POOL_ADDRESS_N` / `POOL_PRIVATE_KEY_N` for N pool accounts. One watcher goroutine per pool. Currently intent creation always picks `PoolAccounts[0]` — round-robin or least-loaded assignment is a future improvement.

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
- All cases logged in `forwards` table with status `failed`

### Architecture Split
- **Relayer** owns the deposit hot path: Horizon SSE → memo parse → intent lookup → forward
- **wallet-backend** owns the query path: balances, transaction history
- **latch-api** owns intent creation and user-facing deposit status
