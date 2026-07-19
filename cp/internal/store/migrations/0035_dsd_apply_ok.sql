-- SIGMA-104: track whether the agent's status report for the current DSD version
-- converged (every op "applied") or had a failed/skipped op. Without this the CP
-- treats applied_version == version as "converged" even when ops failed, so the
-- 60s resync — which re-renders identical ops and finds an unchanged doc_hash —
-- never re-issues, and a transiently-failed op (registry blip, volume-in-use) is
-- orphaned until an unrelated change bumps the version. When apply_ok is false and
-- the agent has caught up (applied_version == version), the reconciler re-issues
-- the same ops as a new version so the agent retries.
ALTER TABLE server_dsd ADD COLUMN IF NOT EXISTS apply_ok BOOLEAN NOT NULL DEFAULT true;
