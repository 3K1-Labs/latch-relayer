# Free Deployment Guide

This mirrors latch-api's deployment (`latch-api/docs/deployment.md`) on the same free stack:

| Service     | Provider | Purpose                                    |
| ----------- | -------- | ------------------------------------------- |
| App hosting | Render   | Runs the Go relayer via Docker              |
| PostgreSQL  | Neon     | Managed serverless Postgres (registrations, forwards, cursors) |

No Redis needed here — unlike latch-api, latch-relayer has no OTP/rate-limit/price-cache concerns.

> **Note:** Render's free web service spins down after 15 minutes of inactivity and takes ~30 seconds to wake on the next request. Acceptable for development/testing; upgrade to a paid tier before this backs real production deposits (a sleeping relayer means a delayed deposit-forward, not a lost one — Horizon SSE resumes from the saved cursor on wake — but a paid always-on tier is the right call before this handles real user funds).

---

## Prerequisites

- GitHub account with the latch-relayer repo pushed to it
- A funded pool account keypair (public + secret key) — see [Generating pool account keys](#generating-pool-account-keys) below if you don't have one yet
- A recovery address (any Stellar G-address you control) to catch deposits with unrecognized/missing memos

---

## Step 1 — PostgreSQL on Neon

1. Go to [neon.tech](https://neon.tech) and click **Sign Up** (GitHub login works).
2. Click **New Project**, name it (e.g. `latch-relayer`), pick a region, **Create Project**.
3. **Connect** → **Connection string** → copy the URL:
   ```
   postgres://username:password@ep-xxx.us-east-2.aws.neon.tech/neondb?sslmode=require
   ```
4. Save this as `DATABASE_URL`.

**Unlike latch-api, there's no separate migrate step.** `latch-relayer` embeds its schema (`migrations/001_init.up.sql` via `//go:embed`) and applies it automatically on every boot (`IF NOT EXISTS` throughout, so it's safe to run on every restart). The first deploy will create the schema itself.

---

## Step 2 — Push to GitHub

If not already pushed:

```bash
git remote add origin git@github.com:3000-Labs/latch-relayer.git
git branch -M main
git push -u origin main
```

---

## Step 3 — Deploy on Render

1. [render.com](https://render.com) → **Sign Up** (GitHub login works).
2. **New** → **Web Service** → connect GitHub → select `latch-relayer`.
3. Service settings:

   | Field               | Value                    |
   | ------------------- | ------------------------ |
   | **Name**            | `latch-relayer`          |
   | **Region**          | Same as Neon             |
   | **Branch**          | `main`                   |
   | **Runtime**         | **Docker**               |
   | **Dockerfile path** | `./Dockerfile`           |
   | **Instance Type**   | **Free**                 |

4. **Environment Variables** — add each, one pool account pair shown (add more as `POOL_ADDRESS_2`/`POOL_PRIVATE_KEY_2` etc. if running multiple pools):

   ```
   NETWORK                testnet
   HORIZON_URL             https://horizon-testnet.stellar.org
   RPC_URL                 https://soroban-testnet.stellar.org
   DATABASE_URL            postgres://...neon.tech/neondb?sslmode=require
   PORT                    4000
   POOL_ADDRESS_1          <pool public key, G...>
   POOL_PRIVATE_KEY_1      <pool secret key, S...>
   RECOVERY_ADDRESS        <a G-address you control, for unmatched-memo deposits>
   ```

   **`POOL_PRIVATE_KEY_1` is a live signing key controlling pooled funds — paste it directly into Render's dashboard, never into a commit, a chat log, or any file that gets checked in.** For testnet this only ever holds Friendbot-funded test XLM, but treat the habit as if it were real from day one.

5. **Create Web Service.** First deploy takes 3-5 minutes.

6. Your relayer URL will be shown at the top of the service page:
   ```
   https://latch-relayer.onrender.com
   ```

---

## Step 4 — Verify the deployment

```bash
curl https://latch-relayer.onrender.com/health
# Expected: {"status":"ok"}
```

```bash
curl -X POST https://latch-relayer.onrender.com/register \
  -H "Content-Type: application/json" \
  -d '{"c_address":"CCOX4AG3XESDAZC7L27AMQZ6KKMUWEU2KCHFXJ2PXNAXMDUCL225MN2P"}'
# Expected: {"memo_id":"...","pool_address":"..."}
```

---

## Step 5 — Point latch-api at the live relayer

In whichever environment latch-api is deployed to (Render, per `latch-api/docs/deployment.md`), set:

```
RELAYER_URL=https://latch-relayer.onrender.com
```

`RELAYER_URL` is unset by default, so nothing changes for latch-api until this is deliberately configured.

---

## Re-deploying

Render auto-deploys on every push to `main`. Manual trigger: Render dashboard → **Manual Deploy** → **Deploy latest commit**.

---

## Generating pool account keys

`scripts/keygen/main.go` generates a fresh keypair pair for testing (a pool/hot account, and a separate depositor account to simulate an inbound deposit):

```bash
go run ./scripts/keygen
```

Fund both on testnet via [Friendbot](https://laboratory.stellar.org/#account-creator?network=test) (free, testnet-only):

```bash
curl "https://friendbot.stellar.org/?addr=<PUBLIC_KEY>"
```

**Never use a Friendbot-funded/testnet-generated keypair for a mainnet pool account.** Generate a separate keypair for mainnet and fund it with real XLM through a real on-ramp when that day comes.

---

## Upgrading away from the free tier

Before this relayer backs real (mainnet) deposits, move off Render's free tier (spin-down risk) to at least the **Starter** plan ($7/month, always-on). Neon's free tier (512MB storage) is generous enough for `registrations`/`forwards`/`cursors` row volumes for a long time.
