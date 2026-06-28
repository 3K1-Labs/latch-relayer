# Latch Relayer — Build Log

## Project Overview
The latch-relayer is a Go microservice that bridges the standard Stellar deposit UX (G-address + memo) to Soroban smart contract addresses (C-addresses). It is part of the Latch wallet infrastructure — Deliverable 3 of the Latch grant.

**Notion page:** Deliverable 3: G→C Bridge Protocol
**Repo:** github.com/3000-Labs/latch-relayer
**Module path:** github.com/latch/relayer

---

## System Architecture (three tiers)
```
wallet-backend          ← data layer: Stellar ledger indexer, GraphQL API
latch-api + relayer     ← middleware: REST APIs, deposit bridging
extension / mobile      ← user-facing: Freighter wallet extension / mobile app
```
The relayer is a separate microservice alongside latch-api, not embedded in it.

---

## Architecture Decisions
See ARCHITECTURE.md for the full spec. Summary:

| Decision | Choice |
|---|---|
| Language | Go |
| Memo ID | `uint64(sha256(c_address)[:8])`, collision rejected at registration |
| DB loss recovery | Accept risk — user re-registers from device on next open |
| Pooled addresses | Multiple (e.g. 10), round-robin at registration |
| Watcher | Horizon SSE stream per pool address, cursor saved in DB |
| Data source | Relayer → Horizon (deposit hot path). latch-api → wallet-backend (query path) |
| Fee bump | Not needed — relay pays fee directly from pooled G-address balance |
| Fee strategy | Dynamic — fetch network base fee before each forward |
| Idempotency | `tx_hash` checked before processing |
| Retry | 3 quick in-goroutine retries → `pending_retry` in DB → background retry worker every 30s |
| Bad memo | Sweep to recovery address, log for manual review |
| Memo prevention | Wallet UI always pairs pooled address + memo_id |

---

## Folder Structure
```
latch-relayer/
├── cmd/serve/main.go              ← binary entrypoint
├── internal/
│   ├── config/config.go           ← env vars + config struct
│   ├── db/db.go                   ← postgres connection + migrations
│   ├── handler/handler.go         ← HTTP handlers
│   ├── memo/memo.go               ← memo_id derivation (sha256 → uint64)
│   ├── service/
│   │   ├── watcher/watcher.go     ← Horizon SSE stream per pool address
│   │   ├── forwarder/forwarder.go ← builds + submits payment tx
│   │   └── retry/retry.go         ← background retry worker (every 30s)
│   └── store/store.go             ← DB queries (registrations, forwards, cursors)
├── migrations/                    ← SQL files
├── scripts/                       ← dev utilities (keygen, seed, etc.)
├── go.mod
├── .env                           ← local secrets (gitignored)
├── .env.example                   ← template
├── ARCHITECTURE.md                ← full architecture spec
└── BUILD_LOG.md                   ← this file
```

---

## Database Tables

### `registrations`
```sql
memo_id      BIGINT PRIMARY KEY       -- derived: uint64(sha256(c_address)[:8])
c_address    TEXT NOT NULL            -- Soroban contract address (C...)
pool_address TEXT NOT NULL            -- which pooled G-address this user is assigned to
created_at   TIMESTAMPTZ DEFAULT NOW()
```

### `forwards`
```sql
id           UUID PRIMARY KEY DEFAULT gen_random_uuid()
memo_id      BIGINT NOT NULL
tx_hash      TEXT NOT NULL UNIQUE     -- idempotency key (incoming deposit tx hash)
amount       TEXT NOT NULL
asset        TEXT NOT NULL            -- e.g. "native" or "USDC:GABC..."
status       TEXT NOT NULL            -- pending | forwarded | failed | pending_retry | unknown_memo
created_at   TIMESTAMPTZ DEFAULT NOW()
updated_at   TIMESTAMPTZ DEFAULT NOW()
```

### `cursors`
```sql
pool_address TEXT PRIMARY KEY
cursor       TEXT NOT NULL            -- last processed Horizon paging_token
updated_at   TIMESTAMPTZ DEFAULT NOW()
```

---

## API Endpoints

| Method | Route | Who calls it | Purpose |
|---|---|---|---|
| `POST` | `/register` | latch-api | Register C-address → assign memo_id + pool_address |
| `GET` | `/deposit/status/:memo_id` | wallet / latch-api | Check if deposit was forwarded |
| `GET` | `/health` | infra | Liveness probe |

### POST /register
Request:
```json
{ "c_address": "C..." }
```
Response:
```json
{ "memo_id": 3891273648, "pool_address": "G..." }
```

### GET /deposit/status/:memo_id
Response:
```json
{
  "memo_id": 3891273648,
  "status": "forwarded",
  "tx_hash": "abc123...",
  "amount": "10.0000000",
  "asset": "native",
  "forwarded_at": "2026-06-28T10:00:00Z"
}
```

