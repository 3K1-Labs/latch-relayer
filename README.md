# latch-relayer

A Go microservice that watches a Stellar pool account for inbound payments and forwards them onward as Soroban SAC transfers to the depositor's C-address.

## How it works

1. A user opens an intent via `POST /intents` — the API returns a `memo_id` and the pool address to deposit to.
2. The relayer watches the pool address via Horizon SSE. When a payment arrives with a matching `memo_id`, it looks up the intent and submits a Soroban transfer to the C-address.
3. Deposits with a missing or unknown `memo_id` are forwarded to the configured recovery address.

## Prerequisites

- Go 1.26+
- PostgreSQL
- A funded Stellar testnet account (pool account)

## Setup

```bash
cp .env.example .env
# Fill in DATABASE_URL, POOL_ADDRESS_1, POOL_PRIVATE_KEY_1, RECOVERY_ADDRESS
```

Apply migrations and start:

```bash
make run
```

## Common commands

```bash
make build    # compile
make test     # unit tests
make vet      # go vet
make fmt      # gofmt
make e2e      # end-to-end test against testnet
make sweep    # test invalid/missing memo recovery flow
make docker   # build Docker image
```

## API

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/intents` | Create a deposit intent, returns memo_id + pool address |
| `GET`  | `/deposit/status/{memo_id}` | Check intent status |
| `GET`  | `/health` | Health check |

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design, intent lifecycle, retry strategy, and sweep/recovery flow.
