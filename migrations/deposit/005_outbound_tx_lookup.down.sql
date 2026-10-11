-- Rollback 005_outbound_tx_lookup: drop lookup indexes on forwards.
DROP INDEX IF EXISTS forwards_submitted_tx_idx;
DROP INDEX IF EXISTS forwards_forward_tx_idx;
