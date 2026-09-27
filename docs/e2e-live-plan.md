# Live End-to-End Test Plan (latch-api → latch-relayer → Stellar testnet)

## 1. Who talks to whom

```
 test script (plays the mobile app + a depositor wallet)
      │  JWT auth
      ▼
 latch-api (Render)  ──Bearer RELAYER_API_KEY──►  latch-relayer (Render)
   /v1/auth/challenge, /sign-in                    POST /intents
   /v1/accounts/register                           GET  /deposit/status/{memo_id}
   /v1/accounts/deposit-intent   ───────────────►  PATCH /intents/{memo_id}
   /v1/accounts/deposit/status/:memo_id ────────►
   /api/on-ramp/session, /intent/:id (webapp) ──►
                                                        │ watches pool via Horizon SSE
 depositor wallet ── payment + MEMO_ID ──► pool G-address ┘
                                                        │ SAC transfer
                                                        ▼
                                              smart account C-address
```

- **latch-api is the only real client of the relayer.** The relayer requires `Authorization: Bearer <RELAYER_API_KEY>`, and only latch-api holds that key. So a real E2E test goes **through latch-api**, like the mobile app does.
- **Part of the test goes straight to the chain.** latch-api never sends the deposit; the user's wallet does. The test script sends the payment to Horizon itself, and the relayer sees it there.

## 2. Two layers of tests

| Layer | Path | Why |
|---|---|---|
| **A. Relayer-direct** | script → relayer (with API key) → chain | Isolates the relayer. If this fails, latch-api isn't the problem. The existing `make e2e` already does this; point `RELAYER_URL` at the live host. |
| **B. Full-stack** | script → latch-api → relayer → chain | The real user path: auth, account ownership check, relayer proxy, boot-retry logic. |

Run A first. If A passes and B fails, the bug is in latch-api or in the config between the two services.

## 3. Automating auth (no email OTP needed)

latch-api has wallet sign-in, so a script can get a JWT with just a keypair:

1. `POST /v1/auth/challenge` `{"wallet":"G...","key_type":"ed25519","network":"testnet"}` → `nonce`
2. Sign the raw nonce bytes with the ed25519 key, base64 it.
3. `POST /v1/auth/sign-in` `{wallet, key_type, network, nonce, signature}` → `access_token`

(Check whether `nonce` is base64url, since the service calls it `NonceB64URL`. If so, decode it before signing.)

## 4. Test scenarios

### Preflight (fail fast)
- P1 `GET latch-api/health` → DB+Redis ok
- P2 `GET relayer/health` → ok (the first call may take ~15–30s while Render wakes it)
- P3 Relayer without a Bearer key → 401 (confirms auth is actually on in prod)
- P4 Depositor account funded on testnet (Friendbot)
- P5 Pool account balance above the reserve (it pays forward fees)

### Happy path, full stack (the main test)
1. Wallet sign-in → JWT
2. `POST /v1/accounts/register` with the test C-address
3. `POST /v1/accounts/deposit-intent` → `memo_id`, `pool_address`, `expires_at` (client timeout ≥30s)
4. Depositor sends N XLM to `pool_address` with `MEMO_ID(memo_id)`
5. Poll `GET /v1/accounts/deposit/status/{memo_id}` every 3s, up to about 90s
6. Assert: `status=completed`, one forward with `status=done` and a `forward_tx` hash
7. **On-chain check:** the C-address's native SAC balance went up by about N (via RPC `simulate` of `balance`), and `forward_tx` succeeded on Horizon

### Negative / edge paths
| # | Case | Expected |
|---|---|---|
| N1 | Deposit with **no memo** | Swept to `RECOVERY_ADDRESS`; recovery balance goes up |
| N2 | Deposit with an **unknown memo** | Same as N1 |
| N3 | Status for a memo owned by **another user** | latch-api 403/404 (the ownership check works) |
| N4 | deposit-intent with `network:"mainnet"` | 400 VALIDATION_ERROR |
| N5 | deposit-intent for an **unregistered** C-address | Rejected |
| N6 | Intent with a short `expires_in`, then pay after it expires | Intent `expired`, funds go to recovery (confirm the intended behaviour in ARCHITECTURE.md) |
| N7 | **Same tx seen twice** (restart relayer mid-flow / cursor resume) | Exactly one forward (the `tx_hash` idempotency key) |
| N8 | No JWT on `/v1/accounts/*` | 401 |

