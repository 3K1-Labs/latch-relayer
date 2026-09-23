-- channel_accounts: the gasless service's pool of transaction-source accounts.
-- Derived from CHANNEL_SEED at m/44'/148'/hd_index'. A channel is leased to one
-- in-flight sponsored transaction at a time (Stellar accepts one pending
-- transaction per source account), which is what makes throughput scale with
-- the channel count across any number of gasless instances.
CREATE TABLE IF NOT EXISTS channel_accounts (
    id                 SERIAL      PRIMARY KEY,
    hd_index           INT         NOT NULL UNIQUE,
    address            TEXT        NOT NULL UNIQUE,
    -- active: leasable. disabled: temporarily out (missing on-chain, balance
    -- below floor). retired: index >= CHANNEL_COUNT.
    status             TEXT        NOT NULL DEFAULT 'active'
                                   CHECK (status IN ('active', 'disabled', 'retired')),
    disabled_reason    TEXT,
    -- Last sequence number this service knows the account consumed. NULL or
    -- needs_resync = true means "reload from the network before building".
    seq                BIGINT,
    needs_resync       BOOLEAN     NOT NULL DEFAULT TRUE,
    -- Lease: set while a transaction built on this channel is in flight.
    lease_token        TEXT,
    leased_by          TEXT,
    lease_expires_at   TIMESTAMPTZ,
    last_used_at       TIMESTAMPTZ,
    balance_stroops    BIGINT,
    balance_checked_at TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Acquire scans active channels least-recently-used first.
CREATE INDEX IF NOT EXISTS channel_accounts_lru_idx
    ON channel_accounts (last_used_at NULLS FIRST, hd_index) WHERE status = 'active';
