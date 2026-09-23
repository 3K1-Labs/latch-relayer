# Gasless service: keys, channels and rotation

The gasless service (`cmd/gasless`) is the executor for the FeeForwarder contract (latch-contracts `fee-forwarder/`). It is a **separate process** from the deposit bridge (`cmd/serve`), with its own env file (`gasless.env`), database, API key and deployment. It refuses to start if any `POOL_*` or `RECOVERY_ADDRESS` variable is present, so pool keys, which control user deposits, are never loaded into it.

## The three kinds of key

| Key | Env | Holds | On-chain role | If leaked |
|---|---|---|---|---|
| **Executor** | `EXECUTOR_ADDRESS` / `EXECUTOR_PRIVATE_KEY` | base reserve only | `executor` on FeeForwarder | Attacker can call `forward()` with *already user-signed* trees, choosing `fee_amount` up to each user's signed `max_fee_amount`. Can't forge user signatures, move user funds, or take the fees (they accrue in the contract). Revoke the role. |
| **Funder** | `FUNDER_ADDRESS` / `FUNDER_PRIVATE_KEY` | the XLM float | none | Attacker drains the float. No user funds at risk. |
| **Channels** | `CHANNEL_SEED` + `CHANNEL_COUNT` | ~1.5 XLM each | none | Attacker can burn channel sequence numbers (a nuisance) and take ~1.5 XLM each. |

Compare the deposit bridge's **pool keys**, whose leak drains pooled user deposits. That difference in blast radius is why the services are separate.

## How a sponsored transaction uses them (P4)

- The **transaction source** is a leased **channel**: one in-flight transaction per channel. Stellar accepts one pending transaction per source account, so throughput ≈ `CHANNEL_COUNT` transactions per ledger.
- The **fee source** is the **funder**, which wraps each transaction in a fee-bump.
- The **executor** signs the relayer's authorization entry for `forward()` explicitly (Address credentials), so the executor doesn't need to be the transaction source.
- **Adding capacity needs no contract admin action:** channels hold no role. Raise `CHANNEL_COUNT`, run `make channels`, restart.

## Setting up (testnet)

1. **Executor.** Testnet already has one granted: `GBLDLFA2Y3RXGL3LZPFTZYDCAE5OZRUVDLBRWAZGA7ZRWT7BGSA7IHMT` (latch-contracts `docs/BUILD.md`). Put its key in `EXECUTOR_*`. It must exist on-chain; fund it with friendbot if needed.
2. **Funder.** Create a fresh account, fund it (testnet: `https://friendbot.stellar.org/?addr=G...`), and put its key in `FUNDER_*`.
3. **Channel seed.** Run `openssl rand -hex 32` and set `CHANNEL_SEED`. Set `CHANNEL_COUNT`.
4. **Create the channels.** `make channels` (or `make channels N=25`). This creates any missing ones from the funder, and is safe to re-run after a testnet reset.
5. **Start the service.** `make run-gasless`.

To check the role encoding against the real testnet contract, run:

```bash
GASLESS_TESTNET=1 go test -run TestTestnetFeeForwarderRoles ./internal/gasless/chain/
```

It needs no keys: it funds a throwaway account from friendbot.

At boot the service checks the network passphrase, that the executor and funder exist, and that the executor **holds the `executor` role** (a simulated `has_role`). Any failure refuses the boot with a message saying what to fix. On Render a failed boot keeps the previous deploy serving.

## Operating

- **`GET /health`** (public): `ok` / `degraded`. It returns 503 only if the database is unreachable.
  - "Degraded" means the funder is under `FUNDER_MIN_XLM`, or no channel is in rotation.
  - Deliberately *not* a restart trigger: restarting doesn't top up XLM.
- **`GET /gasless/status`** (API key): funder and executor balances, channel counts, and which accounts are in use.
- **`GET /metrics`** (API key), to alert on:
  - `latch_gasless_funder_balance_stroops`: well above the floor
  - `latch_gasless_sponsorship_available == 0`
  - `latch_gasless_channels{state="disabled"} > 0`
- **Balance monitor** (every `BALANCE_CHECK_SECONDS`):
  - Takes a channel out of rotation if it's missing on-chain or under `CHANNEL_MIN_XLM`, and puts it back once healthy.
  - Stops sponsorship while the funder is under its floor.
- **Top-ups** come from a treasury account that is **never** loaded into the relayer. Keep the funder float small: a few days of expected fees plus margin.

## Rotation

- **Channels** (routine or suspected leak):
  1. Generate a new `CHANNEL_SEED`.
  2. Run `make channels` with the new seed.
  3. Deploy.
  4. With the *old* seed, run `make channels N=0 MERGE_TO=<old count>` to merge the old channels back into the funder.

  The channel table keys on derivation index; a changed address at an index resets that row, so in-flight leases on old channels should drain first (deploy during low traffic).
- **Funder:**
  1. Create and fund a new funder.
  2. Swap `FUNDER_*`.
  3. Deploy.
  4. Merge the old funder into the treasury.

  No contract change is needed.
- **Executor** (needs the cold `admin` key, latch-contracts#84):
  1. The admin calls `grant_role(new_executor, "executor")`.
  2. Swap `EXECUTOR_*` and deploy. Preflight confirms the new key holds the role.
  3. The admin calls `revoke_role(old_executor, "executor")`.

  As deployed, only `admin` can grant or revoke `executor` (no role admin is set).

## Mainnet: signing backend

Testnet uses env-loaded seeds. Mainnet should not.
- All signing goes through `internal/signer.Signer` (`Address()`, `Sign(ctx, hash)`), so a KMS-backed implementation can replace the in-memory keypair without changing callers.
- **Executor and funder move first:** they sign most often and guard the most.
- The candidates are GCP KMS or Vault Transit (both support Ed25519). AWS KMS supports Ed25519 only in newer key specs; confirm before choosing.
- Channels can stay seed-derived: they hold ~1.5 XLM and no role.
- Tracked with GAPS.md "Private keys loaded into plain heap memory" (P1).
