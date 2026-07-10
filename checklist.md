Brief description:

- Latch runs a single pooled G-address (the relay account) rather than generating a per-user proxy G-address. This eliminates per-user keypair derivation, reduces key management complexity, and is universally compatible with CEX withdrawals today.
- Each funding session gets a unique random memo ID tied to the user's C-address for that intent only. This enables on-ramp integration (MoonPay-style wallet tags), expiry, and per-session reconciliation.
- Users give on-ramps and other senders: Latch's pooled G-address + the intent memo ID. This is the standard Stellar deposit UX that users already know from exchanges.
- Relay service watches the pooled account for incoming payments (via Horizon SSE), parses the memo, maps it to the target C-address via the intent lookup, and forwards funds immediately via Soroban SAC transfer.
- The pool G-address is both signer and fee payer — no fee bump needed. Fees are paid from the pool's operational balance.
- Works with CEX withdrawals, fiat on-ramp rails (MoonPay etc.), and direct Stellar wallet sends.

Checklist:

- [x]  Design and implement memo ID assignment (per-intent memo_id → C-address mapping, stored in relay DB)
- [x]  Build relay deposit watcher (Horizon SSE on pooled G-address with cursor resume)
- [x]  Implement memo parsing and memo_id → C-address lookup via intent table
- [x]  Build forwarding service: on deposit detected, simulate via RPC and submit SAC transfer to C-address
- [x]  Add retry + idempotency logic to relay (handle duplicate events, failed forwards, expired intents)
- [x]  Test: on-ramp style deposit (POST /intents → G-address + memo) → C-address routes correctly on testnet
- [x]  Test: direct Stellar wallet send (G-address + memo) → C-address routes correctly on testnet
- [ ]  Test: missing or invalid memo handling (sweep/recovery flow) — implemented, no dedicated test script yet
- [ ]  Implement muxed account (M-address) support as a parallel path alongside memo
- [ ]  Publish relay repo and ops documentation

Proof of completion: A POST /intents → deposit → forward → status=completed round-trip works end-to-end on testnet. The relay correctly forwards XLM to a real Soroban C-address, handles retries, and expires stale intents.

---

📝 Architectural Decision Record — Why We Moved Away from Proxy G-Addresses

Original Design (Rejected):

The initial approach was to generate a unique proxy G-address per user, derived deterministically from their C-address using something like HMAC-SHA256(relay_master_secret, c_address) → raw Ed25519 seed → keypair. The idea was that the relay holds one master secret and can re-derive any user's proxy keypair on demand without per-user key storage, keeping the system non-custodial in appearance.

Why it doesn't work well:

1. C-addresses have no keypair. A C-address is a Soroban contract address — it is derived from a deployer + salt hash at deploy time. There is no private key behind it. You cannot reverse-derive a G-address from a C-address in any meaningful cryptographic sense; you can only create an arbitrary keypair and label it as belonging to that C-address in your own database. This makes the "deterministic derivation" a relay-internal convention, not a protocol guarantee.
2. Per-user keypair management. Even with a master secret derivation scheme, the relay must re-derive and hold in memory the private key for each proxy G-address at the moment of forwarding. At scale this is a significant operational and security surface — one leaked master secret exposes every user's proxy funds.
3. Each proxy G-address needs funding. A Stellar G-address does not exist on-ledger until it receives the minimum base reserve (~0.5 XLM). This means the relay must pre-fund every proxy account before a user can receive a deposit — adding cost and an extra transaction per user even before any deposit arrives.
4. Fee bump complexity. Forwarding from a proxy G-address that has just been funded adds ordering dependencies: fund the proxy → wait for confirmation → user sends deposit → relay detects → relay signs with derived key → fee bump wraps it. Multiple sequential transactions, multiple failure points.
5. Memos were misunderstood as per-transaction. The team initially assumed memo values were unique per transaction (like a payment reference), which made a memo-based pooled approach seem unreliable. In practice, memos are just a free field — exchanges assign users a persistent memo ID that never changes, and the user supplies that same memo on every deposit. This is the standard Stellar custodial deposit pattern used by all major exchanges today.

What the pooled account + memo approach gives us instead:

- One keypair to manage, one account to fund
- Zero per-user setup cost before a deposit arrives
- CEX compatibility out of the box (G-address + memo is universally supported)
- Per-intent memos enable on-ramp integration with expiry, reconciliation, and MoonPay-style wallet tags

---

📝 Architectural Decision Record — Why We Moved from Permanent Per-User Memos to Per-Intent Memos

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
