-- SIGMA-72: enforce "once per day" for the scheduled sweeps at the DB level, so
-- two concurrent sweep runs (multi-replica, or an overlapping manual trigger)
-- can't both pass the in-tx EXISTS check and double-insert. Partial unique
-- indexes on the UTC calendar day; restore / restore-pitr runs and non-measure
-- s3 ops are intentionally excluded — they may legitimately repeat within a day.
-- The expression matches the ON CONFLICT target the inserts now use.

-- SIGMA-86: de-duplicate any pre-existing same-day rows BEFORE creating the
-- partial unique indexes. Without this, CREATE UNIQUE INDEX aborts with a unique
-- violation on any database that already hit the pre-0030 race the index closes
-- — and since a failed migration rolls back (its filename is never recorded) the
-- whole chain retries and never advances, bricking boot. Keep the physically
-- first row (min ctid) per key; the discarded duplicates were redundant sweep
-- rows for the same policy/kind/day (or resource/bucket/day) anyway.
DELETE FROM backup_runs
 WHERE kind IN ('backup', 'basebackup', 'verify')
   AND ctid NOT IN (
     SELECT min(ctid) FROM backup_runs
      WHERE kind IN ('backup', 'basebackup', 'verify')
      GROUP BY policy_id, kind, ((created_at AT TIME ZONE 'UTC')::date)
   );

DELETE FROM pending_s3_ops
 WHERE action = 'measure'
   AND ctid NOT IN (
     SELECT min(ctid) FROM pending_s3_ops
      WHERE action = 'measure'
      GROUP BY resource_id, bucket, ((created_at AT TIME ZONE 'UTC')::date)
   );

CREATE UNIQUE INDEX IF NOT EXISTS backup_runs_daily_uniq
    ON backup_runs (policy_id, kind, ((created_at AT TIME ZONE 'UTC')::date))
    WHERE kind IN ('backup', 'basebackup', 'verify');

CREATE UNIQUE INDEX IF NOT EXISTS pending_s3_ops_measure_daily_uniq
    ON pending_s3_ops (resource_id, bucket, ((created_at AT TIME ZONE 'UTC')::date))
    WHERE action = 'measure';
