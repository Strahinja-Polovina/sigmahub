-- SIGMA-188: DeployTargetsForServer filters on (server_id, status) and orders by
-- (resource_id, created_at DESC), but `deployments` only had indexes leading with
-- resource_id and org_id. Neither can drive the predicate, so every DSD render —
-- per push, per resource mutation, and for every server on the 60s fleet resync —
-- scanned the whole deploy history, which only ever grows (rows are superseded,
-- never deleted).
--
-- This partial index matches both the filter and the DISTINCT ON ordering, so the
-- render cost becomes proportional to the resources on that server instead of to
-- the install's total deploy history.
CREATE INDEX IF NOT EXISTS deployments_server_target_idx
    ON deployments (server_id, resource_id, created_at DESC)
    WHERE status IN ('queued', 'building', 'deploying', 'success');
