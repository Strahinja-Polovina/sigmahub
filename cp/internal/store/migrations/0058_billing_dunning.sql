-- SIGMA-295: give a delinquent subscription a clock and a dunning cursor.
--
-- Before this, org_billing knew a status and nothing about WHEN it started. A
-- past_due transition enqueued exactly one alert, a canceled transition enqueued
-- none, and no code path anywhere consulted the status before creating a server.
-- A customer could cancel and keep every capability — deploys, certificates,
-- backups, restore-verifies, telemetry — indefinitely and for free.
--
-- updated_at could not stand in for either column: it moves on any write,
-- including RecordQuantitySynced, so a delinquent org's clock would reset every
-- time the quantity sweep touched the row.

-- When the CURRENT status was first entered. The grace period is measured from
-- here. Existing rows are backfilled to updated_at rather than now(): an org
-- that has been past_due for a month should not be handed a fresh grace period
-- by a deploy of this migration, and updated_at is the closest evidence we have.
ALTER TABLE org_billing
    ADD COLUMN IF NOT EXISTS status_since TIMESTAMPTZ;
UPDATE org_billing SET status_since = updated_at WHERE status_since IS NULL;
ALTER TABLE org_billing
    ALTER COLUMN status_since SET DEFAULT now(),
    ALTER COLUMN status_since SET NOT NULL;

-- When the last dunning reminder was enqueued, so the reminder repeats on a
-- schedule rather than on every 10-minute sweep pass. NULL = never reminded
-- since the transition (the transition alert itself sets it).
ALTER TABLE org_billing
    ADD COLUMN IF NOT EXISTS dunning_last_at TIMESTAMPTZ;

-- The delinquent-orgs view scans by status; small table, but the sweep runs
-- every pass and this keeps it off a seq scan as tenants grow.
CREATE INDEX IF NOT EXISTS org_billing_delinquent_idx
    ON org_billing (status, status_since)
    WHERE status IN ('past_due', 'canceled');
