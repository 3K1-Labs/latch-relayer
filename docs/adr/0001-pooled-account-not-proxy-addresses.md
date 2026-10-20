# ADR 0001: Pooled account and memo instead of per-user proxy G-addresses

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

