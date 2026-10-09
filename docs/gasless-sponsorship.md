# Gasless sponsorship via FeeForwarder — reference

How `latch-relayer` sponsors transactions for Latch accounts that hold no XLM, what keys that
takes, how throughput scales, and how to keep it from being abused. Written 2026-09-03 from the
contract as frozen at latch-contracts tag `audit-v1`; issue map at the bottom. Update this
when the contract or the model changes — it's meant to be the thing you open first.

## 1. The contract in one page

`FeeForwarder` (latch-contracts `fee-forwarder/`) is a ~65-line shell over OpenZeppelin's
audited `stellar-fee-abstraction` and `stellar-access` (both pinned `=0.7.2`). One singleton
shared by every Latch account. Its job: collect a fee in a token the user holds, then make the
user's real call, atomically. If the call fails, the fee collection reverts too.

```
forward(fee_token, fee_amount, max_fee_amount, expiration_ledger,
        target_contract, target_fn, target_args, user, relayer) -> Val
```

Who signs what:

| Party | Signs | Notes |
|---|---|---|
| `user` (a Latch account, `C…`) | `fee_token, max_fee_amount, expiration_ledger, target_contract, target_fn, target_args` | Via `require_auth_for_args`; the account's `__check_auth` runs. Does **not** sign `fee_amount` or `relayer`. |
| `relayer` (executor, `G…`) | the whole call | `#[only_role(relayer, "executor")]`: must hold the role **and** sign. Also the tx source, so it pays the XLM. |

Sub-invocations the user's authorization tree may need to cover:
- `fee_token.approve(user, fee_forwarder, max_fee_amount, expiration_ledger)` — only if the
  user's existing allowance to the forwarder is below the cap (`FeeAbstractionApproval::Lazy`).
  Clients: check the allowance first, or always include it (see latch-relayer#41 to confirm
  the host tolerates an unused authorized sub-invocation).
- the target call, if it requires the user's own auth (a token transfer from the account does).

A Latch account's `Default` rule matches all three contexts. A session rule scoped to one
`CallContract(target)` does not cover the forwarder or the fee token.

Roles (OZ `AccessControl`):

| Role | Can | Cannot |
|---|---|---|
| `executor` | call `forward()` | spend anything a user didn't sign; exceed the cap; touch collected fees |
| `manager` | `enable_fee_token` / `disable_fee_token`; `sweep_tokens` (all collected fees, any recipient) | grant/revoke roles |
| `admin` | `grant_role` / `revoke_role` for both; two-step admin transfer; `renounce_admin` | sweep or forward directly |

