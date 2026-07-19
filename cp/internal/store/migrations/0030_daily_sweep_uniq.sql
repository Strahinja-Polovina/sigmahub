-- SIGMA-72: enforce "once per day" for the scheduled sweeps at the DB level, so
-- two concurrent sweep runs (multi-replica, or an overlapping manual trigger)
-- can't both pass the in-tx EXISTS check and double-insert. Partial unique
-- indexes on the UTC calendar day; restore / restore-pitr runs and non-measure
-- s3 ops are intentionally excluded — they may legitimately repeat within a day.
-- The expression matches the ON CONFLICT target the inserts now use.
CREATE UNIQUE INDEX IF NOT EXISTS backup_runs_daily_uniq
    ON backup_runs (policy_id, kind, ((created_at AT TIME ZONE 'UTC')::date))
    WHERE kind IN ('backup', 'basebackup', 'verify');

CREATE UNIQUE INDEX IF NOT EXISTS pending_s3_ops_measure_daily_uniq
    ON pending_s3_ops (resource_id, bucket, ((created_at AT TIME ZONE 'UTC')::date))
    WHERE action = 'measure';
