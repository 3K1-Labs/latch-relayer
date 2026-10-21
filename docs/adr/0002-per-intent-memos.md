# ADR 0002: Per-intent memos instead of permanent per-user memos

Original Design (Superseded):

The first implementation derived a permanent memo_id from each user's C-address using `uint64(sha256(c_address)[:8])`. The same C-address always produced the same memo_id — no server round-trip needed to re-derive it. The relay stored this as a permanent registration.

Why per-intent is better:

1. On-ramp compatibility. MoonPay and similar on-ramps treat the wallet tag as a session identifier for a specific payment, not a permanent user handle. Per-intent memos map directly to how on-ramps think about deposit flows.
2. Expiry and reconciliation. A permanent memo lives forever — there is no way to retire it, set an expected amount, or correlate it with an external transaction ID. An intent has a TTL, an expected amount, an external_id (e.g. MoonPay's transaction ID), and a status lifecycle.
3. No "all deposits for Alice forever share one memo" problem. If the same memo is reused across sessions, any deposit to that address at any time routes to the same C-address indefinitely. Per-intent memos expire after the session ends.
4. The lookup requirement is the same either way. Whether the memo is derived or random, the relay still needs to look up memo_id → c_address at forward time. The DB is required in both cases.

What changed in the implementation:

- `registrations` table replaced by `intents` table with `expires_at`, `status`, `expected_amt`, `external_id`
- `POST /register` replaced by `POST /intents` — latch-api calls this to create a funding session
- Memo_id is now a random `int64` generated server-side (via `math/rand/v2`, auto-seeded from crypto/rand)
- Intent lifecycle: `pending` → `completed` (forward succeeded) / `expired` (TTL passed) / `failed` (all retries exhausted)
- Retry worker now also calls `ExpireStaleIntents` on each tick
- `DeriveID` and `ToMuxedAddress` removed from memo package (unused)
