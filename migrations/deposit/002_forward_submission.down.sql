-- Rollback 002_forward_submission: drop submitted_tx and submitted_until columns.
ALTER TABLE forwards DROP COLUMN IF EXISTS submitted_tx;
ALTER TABLE forwards DROP COLUMN IF EXISTS submitted_until;
