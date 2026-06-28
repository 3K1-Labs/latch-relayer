# wallet-backend

## What This Service Is

`wallet-backend` is **not a clone of the Freighter backends** — it is a completely separate, upstream service that `stellar-freighter-backend-v2` depends on.

Think of the relationship like this:
```
Freighter Wallet (browser extension)
        ↓
stellar-freighter-backend-v2   ← REST API, the middleman
        ↓
wallet-backend                 ← GraphQL API, the data layer
        ↓
Stellar Blockchain (RPC + Horizon)
```

`wallet-backend` is a **reusable Stellar ledger indexer**. It watches the Stellar blockchain, ingests every transaction and ledger change, stores everything in a PostgreSQL database, and exposes a GraphQL API for querying that data. Any wallet app (not just Freighter) could use it.

`stellar-freighter-backend-v2` uses it as its source of truth for account balances and transaction history.

---

## Language & Stack

**Language:** Go
**API style:** GraphQL (single `/graphql` endpoint, schema generated with `gqlgen`)
**CLI framework:** Cobra — multiple subcommands (serve, ingest, migrate, etc.)
**Database:** PostgreSQL — stores indexed ledger data
**Cache:** Redis
**Blockchain:** Stellar RPC + Horizon

Key libraries:
- `gqlgen` — generates Go GraphQL server code from a schema
- `stellar/go` — official Stellar blockchain SDK
- `golang-jwt/jwt` — Ed25519 JWT authentication for clients

---

## Folder Structure

```
wallet-backend/
├── cmd/                        ← CLI subcommands
│   ├── serve/                  ← start the GraphQL API server
│   ├── ingest/                 ← start the ledger ingestion process
│   ├── migrate/                ← run database migrations
│   ├── protocol_setup/         ← set up protocol data
│   ├── protocol_migrate/       ← migrate protocol data
│   └── loadtest/               ← load testing utilities
├── internal/
│   ├── serve/                  ← GraphQL server and resolvers
│   ├── ingest/                 ← ledger ingestion logic
│   ├── indexer/                ← ledger scanning and processing
│   ├── data/                   ← protocol-specific data models
│   ├── db/                     ← database access layer (PostgreSQL)
│   ├── services/               ← account token cache, contract validation, metadata
│   └── entities/               ← domain models
└── pkg/
    ├── wbclient/               ← Go client library for callers (used by freighter-backend-v2)
    ├── sorobanauth/            ← Ed25519 JWT signing utilities
    └── utils/                  ← shared utilities
```

The `pkg/wbclient/` package is significant — it's a ready-made Go client library that `stellar-freighter-backend-v2` imports directly to call this service's GraphQL API without hand-writing queries.

---

## What It Does

**Two main processes run separately:**

### 1. Ingester (`cmd/ingest`)
Connects to Stellar RPC, subscribes to ledger closes, and processes every transaction and state change as it happens. Stores account balances, transaction history, operations, and contract state changes in PostgreSQL. This is the "always running" background process that keeps the database up to date.

### 2. API Server (`cmd/serve`)
Exposes a GraphQL API over the ingested data. Clients (like `stellar-freighter-backend-v2`) query it to get account balances, transaction history, and operations. Authentication is Ed25519 JWT — clients sign their requests so wallet-backend knows which app is calling.

---

## GraphQL API

Single endpoint: `POST /graphql`

The schema covers:
- Account balances (all assets held by an address)
- Transaction history (with pagination)
- Operations (payments, path payments, contract calls, etc.)
- State changes (contract storage diffs)
- Asset metadata

Clients authenticate by sending a JWT signed with their Ed25519 private key. wallet-backend verifies it against a list of authorised public keys (`CLIENT_AUTH_PUBLIC_KEYS`).

---

## Environment Variables

From `.env.example`:

| Variable | Purpose |
|---|---|
| `CLIENT_AUTH_PUBLIC_KEYS` | Comma-separated Ed25519 public keys of authorised callers |
| PostgreSQL connection vars | Database host, port, name, user, password |
| Redis connection vars | Host, port, password |
| Stellar RPC URL | For ledger ingestion |
| Horizon URL | Legacy fallback |

---

## How It's Used In Production

1. **Two separate processes** — the ingester and the API server run independently (can be scaled separately)
2. **PostgreSQL is the source of truth** — all indexed ledger data lives here
3. **Redis** — used for caching frequently accessed account data
4. **JWT-authenticated** — only services with a registered public key can call the GraphQL API
5. **`stellar-freighter-backend-v2` is a client** — it imports `pkg/wbclient`, signs JWTs with its private key, and calls this service for account balances and transaction history
6. **Forked by 3000-Labs** — the repo at `/Users/user/SuperFranky/Backends/wallet-backend` is a fork of `github.com/stellar/wallet-backend` (the upstream Stellar Foundation project)

---

## External Services

| Service | Purpose |
|---|---|
| **Stellar RPC** | Ledger ingestion — receives new ledger data in real time |
| **Stellar Horizon** | Legacy fallback for historical data |
| **PostgreSQL** | Primary data store for all indexed blockchain data |
| **Redis** | Account data cache |

---

## Relationship Summary

| | wallet-backend | stellar-freighter-backend-v2 | stellar-freighter-backend (v1) |
|---|---|---|---|
| Role | Data layer | Application layer | Application layer |
| Language | Go | Go | TypeScript |
| API style | GraphQL | REST | REST |
| Data source | Stellar RPC (direct) | wallet-backend | Mercury indexer |
| Used by | freighter-backend-v2 | Freighter wallet | Freighter wallet |
| Is a clone? | No — standalone service | No — depends on wallet-backend | No — separate v1 system |
