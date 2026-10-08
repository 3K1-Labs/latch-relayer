# Gasless sponsor API

How latch-api submits wallet transactions through the gasless service. latch-api decides what the transaction is and validates it; the gasless service pays for it and gets it on-chain through a leased channel account and a fee-bump from the funder.

Only **sponsored** mode exists today: Latch pays, the user is charged nothing, and only a new wallet's setup calls qualify (`SPONSORED_CALLS`). **Forward** mode, where the user reimburses the fee in XLM or USDC through FeeForwarder, answers `501` until it is built.

All routes need `Authorization: Bearer <RELAYER_API_KEY>`. Clients never call this service.

## `POST /gasless/submit`

```json
{
  "request_id": "deploy-7f3c2a91",
  "wallet": "CB...WALLET",
  "mode": "sponsored",
  "transaction": "<base64 TransactionEnvelope>"
}
```

| Field | Rules |
|---|---|
| `request_id` | 8–64 characters of `[A-Za-z0-9_-]`. Idempotency key: the same id with the same body returns the stored outcome and never pays twice; the same id with a different body is `409`. |
| `wallet` | The C-address the transaction is for. Caps are counted against it. |
| `mode` | `sponsored` (or `forward`, not built yet). |
| `transaction` | An unsigned v1 envelope with **exactly one** `InvokeHostFunction` operation that invokes a contract, with its authorization entries already signed. Its source account, sequence number, fee, time bounds and Soroban data are ignored: the service re-sources it onto a channel and re-simulates. |

What the service checks before it pays:

- The call (contract + function) is in `SPONSORED_CALLS`. `wallet:fn` matches only when the invoked contract is the request's `wallet`.
- No authorization entry uses source-account credentials (they would authorize as the channel) or claims to act for the executor, funder or a channel.
- Simulation in **enforce** auth mode succeeds, so a bad or expired user signature fails here and costs nothing.
- The funder is above `FUNDER_MIN_XLM`.
- The wallet is under `SPONSOR_MAX_TX_PER_WALLET` and `SPONSOR_MAX_XLM_PER_WALLET`, and all wallets together are under `SPONSOR_MAX_XLM_PER_DAY`. The submission's maximum cost is reserved before sending, under a lock, so concurrent requests can't overrun a cap.

### Responses

A submission answers with its record once it is final, or after `SPONSOR_SYNC_WAIT_SECONDS` with `202` while it is still in flight.

```json
{
  "request_id": "deploy-7f3c2a91",
  "wallet": "CB...WALLET",
  "mode": "sponsored",
  "status": "success",
  "tx_hash": "4f1c...",
  "fee_charged_stroops": 81234,
  "created_at": "2026-10-08T10:00:00Z",
  "updated_at": "2026-10-08T10:00:07Z"
}
```

| HTTP | `status` | Meaning | What latch-api does |
|---|---|---|---|
| 200 | `success` | Landed and succeeded. | Done. |
| 200 | `failed` | Landed and failed (`error_code` is the result code). The fee was charged. | Show the failure. A new attempt needs a new `request_id`. |
| 200 | `rejected` | Refused before anything landed (`channels_busy`, `network_busy`, `insufficient_fee`, `sequence_contention`, `expired`, …). Nothing was charged and it doesn't count against caps. | Safe to retry with a new `request_id`. |
| 202 | `pending` / `unconfirmed` | In flight, or sent with the outcome not seen yet. It may still land. | Poll `GET /gasless/requests/{request_id}`. Don't resubmit under a new id. |

Refusals before anything is reserved come back as `{"error": {"code", "message"}}`:

| HTTP | `code` |
|---|---|
| 400 | `invalid_request` |
| 403 | `not_sponsorable` |
| 409 | `request_id_conflict` |
| 422 | `simulation_failed` |
| 429 | `wallet_cap_reached`, `daily_budget_reached` |
| 501 | `not_implemented` (forward mode) |
| 503 | `sponsorship_unavailable` (funder below its floor) |

## `GET /gasless/requests/{request_id}`

Returns the same record: `200` when final, `202` while pending or unconfirmed, `404` if unknown. A resolver settles unconfirmed and abandoned submissions in the background by looking up their hash, so every request reaches a final status: a transaction that never lands is marked `rejected` / `expired` once its time bounds pass.

## Fees

- Inclusion fee: the network's recent p90 Soroban inclusion fee (`getFeeStats`), at least 100 stroops and at most `MAX_INCLUSION_FEE_STROOPS`. A `tx_insufficient_fee` rejection doubles it, up to twice, within that cap.
- The fee-bump bids the inner transaction's rate, not txnbuild's doubled bid (see `internal/feebump`).
- `fee_charged_stroops` is what the network took, read from the result.
