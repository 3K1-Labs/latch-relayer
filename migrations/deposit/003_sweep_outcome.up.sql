-- sweep: the deposit could not be credited (unknown memo_id, expired intent)
-- and is being returned to the recovery account instead. Persisted when that
-- decision is made so a retry after a crash sweeps it rather than re-deciding.
ALTER TABLE forwards ADD COLUMN IF NOT EXISTS sweep BOOLEAN NOT NULL DEFAULT false;

-- swept: the recovery payment landed; forward_tx holds its hash. Previously a
-- sweep was recorded as 'failed' whether or not the payment went through.
ALTER TABLE forwards DROP CONSTRAINT IF EXISTS forwards_status_check;
ALTER TABLE forwards ADD CONSTRAINT forwards_status_check
    CHECK (status IN ('pending', 'done', 'failed', 'pending_retry', 'swept'));
