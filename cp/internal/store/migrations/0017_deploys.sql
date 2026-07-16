-- P1-9 real Git deploy pipeline. A deploy is CP-orchestrated and executed by
-- sigmad: clone at SHA → BuildKit build on the target → zero-downtime rollout.
-- Every deploy writes an IMMUTABLE history row (git SHA, image digest, config
-- hash, outcome, duration) that feeds the M1 success-rate metric and backs
-- one-click rollback to any of the last 10 releases without a rebuild.

CREATE TABLE IF NOT EXISTS deployments (
    id               TEXT PRIMARY KEY,                          -- dep_<hex>
    org_id           TEXT NOT NULL,
    resource_id      TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    environment_id   TEXT REFERENCES environments(id) ON DELETE SET NULL,
    server_id        TEXT REFERENCES servers(id) ON DELETE SET NULL,
    connection_id    TEXT REFERENCES git_connections(id) ON DELETE SET NULL, -- git-triggered
    trigger          TEXT NOT NULL DEFAULT 'git',               -- 'git' | 'manual' | 'rollback'
    git_ref          TEXT,
    git_sha          TEXT,
    image_digest     TEXT,                                      -- built/deployed image
    config_hash      TEXT,                                      -- resource config at deploy time
    -- queued -> building -> deploying -> success | failed; superseded when a newer
    -- deploy wins the race; rolled_back is a terminal marker on a prior success.
    status           TEXT NOT NULL DEFAULT 'queued',
    detail           TEXT NOT NULL DEFAULT '',                  -- failure summary / rollback source
    -- rollback_of points at the deployment whose image this one re-serves.
    rollback_of      TEXT REFERENCES deployments(id) ON DELETE SET NULL,
    build_seconds    INT,
    duration_seconds INT,
    created_by       TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at       TIMESTAMPTZ,
    finished_at      TIMESTAMPTZ
);
-- Rollback + retention both walk a resource's releases newest-first.
CREATE INDEX IF NOT EXISTS deployments_resource_idx ON deployments (resource_id, created_at DESC);
CREATE INDEX IF NOT EXISTS deployments_org_idx ON deployments (org_id, created_at DESC);

-- Build dedup: a build is keyed by (resource, config-hash + git-SHA). A retry of
-- the same inputs reuses the already-built image digest instead of rebuilding —
-- the architecture idempotency invariant (also what makes a &lt;30s rollback
-- rebuild-free: the image is already present).
CREATE TABLE IF NOT EXISTS builds (
    id           TEXT PRIMARY KEY,                              -- bld_<hex>
    org_id       TEXT NOT NULL,
    resource_id  TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    server_id    TEXT REFERENCES servers(id) ON DELETE SET NULL,
    dedup_key    TEXT NOT NULL,                                 -- config_hash + ':' + git_sha
    git_sha      TEXT,
    image_ref    TEXT,                                          -- local tag the agent built
    image_digest TEXT,
    status       TEXT NOT NULL DEFAULT 'pending',               -- pending|building|built|failed
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One row per (resource, dedup key): the retry that finds it 'built' skips the build.
CREATE UNIQUE INDEX IF NOT EXISTS builds_dedup_idx ON builds (resource_id, dedup_key);

-- Append-only build / orchestration log lines, streamed agent → CP → web (SSE).
-- Steady-state container logs are NOT here — those are P1-13's Loki path.
CREATE TABLE IF NOT EXISTS deploy_logs (
    id            BIGSERIAL PRIMARY KEY,
    deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    stream        TEXT NOT NULL DEFAULT 'build',                -- build | orchestration | startup
    line          TEXT NOT NULL,
    at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deploy_logs_deployment_idx ON deploy_logs (deployment_id, id);
