# stellar-freighter-backend-v2

## Language & Stack

**Language:** Go 1.25.9
**CLI framework:** Cobra — structures the app as a command with subcommands (`serve`)
**Config management:** Viper — reads from a TOML file, environment variables, and CLI flags all at once
**HTTP server:** Go standard library `net/http` with no external framework
**Build:** Multi-stage Docker build (Go compiler → slim Debian runtime image)

Key libraries:
- `stellar/wallet-backend` — upstream service this backend depends on for account data (see `wallet-backend.md`)
- `stellar/go` + `stellar/go-stellar-sdk` — official Stellar blockchain SDKs
- `golang-jwt/jwt` — verifies Ed25519 JWT tokens for authentication
- `redis/go-redis` — caching for token prices
- `prometheus/client_golang` — exposes metrics for monitoring
- `spf13/cobra` + `spf13/viper` — CLI and config
- `testcontainers/testcontainers-go` — runs real Docker containers during integration tests

---

## What This Service Is

This is **Freighter's next-generation backend, rewritten in Go** (v1 was TypeScript — see `stellar-freighter-backend.md`). It serves the same purpose — sitting between the Freighter wallet and external services — but with a cleaner architecture and different data sources.

The key shift from v1:
- **Drops Mercury indexer** in favour of the `wallet-backend` service for account data
- **Drops background price workers** in favour of on-demand price fetching from Stellar Expert
- **Adds stateless JWT authentication** (Ed25519 signatures)
- **Rewrites everything in Go** for better performance and simpler deployment (single compiled binary)

---

## README Summary

The README focuses on the **release process**. Two-step flow:

1. **Cut a prerelease** — triggers a GitHub Action that builds a Docker image, pushes it to staging ECR, and creates a GitHub prerelease tag
2. **Promote to release** — re-tags the staging image to production (no rebuild, same exact binary), creates a final GitHub release

All releases are cut from `main`. Version format: `v1.2.3` for releases, `v1.2.3-rc.1` for prereleases.

---

## Configuration

Config lives in a TOML file (`.toml-EXAMPLE`), environment variables, or CLI flags — all three sources are merged by Viper.

| Key | Purpose |
|---|---|
| `FREIGHTER_BACKEND_PORT` / `HOST` | API server address |
| `MODE` | `development` or `production` |
| `SENTRY_KEY` | Sentry error tracking DSN |
| `PUBNET_RPC_URL` / `TESTNET_RPC_URL` / `FUTURENET_RPC_URL` | Stellar RPC endpoints per network |
| `HORIZON_PUBNET_URL` / `HORIZON_TESTNET_URL` | Horizon (legacy, optional) |
| `REDIS_HOST` / `REDIS_PORT` / `REDIS_PASSWORD` | Redis connection |
| `BLOCKAID_API_KEY` | Blockaid security scanning key |
| `USE_BLOCKAID_DAPP_SCANNING` / `USE_BLOCKAID_TX_SCANNING` / etc. | Toggle each Blockaid scan type |
| `COINBASE_API_KEY` / `COINBASE_API_SECRET` | Coinbase onramp |
| `STELLAR_EXPERT_PUBNET_URL` / `TESTNET_URL` / `API_KEY` | Token price source |
| `PRICE_CACHE_TTL_SECONDS` | How long to cache prices in Redis |
| `PRICE_FETCH_TIMEOUT_SECONDS` | Max time to spend fetching uncached prices |
| `MAX_TOKENS_PER_REQUEST` | Cap on tokens per `/token-prices` call |
| `MAX_CONCURRENT_PRICE_FETCHES` | Concurrent fetches per request |
| `MAX_CONCURRENT_RPC_CALLS` | Concurrent RPC calls for collectibles (default: 10) |
| `MERIDIAN_PAY_*` | NFT collection addresses for collectibles |

Wallet Backend connection config (set per network):
- URL to the wallet-backend service
- Ed25519 signing key used to generate JWTs that authenticate this service to wallet-backend

---

## Folder Structure

```
stellar-freighter-backend-v2/
├── main.go                        ← binary entrypoint
├── go.mod / go.sum                ← Go module dependencies
├── Makefile                       ← build, test, lint targets
├── configs/
│   └── .toml-EXAMPLE             ← all config keys with descriptions
├── deployments/
│   ├── Dockerfile                 ← multi-stage build
│   └── docker-compose.yml        ← app + Redis for local dev
├── docs/                          ← design docs
├── scripts/                       ← build/utility scripts
├── .github/workflows/             ← CI/CD (lint, test, publish, promote)
├── cmd/
│   ├── root.go                    ← registers subcommands
│   └── serve/
│       └── serve.go               ← serve subcommand + all 50+ CLI flags
└── internal/
    ├── api/
    │   ├── serve.go               ← ApiServer struct; starts both HTTP servers
    │   ├── handlers/              ← one file per route (27 handler files)
    │   ├── middleware/            ← auth, logging, metrics, body size limit
    │   ├── httperror/             ← structured JSON error responses
    │   └── httpresponse/          ← response wrapper utilities
    ├── auth/                      ← Ed25519 JWT verification
    ├── services/
    │   ├── rpc.go                 ← Stellar RPC client (simulate, ledger entries)
    │   ├── wallet_backend.go      ← account balances + history via wallet-backend
    │   ├── prices.go              ← token prices via Stellar Expert + Redis cache
    │   └── stellar_expert.go      ← HTTP client for Stellar Expert API
    ├── config/                    ← config structs
    ├── store/
    │   └── redis.go               ← Redis wrapper
    ├── types/                     ← interfaces + response DTOs
    ├── logger/                    ← slog wrapper
    ├── metrics/                   ← Prometheus metric definitions
    ├── utils/                     ← utilities (asset ID parsing, etc.)
    └── integrationtests/          ← Docker-based integration tests
```

