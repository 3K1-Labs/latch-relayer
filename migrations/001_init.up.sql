-- registrations: maps a C-address to its derived memo_id and pool account.
-- memo_id is stored as BIGINT (signed). Go casts uint64 ↔ int64 preserving bits.
CREATE TABLE IF NOT EXISTS registrations (
    memo_id      BIGINT      PRIMARY KEY,
    c_address    TEXT        NOT NULL UNIQUE,
    pool_address TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

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
