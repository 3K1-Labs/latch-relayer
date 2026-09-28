-- deposit_channel_accounts: the deposit bridge's transaction-source accounts
-- (#48). Same schema as the gasless service's channel_accounts, in its own
-- table and derived from its own DEPOSIT_CHANNEL_SEED, so the two services
-- never lease each other's channels. A forward or sweep leases one channel as
-- its transaction source; the pool stays the payment's operation source and
-- fee-bumps the transaction. Stellar accepts one pending transaction per
-- source account, so throughput scales with the channel count instead of the
-- number of pool keys.
CREATE TABLE IF NOT EXISTS deposit_channel_accounts (
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
CREATE INDEX IF NOT EXISTS deposit_channel_accounts_lru_idx
    ON deposit_channel_accounts (last_used_at NULLS FIRST, hd_index) WHERE status = 'active';
