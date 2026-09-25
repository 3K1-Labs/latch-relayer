-- The outbound transaction a forward has put on the network but not yet seen
-- settle, recorded before it is sent. Once a transfer may be in flight, a
-- retry must look this hash up rather than build a new transfer: the first one
-- stays valid until submitted_until and can still land, so a second one would
-- pay the C-address twice. Both are cleared when the transaction settles or is
-- known never to have been queued.
ALTER TABLE forwards ADD COLUMN IF NOT EXISTS submitted_tx    TEXT;
ALTER TABLE forwards ADD COLUMN IF NOT EXISTS submitted_until TIMESTAMPTZ;
