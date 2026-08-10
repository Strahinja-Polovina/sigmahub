-- SIGMA-332: normalise Compose per-service placement so the DSD render can be
-- indexed again.
--
-- Migration 0039 added deployments_server_target_idx (server_id, resource_id,
-- created_at DESC) WHERE status IN (...) and said, in its own comment, that it
-- makes "the render cost proportional to the resources on that server instead of
-- to the install's total deploy history" (SIGMA-188).
--
-- A later commit added a third arm to DeployTargetsForServer's disjunction — the
-- Compose-placement check
--
--     jsonb_typeof(r.spec->'compose'->'services') = 'array'
--     AND EXISTS (SELECT 1 FROM jsonb_array_elements(...) svc
--                  WHERE svc->>'serverId' = $1)
--
-- which reads r.spec from the JOINED resources table. A disjunction is only a
-- restriction on `deployments` if every arm is, so that one arm took the whole
-- WHERE out of the index's reach: measured here, the plan became a Seq Scan over
-- every deployment row plus a jsonb_array_elements function scan per row. The
-- index still existed, so nothing looked broken; it was simply never chosen
-- again. That render runs once per server on the 60s resync, once per push, once
-- per resource mutation and once per deploy status report.
--
-- Placement now lives in its own table, so the arm becomes a subquery over
-- placement rows and the deployments side of the query is once again expressible
-- as predicates on deployments columns alone.
--
-- resources.spec stays the source of truth the reconciler renders from; this
-- table is a derived projection of it, rewritten in the same transaction as
-- every spec write (see syncServicePlacementsTx). It is keyed on all three
-- columns rather than (resource_id, service) so it reproduces the old
-- predicate's semantics exactly, including a service that declares a placement
-- but no name.
CREATE TABLE IF NOT EXISTS resource_service_placements (
    resource_id TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    service     TEXT NOT NULL,
    server_id   TEXT NOT NULL,
    PRIMARY KEY (resource_id, service, server_id)
);

-- "Which resources put a service on this server" is the only question asked of
-- this table.
CREATE INDEX IF NOT EXISTS resource_service_placements_server_idx
    ON resource_service_placements (server_id, resource_id);

-- Backfill from the specs that are the source of truth. Identical to the
-- INSERT in syncServicePlacementsTx, minus the single-resource filter: a
-- placement is a compose service carrying a non-empty serverId.
INSERT INTO resource_service_placements (resource_id, service, server_id)
SELECT r.id, COALESCE(svc->>'name', ''), svc->>'serverId'
  FROM resources r
  CROSS JOIN LATERAL jsonb_array_elements(
       CASE WHEN jsonb_typeof(r.spec->'compose'->'services') = 'array'
            THEN r.spec->'compose'->'services' ELSE '[]'::jsonb END) svc
 WHERE COALESCE(svc->>'serverId', '') <> ''
ON CONFLICT DO NOTHING;
