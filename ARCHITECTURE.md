# Latch Relayer — Architecture Decisions

## What It Is
An off-chain Go service that bridges the standard Stellar deposit UX (G-address + memo) to Soroban smart contract addresses (C-addresses). It watches a single pooled G-address, parses incoming payment memos, and forwards funds to the correct C-address via fee-bumped transactions.

---

## Decisions Made

### Language
Go — consistent with wallet-backend and stellar-freighter-backend-v2.

### Memo ID Generation
- Deterministic from C-address: `uint64(sha256(c_address)[:8])`
- One-way derivation — relay still needs a DB index for the reverse lookup (memo_id → C-address)
- The DB is a **rebuildable index**, not a source of truth — latch-api knows all C-addresses
- Collision detection at registration time: if memo_id already exists in DB, reject the registration
- uint64 space (~18.4 quintillion) makes collisions astronomically unlikely at any realistic scale

### DB Loss / Recovery
- No bulk re-register endpoint
- If DB is lost and a deposit arrives with an unknown memo_id: hold or sweep to recovery address
- User re-registers from their device on next app open, relay then forwards
- Wallet device caches the memo_id permanently (always re-derivable from C-address)

### Database Model
Two tables:

**`registrations`** — the memo_id → C-address index
```
memo_id    uint64  (primary key, derived from C-address)
c_address  string  
created_at timestamp
```

**`forwards`** — deposit event log (powers the status endpoint)
```
id         uuid
memo_id    uint64
tx_hash    string
amount     string
asset      string
status     string  (pending | forwarded | failed | unknown_memo)
created_at timestamp
```

### API Surface
- `POST /register` — called by latch-api when a Smart Account is created. Accepts C-address, derives memo_id, stores mapping, returns memo_id.
- `GET /deposit/status/:memo_id` — returns forward status for a given memo_id (for the wallet to poll/check)
- `GET /health` — liveness probe

### Who Calls the Relayer
- **latch-api** calls `POST /register` at Smart Account creation time
- **Wallet / latch-api** calls `GET /deposit/status/:memo_id` to check if a deposit was forwarded

### Watcher
- **Horizon SSE stream** — one stream per pooled address, not wallet-backend polling
- Endpoint: `GET /accounts/{pooled_address}/payments?cursor={last_cursor}&order=asc`
- Horizon pushes each payment in real time (~5-7s after ledger close)
- On startup: read last saved cursor from DB, reconnect from that point — no missed events on restart
- wallet-backend is NOT used for the deposit hot path — it handles the query side (balances, history) via latch-api

### Multiple Pooled Addresses
- Single hot account is a concurrency bottleneck — 10,000 deposits processed sequentially = hours of backlog
- Stellar ledger closes every ~5s and can contain thousands of transactions, all arriving as a sequential SSE list
- Solution: a **pool of G-addresses** (e.g. 10), each with its own SSE stream and its own goroutine worker
- Deposits are spread across the pool — 10 parallel forwarders instead of 1
- Registration assigns a `pool_address` alongside a `memo_id` (round-robin or least-loaded)
- User's deposit instructions: pooled G-address + memo_id (e.g. "Send to GABC..., memo: 847291")

### Data Model (updated)
**`registrations`**
```
memo_id      uint64   (primary key, derived from C-address)
c_address    string
pool_address string   (which pooled G-address this user is assigned to)
created_at   timestamp
```

**`forwards`**
```
id           uuid
memo_id      uint64
tx_hash      string   (the incoming deposit tx hash — used for idempotency)
amount       string
asset        string
status       string   (pending | forwarded | failed | unknown_memo)
created_at   timestamp
```

**`cursors`**
```
pool_address string   (primary key)
cursor       string   (last processed Horizon paging_token for this stream)
updated_at   timestamp
```

### Architecture Split
- **Relayer** owns the deposit hot path: Horizon SSE → memo parse → forward
- **wallet-backend** owns the query path: balances, transaction history (consumed by latch-api)
- Two data sources, clear separation of concerns

### Fee Bump
- **Not needed.** The relay controls the pooled G-address and is both the signer and fee payer
- Flow: deposit arrives at pooled G-address → relay builds a regular payment tx FROM pooled G-address TO C-address → signs with pooled key → submits
- Fee is paid from the pooled G-address operational balance (which already holds the deposited funds)
- User never needs XLM — the "zero pre-balance" goal is still met
- Fee strategy: **dynamic** — relay fetches the current network base fee before each forward (handles surge pricing)
- Pooled G-address private keys held in relay config (env vars for testnet; KMS can be considered for mainnet)

### Idempotency
- Incoming deposit `tx_hash` is unique on Stellar — used as the idempotency key
- Before processing any deposit: check if `tx_hash` already exists in `forwards` table
- If yes: skip (handles SSE duplicate delivery on reconnect)

### Retry Strategy
- **Hybrid: immediate retry + retry queue**
- SSE worker per pool dispatches each forward to its own goroutine and moves on immediately — never blocks the stream
- Each goroutine attempts **3 quick retries** with short backoff (0.5s → 1s → 2s)
- If all 3 fail: mark forward as `pending_retry` in DB and exit goroutine — worker is free
- **Separate retry worker** runs on a timer (every 30s), picks up all `pending_retry` rows, retries with longer backoff
- After a final retry threshold: mark as `failed` for manual review
- Flow:
  ```
  SSE stream → dispatch goroutine (non-blocking)
                   ↓
              3 quick attempts (0.5s, 1s, 2s backoff)
                   ↓ (if all fail)
              mark pending_retry in DB
                   ↓
              retry worker (every 30s) → success or failed
  ```

### Missing / Invalid Memo Handling
Three cases: no memo, unknown memo_id (not in DB), invalid memo (not a valid uint64)

**Relay-side (last resort):**
- All three cases → sweep funds to a designated recovery G-address
- Log the event in `forwards` table with status `unknown_memo`, storing tx_hash, amount, asset, and raw memo value
- Ops can review and manually route if needed

**Prevention (earlier in the flow):**
- Wallet UI always generates deposit instructions as a pair: pooled G-address + memo_id — never one without the other
- memo_id is always valid by construction (uint64 derived from C-address), so invalid memos only come from users bypassing the official deposit flow
- Relay validates memo is a parseable uint64 on receipt, before any DB lookup — malformed memos are swept immediately without a lookup attempt
