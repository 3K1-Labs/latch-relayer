-- Forward mode: what the user reimbursed through FeeForwarder, in the fee
-- token's own units (7 decimals). NULL for sponsored submissions, where Latch
-- pays and the user is charged nothing.
ALTER TABLE sponsored_transactions
    ADD COLUMN IF NOT EXISTS user_fee_token  TEXT,
    ADD COLUMN IF NOT EXISTS user_fee_symbol TEXT,
    ADD COLUMN IF NOT EXISTS user_fee_amount BIGINT;