---

## How the Watcher Works (Horizon SSE)

Each pooled G-address gets its own goroutine:
```
goroutine per pool address:
  1. Read last cursor from DB (or cursor=0 for first run)
  2. Open SSE stream: GET /accounts/{pool_address}/payments?cursor={cursor}&order=asc
  3. For each payment event:
     a. Parse memo → validate it's a uint64
     b. Look up memo_id in registrations table → get c_address
     c. If not found → sweep to recovery address, log as unknown_memo
     d. If found → dispatch forward goroutine (non-blocking)
     e. Save cursor to DB
  4. On disconnect → reconnect after 2s backoff
```

Forward goroutine:
```
1. Check tx_hash in forwards table → skip if already exists (idempotency)
2. Fetch current network base fee (dynamic)
3. Build payment tx: pooled G-address → C-address
4. Sign with pooled address private key
5. Submit to Stellar RPC
6. Attempt 3 times (0.5s, 1s, 2s backoff)
7. On success → mark forwarded in DB
8. On all attempts fail → mark pending_retry in DB
```

Retry worker (single goroutine, runs every 30s):
```
1. SELECT * FROM forwards WHERE status = 'pending_retry'
2. For each → attempt forward again
3. After N total attempts → mark failed
```

---

## Memo ID Derivation
```go
// memo/memo.go
func DeriveID(cAddress string) uint64 {
    hash := sha256.Sum256([]byte(cAddress))
    return binary.BigEndian.Uint64(hash[:8])
}
```

---

## Key Dependencies (to add)
- `github.com/stellar/go-stellar-sdk` — Stellar SDK (keypairs, transactions, Horizon client)
- `github.com/gin-gonic/gin` — HTTP server (consistent with latch-api)
- `github.com/jackc/pgx/v5` — PostgreSQL driver
- `github.com/joho/godotenv` — .env loading
- `github.com/google/uuid` — UUIDs for forwards table

---

## Environment Variables (.env.example)
```
# Network
NETWORK=testnet
HORIZON_URL=https://horizon-testnet.stellar.org
RPC_URL=https://soroban-testnet.stellar.org

# Pooled G-addresses (comma-separated or indexed)
POOL_ADDRESS_1=G...
POOL_PRIVATE_KEY_1=S...

# Recovery address (for unknown/missing memos)
RECOVERY_ADDRESS=G...

# Database
DATABASE_URL=postgres://user:pass@localhost:5432/latch_relayer

# Server
PORT=4000
```

---

## Build Plan (implementation order)

### ✅ Done
- Architecture spec (ARCHITECTURE.md)
- Folder scaffold + go.mod initialized
- Stellar reference docs saved

### 🔄 In Progress
- **Entry 1:** Create two testnet keypairs — hot/pooled account + depositor test account. Fund via Friendbot. Store in .env.

### ⬜ Next Up
- **Entry 2:** `internal/memo/memo.go` — implement DeriveID (sha256 → uint64)
- **Entry 3:** `internal/config/config.go` — load all env vars into typed Config struct
- **Entry 4:** `internal/db/db.go` — postgres connection pool (pgx)
- **Entry 5:** `migrations/` — create SQL files for registrations, forwards, cursors tables
- **Entry 6:** `internal/store/store.go` — DB query functions (insert registration, lookup by memo_id, upsert cursor, insert/update forward)
- **Entry 7:** `internal/handler/handler.go` — POST /register, GET /deposit/status/:memo_id, GET /health
- **Entry 8:** `internal/service/forwarder/forwarder.go` — build + sign + submit payment tx, 3-retry logic
- **Entry 9:** `internal/service/watcher/watcher.go` — Horizon SSE stream, memo parse, dispatch forwarder
- **Entry 10:** `internal/service/retry/retry.go` — background retry worker (ticker every 30s)
- **Entry 11:** `cmd/serve/main.go` — wire everything together, start HTTP server + watchers
- **Entry 12:** End-to-end testnet test — deposit from test account → verify C-address receives funds

---

## Useful References
- Horizon SSE payments stream: `GET /accounts/{address}/payments?cursor={cursor}&order=asc&limit=200`
- Friendbot (testnet funder): `https://friendbot.stellar.org/?addr={public_key}`
- Stellar Go SDK keypair package: `github.com/stellar/go-stellar-sdk/keypair`
- Stellar Go SDK txnbuild (transaction building): `github.com/stellar/go-stellar-sdk/txnbuild`
- Horizon testnet: `https://horizon-testnet.stellar.org`
- Soroban RPC testnet: `https://soroban-testnet.stellar.org`
- latch-api reference (same patterns): `github.com/3000-Labs/latch-api`
