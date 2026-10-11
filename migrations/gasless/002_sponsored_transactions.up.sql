-- sponsored_transactions: one row per submission latch-api asks the gasless
-- service to pay for. request_id makes a submission idempotent; the rows are
-- also the ledger the sponsorship caps are counted from, so they survive
-- restarts and are shared by every gasless instance.
CREATE TABLE IF NOT EXISTS sponsored_transactions (
    request_id          TEXT        PRIMARY KEY,
    -- sha256 of the request body: the same request_id with a different body
    -- is refused instead of returning the other transaction's outcome.
    payload_hash        TEXT        NOT NULL,
    wallet              TEXT        NOT NULL,
    mode                TEXT        NOT NULL,
    target_contract     TEXT        NOT NULL,
    target_function     TEXT        NOT NULL,
    -- pending:     reserved, being submitted.
    -- success:     landed and succeeded.
    -- failed:      landed and failed, or will never land; fee may be charged.
    -- unconfirmed: sent, but the outcome was not seen; it may still land.
    -- rejected:    refused before anything was sent; costs nothing.
    status              TEXT        NOT NULL DEFAULT 'pending'
                                    CHECK (status IN ('pending', 'success', 'failed', 'unconfirmed', 'rejected')),
    tx_hash             TEXT,
    -- Upper bound on what this submission can cost, reserved before sending
    -- so concurrent submissions can't overrun a cap together.
    fee_reserved_stroops BIGINT     NOT NULL,
    -- What the network actually charged, once known.
    fee_charged_stroops BIGINT,
    error_code          TEXT,
    error_message       TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-wallet cap: count and spend by wallet.
CREATE INDEX IF NOT EXISTS sponsored_transactions_wallet_idx
    ON sponsored_transactions (wallet) WHERE status <> 'rejected';

-- Daily budget: spend over the last 24 hours.
CREATE INDEX IF NOT EXISTS sponsored_transactions_created_idx
    ON sponsored_transactions (created_at) WHERE status <> 'rejected';
