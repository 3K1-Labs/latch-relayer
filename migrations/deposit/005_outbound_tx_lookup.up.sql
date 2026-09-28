-- RecordSubmission refuses an outbound hash another forward already holds, in
-- flight (submitted_tx) or settled (forward_tx), so one transfer can never pay
-- two deposits. These keep that check an index lookup as the table grows.
-- Deliberately not UNIQUE: databases that ran before the check existed can
-- already hold two settled forwards on one forward_tx (the case it prevents),
-- and a unique index would refuse to build on them and stop the service booting.
CREATE INDEX IF NOT EXISTS forwards_submitted_tx_idx ON forwards (submitted_tx) WHERE submitted_tx IS NOT NULL;
CREATE INDEX IF NOT EXISTS forwards_forward_tx_idx ON forwards (forward_tx) WHERE forward_tx IS NOT NULL;
