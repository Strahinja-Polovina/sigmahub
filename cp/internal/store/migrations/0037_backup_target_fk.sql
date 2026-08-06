-- SIGMA-135: bind backup_policies.target_id to backup_targets. Without an FK, a
-- write-skew race (DeleteBackupTarget's EXISTS check vs UpdateBackupPolicy's
-- unlocked target validation) could leave a policy pointing at a deleted target,
-- after which every backup/PITR run fails to fetch credentials until the policy
-- is manually re-pointed. The FK takes a FOR KEY SHARE lock on the referenced
-- target during a policy update, which serializes it against the target delete
-- so one of the two fails cleanly instead of committing a dangling pointer.

-- Clean up any pre-existing dangling pointers BEFORE adding the constraint, or
-- ADD CONSTRAINT would abort on a database that already hit the race — and a
-- failed migration rolls back and retries forever, bricking boot (SIGMA-86).
UPDATE backup_policies
   SET target_id = NULL, pitr_enabled = FALSE
 WHERE target_id IS NOT NULL
   AND target_id NOT IN (SELECT id FROM backup_targets);

ALTER TABLE backup_policies
  ADD CONSTRAINT backup_policies_target_fk
  FOREIGN KEY (target_id) REFERENCES backup_targets (id);
