-- Rollback 004_landed_at_sweep_reason: remove landed_at and sweep_reason columns.
ALTER TABLE forwards DROP COLUMN IF EXISTS landed_at;
ALTER TABLE forwards DROP COLUMN IF EXISTS sweep_reason;