Facts that bite:
- **Only `admin` can grant/revoke `executor`** — no role admin is set at construction. Rotating
  a relayer key touches the cold key unless `set_role_admin("executor","manager")` is called
  post-deploy (decision in latch-contracts#84).
- **The allowlist is off until the first `enable_fee_token`**, and "off" means every token is
  accepted. Enable the fee token immediately after deployment.
- **No allowlist getter.** Rebuild it from `FeeTokenAllowlistUpdated` events; a `forward()`
  simulation with a disallowed token fails with error `#5000` (latch-relayer#40).
- **`renounce_admin` freezes the executor set forever.** Never call it while a relayer exists.
- Error codes seen in tests: `#5000` FeeTokenNotAllowed, `#5003` InvalidFeeBounds,
  `#2000` Unauthorized (wrong/unsigned executor).
- `admin` and `manager` can be Latch smart accounts (a `C…` address works anywhere an
  `Address` is required; `require_auth` calls the account's `__check_auth`). `executor` should
  stay a `G…` key: the tx source must be a classic account, and wrapping a hot key in a smart
  account adds signing work without reducing hotness.

## 2. Keys: what the relayer holds and why

Two key sets, same config shape, different risk class. Never share a key between them.

| Set | Config | Holds | If it leaks |
|---|---|---|---|
| Pool accounts (existing deposit flow) | `POOL_ADDRESS_N` / `POOL_PRIVATE_KEY_N` | user deposits in transit | **funds lost** |
| Executor channel accounts (this flow) | `EXECUTOR_ADDRESS_N` / `EXECUTOR_PRIVATE_KEY_N` | a small XLM float | attacker can spend the float on calls users already signed, or burn it; admin revokes; no user funds at risk |

Why N executor keys: a G-account submits transactions in strict sequence-number order, in
practice **one per ledger (~5 s)**. Parallel sponsorship = several keys held by the *same*
relayer process, chosen round-robin per submission. This is Stellar's standard "channel
account" pattern. It is not multiple relayers.

Sizing: `keys ≈ peak sponsored tx/s × 5`. Testnet: 1. Mainnet launch: 3–5. Add more with a
`grant_role` by admin — no redeploy. A dry key is an outage, not a loss: alarm on a per-key XLM
floor and drop dry keys from rotation (latch-relayer#37).

Keeping N keys from being N secrets: derive them all from one seed (SEP-0005 HD paths) or put
them behind one KMS policy (`GAPS.md` P1). Top-ups come from a treasury account that is never
loaded into the relayer.

## 3. Throughput, and how to know when to add keys

Stellar facts: classic transactions carry up to 100 operations; **Soroban transactions carry
exactly one invocation**. Every `forward()` is its own transaction. "Batching" here means
concurrency across channel keys, never multi-call transactions. The network has per-ledger
Soroban resource caps with surge pricing above them — far above early wallet traffic.

Measure these, as Prometheus metrics (latch-relayer#9 is the endpoint; the metrics themselves
are latch-relayer#43):

| Metric | Type | Meaning / action |
|---|---|---|
| `gasless_queue_depth` | gauge | signed submissions accepted but not yet sent. Sustained > number of keys ⇒ add keys. |
| `gasless_queue_wait_seconds` | histogram | accept → submit. p95 above ~1 ledger (5 s) ⇒ keys are the bottleneck. |
| `gasless_inflight{executor}` | gauge | txs sent, not yet final, per key. Should be 0 or 1 per key; >1 means sequence contention. |
| `gasless_ledger_utilization` | gauge | submissions in the last ledger ÷ keys. Near 1.0 ⇒ at capacity. |
| `gasless_executor_xlm{executor}` | gauge | float per key; alert below the floor. |
| `gasless_submit_total{result}` | counter | success / reverted / stale_quote / rejected — the abuse dashboard. |
| `gasless_dry_keys` | gauge | keys excluded from rotation for low balance. Any >0 pages someone. |

Rule of thumb: add keys when `gasless_queue_wait_seconds` p95 stays above one ledger for
minutes, not on a single spike.

## 4. Abuse and denial-of-service

The chain is not the attack surface; the two HTTP endpoints are. Every sponsored transaction
costs XLM, so the attack is "make Latch pay for garbage." Layers, in order of importance:

1. **Simulate before every submission.** A `forward()` that would fail (no fee-token balance,
   disallowed token, failing target) is caught in simulation at RPC cost, not XLM. Never submit
   unsimulated. This alone removes most cost-inflicting attacks.
2. **The user pays you.** Every successful sponsorship collects the fee token. Set the margin so
   `fee_amount` covers XLM + overhead; spam of *valid* transactions is revenue.
3. **Quote-time gates.** Refuse a quote unless the account's fee-token balance ≥ cap. Rate-limit
   quotes per account and per IP. Per-account daily sponsorship cap. Quotes expire in a few
   ledgers, so unsigned quotes cost nothing.
4. **HTTP hygiene** (already in `GAPS.md`): rate limiting, request IDs, backoff.

Residual risk after those: a code path that submits without simulating. That's a test.
Deliberation items: latch-relayer#44.

## 5. Build vs adopt

`reference/openzeppelin-relayer/` in this repo already implements Stellar sponsored
transactions (its changelog, #563), KMS signer backends (AWS/GCP/Azure/Vault/Turnkey), and
multi-key sequence management. Evaluate running it as the executor service, with this Go
service calling it, before hand-building #37/#38/#39. Decision goes in #37.

## 6. Issue map

Umbrella / task list: **#35**. Milestone: *Gasless v1 — testnet end-to-end*.

| Order | Issue | What |
|---|---|---|
| 0 | latch-contracts#84 | custody decision, `set_role_admin` choice, testnet deployment recorded in `BUILD.md` |
| 1 | #37 | N executor channel keys, floors, KMS path, OZ-relayer evaluation |
| 2 | #38 | `POST /gasless/quote` — simulate `forward()` to price the cap |
| 3 | #39 | `POST /gasless/submit` — attach user auth, sign as executor, submit |
| 4 | #40 | allowlist mirror from events |
| 5 | #41 | `make gasless-e2e` — exit criterion |
| pre-mainnet | #43 | throughput metrics above |
| pre-mainnet | #44 | abuse controls above |
| follow-on | latch-web-extension#66, latch-mobile#70 | clients build/sign the auth tree |

Deployed addresses live in latch-contracts `docs/BUILD.md`, not here — one source of truth.
