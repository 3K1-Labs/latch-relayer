# sequencer (cronos-stream)

Source: `github.com/FrankiePower/cronos-stream/tree/main/sequencer`

## Language & Stack

**Language:** Rust (edition 2021)
**Async runtime:** Tokio — Rust doesn't have a built-in event loop like Node.js, so Tokio provides one explicitly
**HTTP framework:** Axum — similar role to Express or Fastify
**Blockchain library:** Alloy — handles Ethereum addresses, EIP-712 signatures, and RPC calls
**Database:** PostgreSQL via SQLx (async)
**Serialization:** Serde — converts Rust structs to/from JSON

Key dependencies:
- `alloy` — Ethereum/EVM library (signing, RPC, address types)
- `tokio` — async runtime
- `axum` — HTTP server
- `sqlx` — async PostgreSQL client
- `serde` / `serde_json` — JSON handling
- `dotenvy` — loads `.env` file
- `tracing` / `tracing-subscriber` — structured logging (controlled by `RUST_LOG` env var)
- `thiserror` — custom error types

---

## What This Service Is

The sequencer is an **off-chain trusted middleman** for a **payment channel system** built on the **Cronos EVM blockchain**. It is completely independent of Stellar.

Payment channels are a pattern for doing many fast, cheap micropayments between two parties without hitting the blockchain for every one. Instead:
1. The payer opens a channel on-chain (one transaction)
2. They send signed payment vouchers off-chain to the sequencer for each payment
3. The sequencer validates and co-signs each voucher
4. Eventually the channel is closed on-chain with the final balance (one transaction)

Only the open and close touch the blockchain. Everything in between is handled off-chain by the sequencer — which is why it's fast and cheap.

```
┌──────────┐   voucher    ┌────────────┐   on-chain    ┌──────────────┐
│  Client  │ ───────────▶ │ Sequencer  │ ────────────▶ │ StreamChannel│
│ (Payer)  │              │  Service   │               │   Contract   │
└──────────┘              └────────────┘               └──────────────┘
     │                          │
     │ signs voucher            │ validates signature (EIP-712)
     │                          │ co-signs voucher
     │                          │ stores state in PostgreSQL
     │                          │ can finalize channel on-chain
```

---

## Environment Variables

From `.env.example`:

| Variable | Purpose |
|---|---|
| `PORT` | Port to listen on (default `4001`) |
| `DATABASE_URL` | PostgreSQL connection string |
| `RPC_URL` | Cronos EVM RPC endpoint (e.g. `https://evm-t3.cronos.org/`) |
| `CHAIN_ID` | EVM chain ID (e.g. `338` for Cronos testnet) |
| `CHANNEL_MANAGER_ADDRESS` | Address of the deployed `StreamChannel` smart contract |
| `SEQUENCER_PRIVATE_KEY` | Private key of the sequencer's wallet — used to co-sign vouchers |

---

## Folder Structure

```
sequencer/
├── src/
│   ├── main.rs        ← entrypoint — wires everything together and starts server
│   ├── config.rs      ← loads env vars into a typed Config struct
│   ├── error.rs       ← custom error types
│   ├── model.rs       ← data structures (ChannelState, request/response types)
│   ├── crypto.rs      ← EIP-712 signature verification and signing
│   ├── db.rs          ← PostgreSQL operations (init tables, load/save state)
│   ├── service.rs     ← all business logic (validation, state updates, finalization)
│   └── handlers.rs    ← HTTP route handlers (thin layer over service)
├── Cargo.toml         ← Rust dependencies (like package.json)
├── Cargo.lock         ← locked dependency versions (like yarn.lock)
├── .env.example       ← environment variable template
├── Dockerfile         ← container build
├── docker-compose.yml ← app + PostgreSQL for local dev
├── README.md          ← detailed docs + Rust concepts guide
└── sequence.md        ← flow diagrams
```

---

## Entrypoint — `src/main.rs`

Here is exactly what happens when the app starts, step by step:

