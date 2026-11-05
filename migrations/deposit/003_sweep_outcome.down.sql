-- Rollback 003_sweep_outcome: remove sweep column and revert status constraint.
ALTER TABLE forwards DROP COLUMN IF EXISTS sweep;
ALTER TABLE forwards DROP CONSTRAINT IF EXISTS forwards_status_check;
ALTER TABLE forwards ADD CONSTRAINT forwards_status_check
    CHECK (status IN ('pending', 'done', 'failed', 'pending_retry'));
