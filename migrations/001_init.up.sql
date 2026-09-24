-- intents: one row per funding session.
-- memo_id is a random uint64 stored as BIGINT (Go casts uint64 ↔ int64, preserving bits).
CREATE TABLE IF NOT EXISTS intents (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    memo_id      BIGINT      NOT NULL UNIQUE,
    c_address    TEXT        NOT NULL,
    pool_address TEXT        NOT NULL,
    expected_amt TEXT,
    expires_at   TIMESTAMPTZ NOT NULL,
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'completed', 'expired', 'failed')),
    external_id  TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS intents_memo_id_idx    ON intents (memo_id);
CREATE INDEX IF NOT EXISTS intents_status_idx     ON intents (status);
CREATE INDEX IF NOT EXISTS intents_expires_at_idx ON intents (expires_at);

-- forwards: one row per inbound payment seen by the watcher.
-- tx_hash is the idempotency key — ON CONFLICT DO NOTHING on insert.
CREATE TABLE IF NOT EXISTS forwards (
    id           BIGSERIAL   PRIMARY KEY,
    tx_hash      TEXT        NOT NULL UNIQUE,
    memo_id      BIGINT      NOT NULL,
    from_address TEXT        NOT NULL,
    amount       TEXT        NOT NULL,
    asset        TEXT        NOT NULL DEFAULT 'native',
    forward_tx   TEXT,
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'done', 'failed', 'pending_retry')),
    retries      INT         NOT NULL DEFAULT 0,
    error        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS forwards_memo_id_idx ON forwards (memo_id);
CREATE INDEX IF NOT EXISTS forwards_status_idx  ON forwards (status);

-- cursors: tracks the Horizon SSE paging position per pool address.
-- On restart the watcher resumes from here instead of replaying all history.
CREATE TABLE IF NOT EXISTS cursors (
    pool_address TEXT        PRIMARY KEY,
    cursor       TEXT        NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The pool account that actually received each inbound payment. Deposits are
-- spread across pools (round-robin at intent creation), and a depositor can pay
-- a different pool than the one their intent named, so the forward and any
-- recovery sweep must move money out of the pool that holds it — not the
-- intent's pool, and never a fixed PoolAccounts[0]. NULL on rows recorded
-- before this column existed; those fall back to the intent's pool.
ALTER TABLE forwards ADD COLUMN IF NOT EXISTS pool_address TEXT;