1. **Load `.env` file** — `dotenvy::dotenv()` reads environment variables from the file
2. **Set up logging** — configured via `RUST_LOG` env var (e.g. `RUST_LOG=info`)
3. **Load config** — `Config::from_env()` parses all env vars into a typed struct
4. **Connect to PostgreSQL** — creates a connection pool (max 5 connections)
5. **Init database** — creates tables if they don't exist
6. **Load existing state** — reads all channel data from PostgreSQL into memory (a `HashMap` in a thread-safe lock)
7. **Create RPC provider** — connects to the Cronos EVM node at `RPC_URL`
8. **Parse sequencer wallet** — loads `SEQUENCER_PRIVATE_KEY` into a signer object, derives the wallet address
9. **Verify against smart contract** — reads the sequencer address registered in the `StreamChannel` contract on-chain and checks it matches. Fails hard if they don't match — prevents misconfiguration
10. **Bundle into `AppState`** — all shared resources (DB, channels, config, provider, signer) are packaged into one struct that all handlers share
11. **Create router** — `create_router(state)` registers all HTTP routes with Axum
12. **Start server** — binds to `0.0.0.0:PORT` and begins accepting requests

---

## API Routes

| Method | Route | Purpose |
|---|---|---|
| `POST` | `/channel/seed` | Register a new channel (called after user opens one on-chain) |
| `GET` | `/channel/:id` | Get current state of a channel |
| `POST` | `/validate` | Validate a payment voucher — read only, no state change |
| `POST` | `/settle` | Accept a payment voucher — validates, co-signs, updates state |
| `POST` | `/channel/finalize` | Close the channel on-chain by calling the smart contract |
| `GET` | `/channels/by-owner/:owner` | List all channels belonging to an address |

---

## Key Modules Explained

### `crypto.rs` — EIP-712 Signatures
EIP-712 is an Ethereum standard for signing structured data (not just raw bytes). When a payer sends a voucher — essentially "I authorise payment of X amount" — they sign it with their wallet's private key using EIP-712. `crypto.rs` verifies that signature (proves the payer really sent it) and then produces the sequencer's co-signature on the same voucher. Both signatures together are what makes the voucher valid on-chain.

### `service.rs` — Business Logic (19KB, the core of the app)
This is where all the rules live:
- When a voucher arrives at `/settle`, service validates: is the channel open? Is the signature valid? Is the new amount greater than the previous? Is there enough balance?
- If valid, it updates the in-memory channel state and persists to PostgreSQL
- When `/channel/finalize` is called, service builds and submits an on-chain transaction to the `StreamChannel` contract to close the channel with the final balance

### `db.rs` — PostgreSQL Persistence
Creates the `channels` table on startup, saves channel state after every voucher, and loads all existing channels back into memory on restart. The in-memory `HashMap` is the fast path for reads; PostgreSQL is the durable backup.

### `model.rs` — Data Structures
Defines `ChannelState` (what a channel looks like: ID, payer address, balance, nonce, status), plus all request and response JSON shapes. Rust's `serde` library auto-generates JSON serialization for these structs.

---

## How It's Used In Production

1. **Single binary** — `cargo build --release` produces a compiled binary
2. **Docker container** — the `Dockerfile` builds it and exposes port 4001
3. **PostgreSQL required** — for durable channel state
4. **Cronos RPC required** — to verify signatures and submit on-chain finalization
5. **Private key in env** — the sequencer wallet key must be set; the on-chain contract must have this address registered as the sequencer
6. **Stateful** — channels live in memory (fast) and PostgreSQL (durable). On restart, state is reloaded from the DB

---

## External Services

| Service | Purpose |
|---|---|
| **Cronos EVM RPC** | Read sequencer address from contract; submit finalization transactions |
| **StreamChannel smart contract** | The on-chain payment channel contract this service manages |
| **PostgreSQL** | Durable storage for all channel state |

---

## How This Differs From the Stellar Backends

| | sequencer | stellar-freighter-backend-v2 | wallet-backend |
|---|---|---|---|
| Language | Rust | Go | Go |
| Blockchain | Cronos (EVM) | Stellar | Stellar |
| Role | Payment channel co-signer | Wallet REST API | Ledger indexer |
| Database | PostgreSQL | PostgreSQL (via wallet-backend) | PostgreSQL |
| Key primitive | EIP-712 vouchers | JWT (Ed25519) | GraphQL |
| Independent of Stellar? | Yes, completely | No | No |
