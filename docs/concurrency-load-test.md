# Concurrent on-ramp load test and what it found

Run on 13 Aug 2026 against Stellar testnet, on branch `perf/concurrent-forwarding`.

The question was whether latch-api and the relayer can handle a thousand
concurrent on-ramps. The short answer is no, and the reason turned out to be a
rule of the Stellar network rather than anything in our code.

## The headline

A single pool account can forward about **12 deposits per minute**, and no
amount of concurrency inside the relayer changes that. Stellar Core accepts at
most one Soroban transaction per source account per ledger, and a ledger closes
about every five seconds. One pool, one slot per ledger, twelve a minute.

Throughput scales with the **number of pool accounts**, not with threads,
queues, or clever sequencing.

## Numbers

All runs are fifty concurrent on-ramps, end to end: mint a session on latch-api,
confirm the relayer registered the memo, send a real testnet payment, wait for
the money to reach the destination contract. Success means the balance moved.

| Setup | Credited | Rate | p50 | p95 | Slowest |
|---|---|---|---|---|---|
| One pool, before any changes | 50/50 | 13.0/min | 1m50s | 3m40s | 3m50s |
| One pool, sequencer + bounded workers | 50/50 | 10.0/min | 2m0s | 4m50s | 5m0s |
| One pool, send and poll split apart | 50/50 | 9.7/min | 2m30s | 5m0s | 5m10s |
| **Five pools** | **50/50** | **33.3/min** | **40s** | **1m10s** | **1m30s** |

And the same five-pool setup at two hundred, to check the rate holds:

| Setup | Credited | Rate | p50 | p95 | Slowest |
|---|---|---|---|---|---|
| Five pools, n=200 | 200/200 | 35.3/min | 2m20s | 4m50s | 5m40s |

Every deposit was credited in every run. Nothing was swept to recovery and
nothing failed permanently. The problem was never correctness; it was speed.

The rate holding steady from fifty to two hundred is the useful part: it means
the pool count is the limit, not queueing somewhere that degrades under load.

## How we got there

The first theory was that concurrent forwards were colliding on the pool
account's sequence number. That was true and worth fixing, but fixing it did
not help, which is the interesting part.

**Sequence collisions were real.** Every forward used to ask Horizon for the
account's sequence number and build a transaction from it. Horizon reports the
state of the last closed ledger, so everything inside a five-second window read
the same number and built the same sequence. One landed, the rest were rejected
and fell into the retry queue, which drains one at a time on purpose.

**Fixing them did not help.** With a shared sequencer handing out consecutive
numbers, throughput went *down* slightly. The logs explained why: forty-seven
`TRY_AGAIN_LATER` responses in a five-minute run, and forwards landing exactly
one per ledger — 20:39:06, then :10, then :16. The network was refusing the
second transaction from that account in each ledger, whatever number it carried.

**That is the documented rule.** Stellar Core's Soroban transaction queue admits
one transaction per source account per ledger. The classic payment queue is more
permissive, which is why this is easy to get wrong. Sequence numbers were never
the binding constraint; the source account was.

**Five pools, and the ceiling moved.** Same code, same test, deposits spread
across five pool accounts: 33.3 per minute instead of 13.0. p95 fell from 3m40s
to 1m10s.

Five pools gave 2.6x rather than a clean 5x. The theoretical ceiling is about 60
a minute, so we are running at roughly half of it — the remaining loss is RPC
contention and the fact that the fifty deposits themselves take about fourteen
seconds to submit.

## What changed in the code

**A shared sequencer** (`internal/service/forwarder/sequence.go`). One Horizon
read per pool, then the sequence advances in memory. Every submitter draws from
it — the forward path, the recovery sweep, and the retry worker all sign as the
same account, so any of them reading independently would put the collision
straight back.

This did not lift the ceiling on its own, but it is still correct and worth
keeping: it removed the `txBadSeq` churn and the retry-queue detour that came
with it.

**A bounded worker pool** (`internal/service/watcher/watcher.go`). The watcher
used to start a goroutine per inbound payment with no limit. Now it hands work
to a fixed set of workers per pool. Unbounded goroutines against a rate-limited
RPC produce `TRY_AGAIN_LATER` storms, not throughput.

**Send split from poll.** Confirmation polling can take up to a minute and does
not touch the sequence, so it now happens outside the ordered window. Sends stay
strictly ordered per pool; everything slow overlaps.

**Round-robin pool selection** (`internal/handler/handler.go`). Intents used to
be pinned to `PoolAccounts[0]`, so extra pool accounts in the config did
nothing. This is the change that actually raised throughput.

**A retry-queue limit** (`internal/store/store.go`). `GetPendingRetries` loaded
the entire backlog on every tick. It now takes 200 at a time, oldest and
least-retried first.

## What this means for a thousand

At 33 a minute, a thousand deposits take about half an hour. To absorb a
thousand inside ten minutes — a hundred a minute — you need roughly **nine to
ten pool accounts**, assuming the same efficiency we measured.

That is a real operational cost, not just a config value. Each pool account is a
funded account with its own signing key that has to be held securely, monitored
for balance, and included in reconciliation. Nine of them is nine times that
surface. Worth deciding deliberately rather than by scaling a number until the
test passes.

The session-minting side is not a concern. Sessions were consistently minted in
under 100ms against a local relayer, and 50 concurrent mints took 87ms in total.

## How to run it

```bash
# relayer with its own database, on port 4000
cd latch-relayer && docker compose up -d

# latch-api pointed at it via RELAYER_URL=http://host.docker.internal:4000
cd latch-api && docker compose up -d

RELAYER_API_KEY=... go run ./scripts/onramp_load \
  -n 50 \
  -c C...,C...,C... \
  -credit-wait 10m
```

The harness funds a separate depositor account per run from friendbot. That
matters more than it looks: sharing one depositor serialises every payment
behind that account's own sequence number, the pool never sees concurrent
arrivals, and the test reports a clean result while measuring nothing.

## A caution about pool accounts

Do not point a second relayer at a pool another relayer is already watching. It
will not recognise the other's memos and will sweep those deposits to its
recovery address, and both will submit from the same account — which corrupts
exactly the measurement this test exists to make. These runs used an isolated
pool for that reason.
