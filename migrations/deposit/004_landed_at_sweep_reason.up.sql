-- landed_at: when the inbound payment closed on-chain. Expiry is judged by it,
-- and a retry re-checks it, so a deposit that arrived after its intent expired
-- is returned even if its first attempt died before deciding. NULL on rows
-- recorded before this column existed; those keep the old behaviour.
ALTER TABLE forwards ADD COLUMN IF NOT EXISTS landed_at TIMESTAMPTZ;

-- sweep_reason: why a deposit is being returned (unknown memo_id, intent
-- expired, unsupported asset). Kept apart from `error`, which retries
-- overwrite, so the reason survives to the final 'swept' record.
ALTER TABLE forwards ADD COLUMN IF NOT EXISTS sweep_reason TEXT;