---

## Entrypoint — `main.go` → `cmd/serve/serve.go`

Here is exactly what happens when the app starts:

1. **`main.go`** — creates the Cobra root command and calls `.Execute()`
2. **`cmd/root.go`** — registers the `serve` subcommand
3. **`cmd/serve/serve.go`** — this is where all 50+ config flags are defined. It reads config from TOML file (searched in current dir, `~/.config/freighter-backend`, `/etc/freighter-backend`), then env vars, then CLI flags. Validates the config.
4. **`internal/api/serve.go`** — `ApiServer.Start()` initialises all services (Redis, RPC, Wallet Backend, Prices), registers all routes and middleware, then starts two HTTP servers:
   - **API server** on `HOST:PORT` (default `localhost:3002`)
   - **Metrics server** on `localhost:9090` for Prometheus

No background worker threads — all data is fetched on-demand (prices are cached in Redis after first fetch).

---

## API Routes

All routes under `/api/v1/`:

| Method | Route | What it does |
|---|---|---|
| `GET` | `/api/v1/ping` | Liveness health check |
| `GET` | `/api/v1/rpc-health` | Checks Stellar RPC per network |
| `GET` | `/api/v1/feature-flags` | Runtime feature flags for the wallet |
| `GET` | `/api/v1/protocols` | List of supported protocols |
| `GET` | `/api/v1/auth/whoami` | JWT validation test (auth-gated) |
| `POST` | `/api/v1/token-prices` | Prices for a list of tokens (Redis-cached) |
| `POST` | `/api/v1/accounts/balances` | Account balances (via wallet-backend) |
| `POST` | `/api/v1/accounts/{address}/transactions` | Transaction history with embedded operations |
| `POST` | `/api/v1/collectibles` | NFT/collectible assets |
| `POST` | `/api/v1/ledger-key/accounts` | Raw ledger entries from Stellar RPC |
| `GET` | `/metrics` | Prometheus metrics (port 9090) |

**Limits enforced per request:**
- Token prices: max 1,000 tokens
- Ledger key accounts: max 100 keys (RPC upstream caps at 200)
- Account balances: max 100 addresses
- Account history: default 20 per page, max 100
- Request body: max 1MB

---

## Key Services Explained

### Wallet Backend Service (`internal/services/wallet_backend.go`)
This wraps the `wallet-backend` client library. When the wallet asks for account balances or transaction history, this service calls wallet-backend's GraphQL API using a JWT it signs with its Ed25519 key. It fans out requests across multiple addresses concurrently.

### Prices Service (`internal/services/prices.go`)
Token prices come from the **Stellar Expert API** (not Mercury or on-chain pathfinding like v1). When a price is requested, it checks Redis first. If not cached, it fetches from Stellar Expert concurrently across tokens, then stores results in Redis with a configurable TTL. No background worker — prices are pulled on-demand.

### RPC Service (`internal/services/rpc.go`)
Wraps the Stellar RPC client. Used for transaction simulation (dry-runs before signing), fetching raw ledger entries, and contract invocations. Supports all three networks (pubnet, testnet, futurenet).

### Authentication (`internal/auth/`)
Stateless Ed25519 JWT verification. The token is bound to the HTTP method + path + body hash — so a token generated for one request can't be replayed on a different one. Two modes:
- **Permissive** — no token is fine, but an invalid token is rejected
- **Strict** — a valid token is required

---

## How It's Used In Production

1. **Single compiled binary** — `./freighter-backend serve` — no Node runtime, no webpack bundles
2. **Docker container** — built with multi-stage Dockerfile; runs as non-root user `freighter`
3. **Redis required** — for price caching; checked on startup
4. **wallet-backend required** — must be reachable for account data routes
5. **Two-step release** — prerelease to staging ECR → promote same image digest to production (no rebuild)
6. **Prometheus scrapes `:9090/metrics`** — tracks HTTP latency, RPC/wallet-backend/price service latencies, error rates
7. **Sentry** captures unhandled errors
8. **80% test coverage enforced in CI** — integration tests spin up real Docker containers

---

## External Services

| Service | Purpose |
|---|---|
| **Stellar RPC** | Transaction simulation, ledger entries, contract calls |
| **Stellar Horizon** | Legacy account data (optional, backcompat) |
| **wallet-backend** | Account balances and transaction history (JWT-authenticated) |
| **Stellar Expert** | Token price data and 24h candle data |
| **Blockaid** | Security scanning for dApps, transactions, assets |
| **Coinbase Onramp** | Fiat-to-crypto buy session generation |
| **Redis** | Token price cache |
| **Sentry** | Error tracking |
| **Prometheus** | Metrics (scraped externally) |

---

## How v2 Differs From v1

| | v1 (TypeScript) | v2 (Go) |
|---|---|---|
| Language | TypeScript / Node.js | Go |
| HTTP framework | Fastify | Standard `net/http` |
| Config | `.env` file | TOML + env + CLI flags (Viper) |
| Account data source | Mercury indexer | wallet-backend GraphQL API |
| Token prices source | On-chain pathfinding (worker thread) | Stellar Expert API (on-demand + Redis cache) |
| Background workers | Price worker + integrity checker | None |
| Auth | None documented | Ed25519 JWT (stateless) |
| Deployment artifact | Node.js + webpack bundles | Single Go binary in Docker |
| Test framework | Jest | Go test + testcontainers |
