# Gasless sponsor API

How latch-api submits wallet transactions through the gasless service. latch-api decides what the transaction is and validates it; the gasless service pays for it and gets it on-chain through a leased channel account and a fee-bump from the funder.

Two modes:

- **Sponsored:** Latch pays and the user is charged nothing. Only a new wallet's setup calls qualify (`SPONSORED_CALLS`), within per-wallet and daily caps.
- **Forward:** the user reimburses the fee in XLM, or USDC if they hold no XLM, through the FeeForwarder contract. See [Forward mode](#forward-mode). No caps apply: the user pays.

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
| `mode` | `sponsored` or `forward`. |
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
| 422 | `simulation_failed`, `max_fee_too_low` (forward: the signed maximum doesn't cover the fee; quote again and re-sign) |
| 429 | `wallet_cap_reached`, `daily_budget_reached` |
| 501 | `not_implemented` (forward mode not configured) |
| 503 | `sponsorship_unavailable` (funder below its floor), `price_unavailable` (no XLM price for a USDC fee) |

## Forward mode

A smart account can't pay its own network fee, so the funder pays the XLM and FeeForwarder collects it back from the user in the same transaction.

**1. Quote** — after simulating the user's action, latch-api asks what maximum to have the user sign:

```http
POST /gasless/quote
{"fee_token": "<XLM or USDC SAC>", "resource_fee_stroops": 51234}
```

```json
{"fee_forwarder": "C…", "relayer": "G…EXECUTOR", "fee_token": "C…", "symbol": "XLM", "max_fee_amount": 114043}
```

`max_fee_amount` is in the token's units (7 decimals). It covers the highest inclusion fee this service will bid plus `FEE_MARGIN_BPS`, so a surge can't push the real fee above it. The user is charged the actual cost, usually much less. `GET /gasless/fee-tokens` lists the accepted tokens with the `fee_forwarder` and `relayer` addresses.

**2. Build and sign** — latch-api wraps the action in `forward(fee_token, fee_amount, max_fee_amount, expiration_ledger, target_contract, target_fn, target_args, user, relayer)` on `fee_forwarder`, with any `fee_amount` (it is replaced) and `relayer` from the quote. It simulates in record mode, keeps the **user's** authorization entry (rooted at `forward` with `(fee_token, max_fee_amount, expiration_ledger, target_contract, target_fn, target_args)`, with the fee token's `approve` and the target call beneath it), has the user sign it, and **drops the executor's entry**.

**3. Submit** with `"mode": "forward"`. The service:

- checks the call is `forward` on the configured FeeForwarder, the fee token is accepted, `user` is the request's `wallet`, and the target isn't the FeeForwarder;
- simulates in record mode to get the resource fee and the executor's authorization;
- prices the cost (2 × inclusion fee + resource fee, plus the margin) in the fee token — XLM 1:1 in stroops, USDC at the XLM/USD price — and refuses with `max_fee_too_low` if it exceeds the signed maximum;
- sets `fee_amount` and `relayer`, signs the executor's entry (valid `EXECUTOR_AUTH_LEDGERS`), and re-simulates in enforce mode, which checks both signatures;
- submits like a sponsored transaction. The record carries `user_fee: {token, symbol, amount}`.

The user's tree depends on the forwarder's allowance at build time (`approve` is included only when it's short). If it changed before submit, enforce simulation fails with `simulation_failed` and nothing is charged: rebuild and re-sign.

## `GET /gasless/requests/{request_id}`

Returns the same record: `200` when final, `202` while pending or unconfirmed, `404` if unknown. A resolver settles unconfirmed and abandoned submissions in the background by looking up their hash, so every request reaches a final status: a transaction that never lands is marked `rejected` / `expired` once its time bounds pass.

## Fees

- Inclusion fee: the network's recent p90 Soroban inclusion fee (`getFeeStats`), at least 100 stroops and at most `MAX_INCLUSION_FEE_STROOPS`. A `tx_insufficient_fee` rejection doubles it, up to twice, within that cap.
- The fee-bump bids the inner transaction's rate, not txnbuild's doubled bid (see `internal/feebump`).
- `fee_charged_stroops` is what the network took, read from the result.
