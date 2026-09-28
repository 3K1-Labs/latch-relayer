# Channel accounts for deposit forwarding (#48)

> **Status (2026-09-28).** Channels shipped in #55. There, the pool is the operation's source and signs the transaction itself, rather than an authorization entry as sketched in section 4. That is cheaper: 13,128 stroops charged per forward, against 19,449 with an auth entry. #60 adds two things on top: the watcher polls Horizon pages instead of streaming (section 5c), and the fee-bump bids exactly what the protocol requires (section 5d). The measurements in 5b and 5c were taken with #60's original auth-entry implementation.

## 1. The limit we hit

Every forward today is one Soroban transaction whose **source account is the pool**. Stellar accepts **one Soroban transaction per source account per ledger**, and a ledger closes about every 5–6 s. So:

```
1 pool = 1 forward per ledger ≈ 9–10 forwards/min
```

The burst tests measured exactly this:
- 12 deposits: 12 forwards in 12 ledgers.
- 50 deposits: 50 forwards in 50 ledgers, drained in 6m05s, p95 time-to-credit 5m25s.

The relayer's code isn't the bottleneck. It spends most of its time waiting for its one slot per ledger.

**Why it happens:** each transaction carries a **sequence number**, and the account's number must go up by exactly one per transaction. The network therefore processes transactions from one account strictly in order, and for Soroban it only admits one per ledger. The limit applies to the account whose sequence number the transaction uses (the *source*), not to whose money moves.

## 2. What a channel account is

A **channel** is an ordinary Stellar account used **only as the source** of transactions. It holds nothing but its minimum balance (~1.5 XLM) and has no power over anyone's funds.

In a Stellar transaction, the account that provides the sequence number doesn't have to be the account whose money moves:

```
Transaction
├─ source:      channel_7         ← provides the sequence number (and the per-ledger slot)
├─ fee payer:   pool (fee-bump)   ← pays the network fee
└─ operation:   SAC.transfer(from = pool, to = C-address, amount)
                └─ authorised by the pool's signature on an auth entry
```

The pool still owns and moves the money, and still has to sign. But the transaction uses **channel_7's** sequence number, so it takes **channel_7's** slot for the ledger, not the pool's. With 10 channels, 10 transfers out of the one pool can land in the same ledger.

For a Soroban call, the pool's signature goes on an **authorization entry**: a small signed statement that says "the pool approves `transfer(pool → C, amount)`". It includes a random **nonce** and an **expiry ledger**. This is the standard way to let one account act for another in Soroban.

### Why channels and not more pools

