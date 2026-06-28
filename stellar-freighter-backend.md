# stellar-freighter-backend

## Language & Stack

**Language:** TypeScript (Node.js runtime, v25.3.0+)
**Package manager:** Yarn
**Server framework:** Fastify — a fast, low-overhead HTTP server (similar role to Express)
**Build tool:** Webpack — bundles TypeScript source into plain JS for production

Key libraries:
- `stellar-sdk` — official Stellar blockchain SDK
- `ioredis` / `redis` — connects to Redis for caching and feature flags
- `prom-client` — exposes Prometheus metrics for monitoring
- `@blockaid/client` — security scanning for dApps and transactions
- `@sentry/node` — error tracking
- `jsonwebtoken` — signs JWTs for Coinbase onramp
- `urql` — GraphQL client (used to talk to Mercury indexer)
- `pino` — structured logging

---

## What This Service Is

This is the **backend for the Freighter wallet** — a Stellar browser extension wallet. It sits between the wallet frontend and several external services (the Stellar blockchain, a data indexer called Mercury, a security scanner called Blockaid, and Coinbase's onramp API). The wallet itself never talks to those services directly — it goes through this backend.

Think of it as a middleware layer that:
1. Fetches and formats blockchain data for the wallet
2. Caches expensive data (like token prices) so every user request doesn't hit the chain
3. Scans transactions and assets for security risks before the user signs
4. Lets users buy crypto via Coinbase onramp

---

## README Summary

- Development mode uses an in-memory store — no Redis needed
- Production requires Redis (specifically Redis Stack for time-series support)
- There is a docs/ folder with architecture, runbook, workers, metrics, debugging, and Mercury integration guides
- The `FREIGHTER_TRUST_PROXY_RANGE` env var is critical in production for binding Coinbase onramp sessions to client IP addresses

---

## Environment Variables

Found in `.env-EXAMPLE`:

| Variable | Purpose |
|---|---|
| `AUTH_EMAIL` / `AUTH_PASS` | Mercury indexer credentials (mainnet) |
| `AUTH_EMAIL_TESTNET` / `AUTH_PASS_TESTNET` | Mercury credentials (testnet) |
| `MERCURY_INTEGRITY_CHECK_ACCOUNT_EMAIL/PASS` | Separate account for data validation |
| `REDIS_CONNECTION_NAME` | Named Redis connection |
| `REDIS_PORT` | Redis port |
| `HOSTNAME` | Redis host |
| `MODE` | `production` or `development` |
| `USE_MERCURY` | Toggle Mercury indexer on/off at runtime |
| `SENTRY_KEY` | Sentry DSN for error tracking |
| `BLOCKAID_KEY` | Blockaid API key for security scanning |
| `COINBASE_API_KEY` / `COINBASE_API_SECRET` | Coinbase onramp credentials |
| `FREIGHTER_HORIZON_URL` | Horizon endpoint for token prices |
| `FREIGHTER_RPC_PUBNET_URL` | Soroban RPC mainnet URL |
| `FREIGHTER_TRUST_PROXY_RANGE` | Trusted proxy IP ranges |
| `DISABLE_TOKEN_PRICES` | Turn off the price worker |
| `PRICE_*` | Various price worker tuning settings |

---

## Folder Structure

```
stellar-freighter-backend/
├── src/
│   ├── index.ts                  ← app entrypoint
│   ├── config.ts                 ← builds config object from env vars
│   ├── logger.ts                 ← structured logger with PII redaction
│   ├── route/
│   │   ├── index.ts              ← all 27 API routes (1,562 lines)
│   │   ├── validators.ts         ← request/response JSON schemas
│   │   └── metrics.ts            ← Prometheus middleware
│   ├── service/
│   │   ├── mercury/              ← GraphQL client for Mercury indexer
│   │   ├── prices/               ← token price calculation + worker thread
│   │   ├── blockaid/             ← security scanning service
│   │   └── integrity-checker/    ← validates Mercury data vs Horizon
│   └── helper/
│       ├── horizon-rpc.ts        ← Horizon REST API client
│       ├── soroban-rpc/          ← Soroban contract interaction
│       ├── onramp.ts             ← Coinbase onramp integration
│       └── metrics.ts            ← Prometheus metric definitions
├── docs/                         ← operational documentation
├── Dockerfile
├── compose.yaml                  ← Redis for local dev
├── webpack.prod.js               ← production build config
└── .github/workflows/            ← CI/CD (runs tests on PRs)
```

---

## Entrypoint — `src/index.ts`

This is where the app starts. Here is exactly what happens in order:

1. **Load environment** — reads `.env` file, expands any variable references
2. **Parse CLI args** — accepts `--env` (mode) and `--port` (port override)
3. **Build config** — `config.ts` reads all env vars and assembles one config object
4. **Create Prometheus registry** — sets up the metrics collector
5. **Connect Redis** — only in production; skipped in development
6. **Start API server** — Fastify listens on port `3002` (default)
7. **Start metrics server** — Prometheus endpoint on port `9090`
8. **Spawn worker threads** — only in production:
   - **Price worker** — runs in a separate thread, periodically fetches and caches token prices
   - **Integrity checker worker** — validates that Mercury data matches Horizon; disables Mercury if they diverge
9. **Register shutdown handlers** — cleans up on `SIGTERM` / `SIGINT`

---

## The 27 API Routes

All routes live under `/api/v1/`. Grouped by purpose:

**Health checks**
- `GET /ping` — liveness probe
- `GET /price-worker-health` — checks price cache and Horizon
- `GET /rpc-health?network=` — checks Soroban RPC
- `GET /horizon-health?network=` — checks Horizon

**Account data** (fetched from Mercury or Horizon)
- `GET /account-history/:pubKey?network=` — transaction history
- `GET /account-balances/:pubKey?network=` — all token balances

**Token & contract data** (fetched from Soroban RPC)
- `GET /token-details/:contractId?network=` — name, symbol, decimals
- `GET /token-spec/:contractId?network=` — token interface spec
- `GET /contract-spec/:contractId?network=` — arbitrary contract spec
- `GET /is-sac-contract/:contractId?network=` — checks if it's a Stellar Asset Contract

**Token prices**
- `POST /token-prices` — returns prices for a list of tokens (served from Redis cache)

**Security scanning** (via Blockaid)
- `GET /scan-dapp?url=` — checks if a website is malicious
- `GET /scan-tx?txXdr=&network=&url=` — scans a transaction before signing
- `POST /scan-tx` — same but body-based
- `GET /scan-asset?asset=&network=` — risk assessment on an asset
- `GET /scan-asset-bulk?assets=&network=` — batch asset scan
- `GET /report-asset-warning` — report a bad asset to Blockaid
- `GET /report-transaction-warning` — report a bad transaction to Blockaid

**Mercury subscriptions** (event indexing)
- `POST /subscription/token` — subscribe to token events
- `POST /subscription/account` — subscribe to account events
- `POST /subscription/token-balance` — subscribe to balance changes

**Transaction operations**
- `POST /submit-tx` — submits a signed transaction to the network
- `POST /simulate-tx` — dry-runs a transaction without submitting
- `POST /simulate-token-transfer` — simulates a token transfer specifically

**Coinbase onramp**
- `POST /onramp/token` — generates a session token for buying crypto (IP-bound)

**Feature flags**
- `GET /feature-flags` — returns runtime config flags to the wallet
- `GET /user-notification` — returns any active user-facing notices

---

## Key Services Explained

### Mercury (`src/service/mercury/`)
Mercury is a third-party **Stellar data indexer**. Rather than scanning every block on the chain yourself, Mercury does it and exposes a GraphQL API. This service authenticates with Mercury, subscribes accounts to be watched, and queries indexed data (transaction history, balances, event streams). It's faster than asking Horizon directly for historical data.

### Price Worker (`src/service/prices/`)
Token prices on Stellar are calculated via **pathfinding** — finding the best exchange path between a token and USDC. This is expensive to compute per-request, so a background worker thread runs on a timer, calculates prices for all known tokens in batches, and stores them in **Redis time-series**. When the wallet requests `/token-prices`, it reads from that cache instead of hitting the chain live.

### Integrity Checker (`src/service/integrity-checker/`)
Mercury is an indexer, not the source of truth — it can have bugs or fall behind. This worker runs in the background, pulls the same data from both Mercury and Horizon, and compares them. If they diverge, it flips a Redis flag to disable Mercury and fall back to Horizon directly. This protects users from seeing wrong balances.

### Blockaid (`src/service/blockaid/`)
Before the wallet lets a user sign a transaction or visit a dApp, it calls this service to scan it for known threats — phishing sites, drainer contracts, suspicious assets. Blockaid is a third-party security provider that maintains these threat databases.

---

## How It's Used In Production

1. **Deployed as a Docker container** — the `Dockerfile` builds the app and runs `node build/index.js`
2. **Redis is required** — run separately (a `compose.yaml` is provided for local dev)
3. **Three compiled bundles** (from webpack): `index.js`, `worker.js`, `price-worker.js` — the main process spawns the workers as separate threads
4. **Prometheus scrapes `:9090/metrics`** — for monitoring request rates, durations, cache health
5. **Sentry captures errors** — any unhandled exception is reported automatically
6. **Rate limited** to 3,500 requests/minute per instance
7. **Redis feature flags** — `USE_MERCURY` can be toggled at runtime without a deploy (the integrity checker flips this automatically if Mercury data goes bad)
8. **Worker auto-restart** — if a worker thread crashes it restarts with exponential backoff (1s → up to 5 minutes, max 10 retries)

---

## External Services It Connects To

| Service | What it does |
|---|---|
| **Horizon** (Stellar) | REST API — account data, transaction history, asset info |
| **Soroban RPC** (Stellar) | JSON-RPC — smart contract interaction, simulation |
| **Mercury** | GraphQL indexer — faster historical data than Horizon |
| **Blockaid** | Security scanning API |
| **Coinbase Onramp** | Fiat-to-crypto buy flow |
| **Redis** | Caching, feature flags, price time-series |
| **Sentry** | Error tracking |
| **Prometheus** | Metrics collection (scraped externally) |

---

## General Understanding

This backend exists because a browser wallet can't safely or efficiently talk to all these services directly:
- **Security** — API keys (Blockaid, Coinbase) can't be exposed in browser code
- **Performance** — token prices and account data are cached so thousands of users share the same computed result
- **Reliability** — the Mercury/Horizon fallback pattern means the wallet keeps working even if the indexer has issues
- **Abstraction** — the wallet just calls simple REST endpoints; all the complexity of GraphQL, RPC, JWT signing, and pathfinding is handled here

The codebase is well-structured: routes are thin (validate input, call a service, return result), services hold the business logic, helpers hold reusable utilities. Worker threads handle the background jobs that would be too slow to run per-request.
