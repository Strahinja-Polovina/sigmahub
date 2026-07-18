-- P2-5b (SIGMA-67): PITR restore-to-timestamp. A restore run of kind
-- 'restore-pitr' recovers a fresh resource to a chosen point in time. The
-- target timestamp rides the run so the executing agent replays WAL up to it
-- (recovery_target_time). NULL for every other run kind.
ALTER TABLE backup_runs ADD COLUMN recovery_target_time TIMESTAMPTZ;
