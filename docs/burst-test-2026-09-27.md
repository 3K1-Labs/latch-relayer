# Burst test, 27 Sep 2026: throughput, and a missed-credit bug

Run against the deployed testnet relayer (`latch-relayer-zy93.onrender.com`, one pool: `GB3AET…6I5`) with `scripts/burst`.

How each run works:
- Intents are created directly on the relayer.
- Each deposit comes from its own depositor account.
- All deposits land in a single ledger.
- Timings come from ledger close times: when the deposit's ledger closed, and when its forward's ledger closed.

## Results

| Burst | Credited per relayer | Credited on chain | p50 | p95 | Max | Drain | Rate |
|---|---|---|---|---|---|---|---|
| 12 | 12/12 | 12/12 | 35s | 1m15s | 1m15s | 1m15s | 9.6/min |
| 50 | 50/50 | **47/50** | 2m20s | 5m5s | 5m45s | 5m45s | 8.7/min |

**Throughput is what we expected.** Forwards go out one per ledger, from one pool: 12 forwards in 12 ledgers, and 50 in 47. That is roughly 9–10 a minute per pool at testnet's current ledger rate. With one pool, time-to-credit grows linearly with how many deposits are queued ahead, about 6–7 s per deposit.

## Bug: two deposits can settle on one transfer

In the 50-deposit run, the relayer reported every forward `done`, and every intent `completed`. But three pairs of deposits recorded the **same** `forward_tx`. Each of those transactions contains **one** 1 XLM transfer. The destination's native balance proves it: 61.3201816 XLM against the 64.3201816 expected. Three deposits were never paid, and their XLM is still in the pool.

| forward_tx | seq | Deposits claiming it (memo) |
|---|---|---|
| `fbc61a79…12dc` | …744410 | 3332647881470652592, 2764367488104516236 |
| `dd924c29…2eb8` | …744423 | 2540523193621476320, 3803953969530060000 |
| `8ff3a133…3c53` | …744428 | 3766131760030639515, 4930895340176482188 |

In the same run, `relayer_pool_contention_total` went up by **3**.

### Likely mechanism
A forward's transaction is made up of:
- the pool (source)
- a sequence number
- the destination C-address
- the amount
- the fee
- an expiry rounded to whole seconds (`validUntil`)

Two forwards with the same destination and amount that draw the **same sequence number** within the same second produce byte-identical transactions, and so the same hash. Then:
1. Forward A sends seq N, and the network returns `PENDING`.
2. Forward B fails on the pool slot (contention). `transfer` resyncs the sequencer from Horizon. Horizon still reports the ledger before A's, so the sequencer hands out N again.
3. Forward C builds with seq N: the same bytes as A, so the same hash. `signAndSend` receives `DUPLICATE`, which it treats as success (`forwarder.go`, `case "DUPLICATE"`).
4. `pollResult` finds A's transfer succeeded, and both A and C are marked `done`.

The sequencer re-reading a stale sequence is the trigger; `transfer`'s own comments already name this hazard. Treating `DUPLICATE` as "mine" is what turns a harmless collision into a missed credit.

**Real-world exposure:**
- **Forwards:** two deposits of the same amount to the same smart account close together, during pool contention. For example, a user funds twice, or an on-ramp splits an order.
- **Recovery sweeps:** these are the same shape (pool → recovery address, same amount, memo `unknown-memo`), so two same-amount unroutable deposits can collapse into one sweep.

### Fix
Fixed in #59:
- **One on-chain transaction settles at most one forward.** `RecordSubmission` refuses a hash that another forward holds, in flight or settled.
- The losing forward sends nothing and is re-queued as contention, without spending its retry budget.
- Regression tests cover this at both the forwarder and the store level.

Considered and ruled out:
- **Make every transfer unique with the deposit hash as a memo.** Soroban transactions reject memos; testnet RPC returns "Soroban transactions do not support memos". Recovery sweeps are classic payments, and `main` already gives them this memo.

Follow-up: stop the resync from handing out a number that is still in flight. For example, remember the highest accepted sequence until its deadline passes. This saves the wasted retries; correctness no longer depends on it.

### Reconciliation
The 3 XLM for the memos above is still in pool `GB3AET…6I5`, and the relayer database shows those forwards as `done`. This is testnet, but the same state on mainnet would need a manual re-forward.
