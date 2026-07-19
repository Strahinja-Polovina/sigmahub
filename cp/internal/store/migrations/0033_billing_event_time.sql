-- SIGMA-99: track the Paddle event's occurred_at so an out-of-order delivery (a
-- delayed or retried OLDER event, which has a distinct delivery id and so isn't
-- deduped) can't overwrite newer subscription state.
ALTER TABLE org_billing ADD COLUMN IF NOT EXISTS last_event_at TIMESTAMPTZ;