### Resilience (optional, run by hand)
- R1 **Cold start:** leave the relayer idle for 15+ min, then create an intent. latch-api should ride out the boot, or return 503 BAD_GATEWAY within its budget.
- R2 **Deposit while the relayer sleeps:** send the payment while it's asleep, then wake it. The SSE cursor should resume and the payment should still be forwarded.
- R3 **Small burst:** 10 concurrent intents and deposits, all forwarded, no duplicates.

### Webapp on-ramp path (if it's enabled in prod)
- `POST /api/on-ramp/session` → memo registered with the relayer; `PATCH` sets `external_id`; `GET /api/on-ramp/intent/:id` reflects the relayer state.

### Gasless service
- It's currently only `GET /health` and `GET /gasless/status`, and no sponsor endpoint is deployed yet. The live check is limited to: health ok, executor role ok, funder above `FUNDER_MIN_XLM`, all channels funded. Full gasless E2E waits for the later phases (P2+).

## 5. What to build

- A new `scripts/e2e_live/main.go` in latch-relayer (Go, reusing the Horizon and keypair code from `scripts/e2e_test`). Flags: `-layer=relayer|api|both`, `-scenario=happy,n1,...`.
- Config only through env vars, never hard-coded:
  ```
  LATCH_API_URL, RELAYER_URL, RELAYER_API_KEY (layer A only)
  WALLET_SEED        # signs in to latch-api
  DEPOSITOR_SEED     # sends the testnet payment
  C_ADDRESS          # smart account to fund
  RECOVERY_ADDRESS   # for asserting N1/N2
  ```
- Output: a pass/fail line per scenario, with tx hashes and stellar.expert links, and a non-zero exit code on failure.
- `make e2e-live` target.

## 6. Safety rules
- **Testnet only.** The script refuses to run unless both health endpoints and Horizon report testnet.
- Small amounts (1–5 XLM). Use Friendbot to top up the depositor.
- Tag every intent with `external_id=e2e-<timestamp>` so test rows in the prod DB are easy to find and clean up.
- Never print or commit secrets. Keep them in a gitignored `e2e.env`.

## 7. Decisions (2026-09-27)
- latch-api: `https://latch-api-4cvn.onrender.com`. Redeployed 2026-09-27 pointing at the new relayer; the old `latch-backend.onrender.com` pointed at the retired relayer.
- First full run, 2026-09-27: `preflight`, `happy`, `nomemo`, `unknownmemo` and `authz` all pass. Load behaviour: see [burst-test-2026-09-27.md](burst-test-2026-09-27.md).
- Relayer: `https://latch-relayer-zy93.onrender.com` (`/health` ok; a request without a key gets 401).
- **Everything goes through latch-api.** Layer A is dropped. `RELAYER_API_KEY` is used only for an optional read-only check that latch-api's `RELAYER_URL` points at this relayer.
- **A new smart account every run**, deployed via `POST /v1/smart-account/challenge` then `/v1/smart-account/ed25519` (no session needed, the bundler pays):
  - challenge nonce = hex
  - sign the hex-decoded bytes
  - signature = std base64
- Webapp on-ramp: out of scope for now.

## 8. Finding: wallet sign-in can't use the deposit endpoints
Checked live on 2026-09-27 with a random keypair:
- `/v1/auth/sign-in` → 200. The token's `sub` is the G-address, with `scope: wallet`.
- `/v1/accounts/register` → **500 INTERNAL_ERROR**, because `AccountService.Register` does `uuid.Parse(userID)`.
- `/v1/accounts/deposit-intent` → 400 "not registered to the caller".

So the test has to log in with **email + OTP**. Plan:
- Type the OTP once.
- Save the rotating refresh token (30-day TTL) in a gitignored `.e2e-session.env`.
- Later runs call `/v1/auth/refresh`.

OTP limit: 3 per hour per email. A second email (`E2E_EMAIL_2`) enables the cross-user check (N3).

The 500 is a latch-api bug either way: it should be a 4xx, or wallet users should be supported.
