# latch-relayer

Two Go services for Latch, deployed separately:

- **Deposit bridge (`cmd/serve`)** watches Stellar pool accounts for inbound payments and forwards them as Soroban SAC transfers to the depositor's C-address.
- **Gasless service (`cmd/gasless`)** submits wallet transactions on mainnet: leased channel accounts as sources, a fee-bump funder, and the FeeForwarder executor so users pay fees in XLM or USDC, with Latch sponsoring only a new wallet's setup transactions. Its sponsor endpoint is not built yet; see [GAPS.md](GAPS.md).

## How the deposit bridge works

1. latch-api opens an intent via `POST /intents`; the relayer returns a `memo_id` and the pool address to deposit to.
2. The relayer polls the pool's payments on Horizon. When a payment arrives with a matching `memo_id`, it looks up the intent and submits a Soroban transfer to the C-address through a leased channel account.
3. Deposits with a missing or unknown `memo_id` are forwarded to the configured recovery address.

## How the gasless service works

latch-api validates and simulates each wallet transaction, then hands it to the gasless service. The service leases a channel account as the transaction source, wraps it in a fee-bump from the funder, executes FeeForwarder `forward()` to collect the user's fee, and submits via Stellar RPC. Keys, fee model and rotation: [docs/gasless-keys.md](docs/gasless-keys.md).

## Prerequisites

- Go 1.26+
- PostgreSQL (one database per service)
- Funded Stellar accounts: the pool account(s) for the deposit bridge; the executor and funder for the gasless service

## Setup

Deposit bridge:

```bash
cp .env.example .env
# Fill in DATABASE_URL, POOL_ADDRESS_1, POOL_PRIVATE_KEY_1, RECOVERY_ADDRESS
make run
```

Gasless service:

```bash
cp gasless.env.example gasless.env
# Fill in the executor, funder and CHANNEL_SEED (see docs/gasless-keys.md)
make channels
make run-gasless
```

## Common commands

```bash
make build             # compile
make test              # unit tests
make vet               # go vet
make fmt               # gofmt
make lint              # lint
make sweep             # test invalid/missing memo recovery flow
make channels          # create gasless channel accounts
make deposit-channels  # create deposit channel accounts
make docker            # build the deposit bridge image
make docker-gasless    # build the gasless image
```

## API

Deposit bridge:

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/intents` | Create a deposit intent, returns memo_id + pool address |
| `GET`  | `/deposit/status/{memo_id}` | Check intent status |
| `GET`  | `/health` | Health check (503 if the database is unreachable) |
| `GET`  | `/metrics` | Prometheus metrics (bearer auth) |

Gasless service:

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/health` | `ok` / `degraded` (503 only if the database is unreachable) |
| `GET`  | `/gasless/status` | Funder and executor balances, channel counts (API key) |
| `GET`  | `/metrics` | Prometheus metrics (API key) |

## Docs

- [ARCHITECTURE.md](ARCHITECTURE.md): design decisions, intent lifecycle, retry strategy, sweep/recovery
- [GAPS.md](GAPS.md): what is left before mainnet, and what is already fixed
- [docs/gasless-keys.md](docs/gasless-keys.md): gasless keys, fee model, rotation
- [docs/deposit-channels.md](docs/deposit-channels.md): deposit throughput and channel accounts
- [docs/deployment.md](docs/deployment.md): deploying to Render
