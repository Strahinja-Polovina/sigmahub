-- SIGMA-116: bound the SIGMA-104 apply_ok re-drive. Without a cap, a
-- permanently-failing op (e.g. a mistyped image tag) makes StoreDSD re-issue an
-- identical DSD as a new version — plus a "DSD issued" audit row and an agent
-- re-apply — on every 60s resync forever. redrive_count tracks how many times
-- the CURRENT (unchanged) doc has been re-issued for a non-converged apply; it
-- resets to 0 whenever the doc_hash changes (a real config change), and once it
-- exceeds the cap the re-drive stops, leaving the failure visible on
-- resources.status instead of churning.
ALTER TABLE server_dsd ADD COLUMN IF NOT EXISTS redrive_count INT NOT NULL DEFAULT 0;