| | More pools (#34) | Channels (#48) |
|---|---|---|
| Measured / expected | 5 pools → 35–37/min (measured) | 10 channels → ~60–90/min (estimate) |
| Keys holding customer money | One per pool (5 hot keys) | **One** (the pool) |
| Addresses depositors pay | Several; intents are spread across pools | One |
| Funds to rebalance and reconcile | Split across pools | One balance |
| Cost of adding capacity | New funded pool key, config, redeploy | `CHANNEL_COUNT += n`, `make channels`, restart; ~1.5 XLM each |
| If a key leaks | That pool's deposits are drained | A channel loses ~1.5 XLM; **no customer funds at risk** |

Channels decouple **throughput** from **custody**. That's the point of #48.

## 3. How we have used channels before

**The general Stellar pattern.** The "channel accounts" pattern has long been used for high-volume classic payments, such as exchange withdrawals. The transaction source is a channel, the operation source is the main account, and both sign. Our recovery sweep (a classic `Payment`) can use it exactly like that.

**Our gasless service (#35, #47).** This was built and merged on this codebase already:
- **Derivation:** channels come from one `CHANNEL_SEED` via SEP-0005 paths `m/44'/148'/i'` (`internal/gasless/keys`). One secret recreates them all; no per-channel key storage.
- **Leases in Postgres** (`internal/gasless/channels`). `Acquire` takes the least-recently-used free channel with `FOR UPDATE SKIP LOCKED`, so two workers or instances never get the same one. A lease expires if its holder dies, and it records the channel's last sequence number. If every channel is busy, it returns `ErrPoolCapacity`.
- **Fees:** a separate funder pays via fee-bump. Channels never spend their own XLM.
- **Authorization:** the key that matters (the executor) signs only an Address-credential auth entry. It is never the transaction source.
- **Tooling:** `make channels` creates and merges channel accounts, and a balance monitor takes underfunded channels out of rotation.

#48 reuses all of that for deposits. The difference is that the account authorising the transfer is the **pool** (moving deposits), not the executor (calling FeeForwarder).

## 4. What changes in the relayer

The forward pipeline becomes:

1. **Lease** a channel. This replaces today's per-pool send lock and in-memory sequencer: each channel's sequence number lives with its lease.
2. **Build** `SAC.transfer(pool → C, amount)` with the **channel** as source.
3. **Simulate** it. RPC returns the auth entry the pool must sign.
4. **The pool signs the auth entry**, with a random nonce and an expiry about 100 ledgers ahead to cover retries. Re-simulate to get the final footprint and fee.
5. **The channel signs** the transaction. The **pool (or a funder) wraps it in a fee-bump** and signs that.
6. **Record the submission** (the #59 guard is unchanged), then **send** and **poll**. Release the lease with the new sequence number.

Sweeps use the classic version: channel as transaction source, `Payment` with operation source = pool, and both sign.

**A side benefit for #59.** The auth entry's random nonce makes every forward transaction unique, even with the same amount, destination and sequence. The memo couldn't give us that on Soroban. So the duplicate-hash case behind the missed credits disappears entirely, not just gets caught.

**Who pays fees:**
- **Recommended: the pool.** No new key, and it's the account paying fees today, so nothing changes in accounting.
- **Alternative: a separate funder**, as in gasless. This isolates fee spend from deposits, but adds a key and a float to top up.

## 5. Estimates

Assumptions:
- Ledgers close in ~6 s (our testnet runs saw 6–7 s).
- The #34 five-pool run reached ~72% of its theoretical 50/min (35/min). The same efficiency is applied to channels to allow for simulation time, occasional missed slots and retries.

| Setup | Theoretical | Expected | 50-deposit burst drains in | p95 time to credit |
|---|---|---|---|---|
| 1 pool (today, measured) | 10/min | **8.2–9.6/min** | ~6 min | ~5.5 min |
| 5 pools (#34, measured) | 50/min | **35–37/min** | ~1.5 min | ~1.2 min |
| 1 pool + 10 channels | 100/min | **~60–90/min** | ~35–60 s | ~45–60 s |
| 1 pool + 25 channels | 250/min | **~150–200/min** | ~15–25 s | ~25–35 s |
| Floor for a single deposit | — | — | — | ~10–15 s (detect + simulate + 1 ledger) |

Past about 25 channels, other limits take over before the ledger does:
- **Stellar RPC rate limits.** Every forward simulates twice. The public testnet RPC will throttle a big burst, and mainnet needs a paid RPC provider.
- **The relayer's own concurrency and database connections** (`DB_MAX_CONNS=20` by default).
- **Network-wide Soroban capacity and surge pricing** when mainnet is busy.

For our volumes, 10–25 channels is the right range: roughly **7–20× today's throughput**, with a single custody key.

These are estimates. The plan is to confirm them with `make burst N=50` and `N=200` after each step.

## 5b. Measured on testnet (2026-09-28)

**Setup:**
- A throwaway **pool** and N **channels**, all fresh accounts.
- Each channel sends `SAC.transfer(pool → C-address, 1 XLM)` as the inner transaction source.
- The pool signs only the Address-credential auth entry, and pays every fee via **fee-bump**.
- Each transfer takes two simulations: a recording pass to get the auth entry and nonce, then an enforcing pass with the signature to get the real footprint and fee.
- Everything goes through the public `soroban-testnet.stellar.org` RPC, protocol 28.

| Channels × rounds | Succeeded | Transfers per ledger | Rate | TRY_AGAIN_LATER | Simulate p95 (1st / 2nd) | Send → final p95 |
|---|---|---|---|---|---|---|
| 10 × 1 | 10/10 | 9 + 1 | 77/min | 0 | 690 ms / 294 ms | 6.8 s |
| 25 × 4 | 100/100 | **25, 25, 25, 25** | **308/min** | 0 | 684 ms / 398 ms | 5.3 s |
| 100 × 2 | 200/200 | **100, 100** | **986/min** | 0 | 883 ms / 877 ms | 5.7 s |

In every run, the destination's balance rose by exactly the number of transfers.

**What this answers:**
1. **One fee payer, many transactions per ledger: yes.** The pool fee-bumped 100 transactions in one ledger, with no rejections and no TRY_AGAIN_LATER.
2. **Many transfers out of one pool per ledger: yes.** 100 transfers all writing the pool's balance entry landed in one ledger. Soroban runs them in sequence; it doesn't reject them.
3. **The limit is the number of channels.** Throughput tracked N exactly up to 100 per ledger. Neither Stellar nor the public RPC pushed back at that level. RPC simulation latency rose a little (p95 about 0.3–0.9 s) but never failed.

So the estimates in section 5 were conservative. With channels, the relayer's own pipeline becomes the thing to keep out of the way:
- per-forward work must run in parallel (no pool-wide send lock or sequencer);
- the database pool must fit the concurrency;
- detecting deposits must keep up.

Past that, the only limits left are the RPC's (rate limits, simulation latency) and Stellar's (ledger capacity, surge pricing).

## 5c. Measured through the relayer (PR 1, 2026-09-28)

**Setup:** the relayer built from this branch, run locally against testnet with a throwaway pool and N channels created by `scripts/channels`. Load came from `scripts/burst`.

| Setup | Burst | Credited | Max forwards in a ledger | Drain | Rate | p95 credit |
|---|---|---|---|---|---|---|
| `main` (pool-sourced, deployed) | 50 | 50/50 | 1 | 6m05s | 8.2/min | 5m25s |
| 25 channels | 100 | 100/100 | **25** | 25 s | 240/min | 25 s |
| 100 channels, SSE watcher | 200 | 200/200 | 68 | 25 s | 480/min | 20 s |
| 100 channels, **polling watcher** | 200 | 200/200 | **100** | 25 s | 480/min | 20 s |

In every run:
- no forwards failed, and there were no retries and no contention;
- every forward was a distinct transaction;
- the destination's balance rose by exactly the burst total.

**The relayer limit we found and removed.** At 100 channels, forwards per ledger stalled at about 68. The relayer's own log showed the watcher handing deposits to the forwarder at only about 10 per second. Replaying the same payments straight from Horizon isolated the cause, which was the watcher's **SSE stream**:

| How the pool's payments are read from Horizon | Rate |
|---|---|
| SSE stream, default page (10) | ~9 per second |
| SSE stream, page 200 | ~21 per second |
| One paged request, 200 records | 200 in 1.6 s (~125 per second) |

The watcher now **polls pages** instead: 200 per request, the next page straight away when the page was full, otherwise every second. After the change it dispatched 161 deposits in one second, and forwards hit **100 per ledger, the channel count**.

**What limits throughput now:**
- **The channel count**, which is configuration. Each channel carries one forward at a time.
- **Channel turnaround, about 2 ledgers.** A channel is held until its transfer is confirmed. Two things add time there: confirmation polling (the SDK's back-off, 0.5–3.5 s) and the next forward's two simulations. So sustained throughput is about channels per 2 ledgers. The next lever is to confirm faster (a fixed 1 s poll) or to add channels.
- **External limits:** RPC simulation latency and rate limits, Horizon's request rate, and ledger capacity and surge pricing. None of them pushed back at 100 per ledger.

## 5d. The fee-bump bid (2026-09-28)

`txnbuild.NewFeeBumpTransaction` requires the outer per-operation fee to be at least the inner transaction's **whole** fee. For Soroban that includes the resource fee, and it then adds the resource fee again. So its bid was about `2 × inner fee + resource fee`, far above what the protocol requires: `2 × inner inclusion fee + resource fee`. #60 builds the fee-bump envelope directly, with that minimal bid.

| Forward (per transaction, stroops) | Bid (`max_fee`) | Charged |
|---|---|---|
| Pool-sourced (before channels) | 32,308 | 12,714 |
| Channel, auth entry (#60 as first written) | 108,382 | 19,449 |
| Channel, txnbuild bid (#55; computed from the same inner fee) | 86,824 | same as below |
| **Channel, minimal bid (#60 now)** | **42,408** | **13,128** |

The network charges the going rate, not the bid, so outside surge pricing the charge is about the same either way. The bid matters under **surge pricing**, when the inclusion fee rises toward it: the old bid would have let a busy network charge the pool more than twice as much per forward.

Verified on testnet: a 200-deposit burst through 100 channels was accepted at the minimal bid, and all 200 were credited. That run's throughput (141/min) was set by the public testnet RPC, which was degraded at the time: simulations hit the 15 s timeout, and even `getLatestLedger` took 2–5 s. It says nothing about the relayer.

## 6. Things to verify early (before building it all)

1. **Can one fee payer bump many transactions in the same ledger?** Gasless relies on this with its funder, but it hasn't been measured under load yet. Test it: sign 10 fee-bumped transfers with the pool as fee payer and send them in one ledger.
2. **Many transfers out of one pool in one ledger.** They all write the pool's balance entry. Soroban should run conflicting transactions one after another within the ledger rather than reject them. Confirm this with the same test.
3. **Auth-entry expiry under retries.** It must outlast the retry window, and a rebuilt transaction needs a fresh nonce.
4. **Separate channel seed from gasless.** Different processes need different seeds, so the two services never lease each other's accounts.

Items 1 and 2 can be answered with a ~100-line throwaway script on testnet, before any service code changes.
