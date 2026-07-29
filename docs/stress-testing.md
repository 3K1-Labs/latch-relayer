# Relayer Stress Test Ideas

This is a planning note for future load and reliability testing. The goal is to understand not only how much traffic the relayer can handle, but whether every deposit reaches a correct terminal state: completed, failed with recovery, or pending retry with a clear path forward.

## 1. API Load Test

Hammer `POST /intents` and `GET /deposit/status/{memo_id}` without sending Stellar deposits.

This tests:

- Render web service capacity
- Neon database insert/read behavior
- memo generation uniqueness
- HTTP latency and error rates
- basic request handling under load

Useful tools:

- `k6`
- `hey`
- `vegeta`
- a small custom Go script

Question it answers:

Can the relayer create and read many intents quickly without database contention, duplicate memo IDs, or elevated error rates?

## 2. Valid Deposit Burst Test

Create many intents, then submit many Stellar payments to the pool account using the returned memo IDs.

This tests the core relayer path:

- Horizon watcher pickup for many incoming payments
- forward record creation
- Soroban transfer submission
- retry behavior
- pool account sequence-number handling
- Stellar Horizon/RPC rate limits

Question it answers:

Can a burst of real deposits be detected, recorded, forwarded, and marked completed without dropped or stuck records?

Suggested ramp:

- 20 deposits
- 50 deposits
- 100 deposits
- higher only after observing clean terminal states

## 3. Same-Account Sequence Stress

Send many deposits very quickly from one depositor account.

This mostly tests transaction submission sequencing from the depositor side. Stellar accounts require correct sequence numbers, so rapid submissions from one account can fail before the relayer has anything meaningful to process.

Question it answers:

How much of the test harness or upstream sender flow breaks when one account submits many transactions rapidly?

Better variant:

Use many funded depositor accounts and send in parallel, so the relayer is stressed more than the source account sequence number.

## 4. Pool Forwarding Contention Test

Send many valid deposits to one pool address at nearly the same time, forcing the relayer to submit many outgoing forwards from the same pool account.

This is one of the most important tests for this relayer because the pool account also has Stellar sequence-number constraints.

This tests:

- whether forwarding is serialized or safely coordinated
- whether retries recover from sequence conflicts
- whether any forwards get stuck in `pending` or `pending_retry`
- whether the pool account can drain deposits reliably under burst load

Question it answers:

Can the relayer safely handle many outgoing forwards from the same pool account without sequence-number failures causing lost or stuck work?

## 5. Recovery / Sweep Stress

Send many invalid or unmatched deposits.

Cases:

- missing memo
- wrong memo type
- unknown memo ID
- expired intent memo

This tests:

- recovery address forwarding behavior
- invalid deposit classification
- database records for failed/recovered flows
- whether bad traffic can clog valid traffic

Question it answers:

Can the relayer consistently handle bad deposits without blocking normal forwarding?

## 6. Soak Test

Run moderate traffic for hours instead of a large short burst.

This tests:

- Render free-tier sleep/wake behavior
- Horizon SSE reconnect and cursor resume
- memory growth
- database connection stability
- retry worker behavior over time
- long-running watcher stability

Question it answers:

Does the relayer remain healthy and eventually consistent over a long period of realistic traffic?

## Recommended First Real Test

Start with a controlled valid deposit burst:

1. Create 20 intents.
2. Send 20 testnet deposits using multiple depositor accounts if possible.
3. Poll all statuses until each reaches a terminal state.
4. Record completed, pending, pending_retry, and failed counts.
5. Repeat with 50, then 100.

The main success metric should be correctness first and speed second. Every deposit should either complete or land in an explainable recovery/retry state.
