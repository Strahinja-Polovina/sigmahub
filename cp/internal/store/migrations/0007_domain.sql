-- P1-1: the Org -> Project -> Environment -> Server -> Resource domain model
-- moves into the control plane as the source of truth for the reconciler.

-- Server attributes the domain model needs: where the box came from, and
-- whether it fronts HTTP traffic (Traefik placement + firewall rules key off
-- proxy_role in P1-5/P1-8).
ALTER TABLE servers ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'byo';
ALTER TABLE servers ADD COLUMN IF NOT EXISTS proxy_role BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS projects (
    id          TEXT PRIMARY KEY,                    -- prj_<hex>
    org_id      TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS projects_org_idx ON projects (org_id);
CREATE UNIQUE INDEX IF NOT EXISTS projects_org_name_idx ON projects (org_id, lower(name));

-- Environments are free-text names (production/staging/dev/...); the
-- `production` flag is the marker backup defaults and preview exclusions key
-- off in later Phase 1 tickets.
CREATE TABLE IF NOT EXISTS environments (
    id         TEXT PRIMARY KEY,                     -- env_<hex>
    org_id     TEXT        NOT NULL,
    project_id TEXT        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT        NOT NULL,
    production BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS environments_project_idx ON environments (project_id);
CREATE UNIQUE INDEX IF NOT EXISTS environments_project_name_idx ON environments (project_id, lower(name));

-- Servers attach to one or more environments.
CREATE TABLE IF NOT EXISTS env_servers (
    environment_id TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    server_id      TEXT NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    org_id         TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (environment_id, server_id)
);
CREATE INDEX IF NOT EXISTS env_servers_server_idx ON env_servers (server_id);

-- Resources: spec is the desired state (reconciler input, P1-2); status is
-- agent-reported. Server binding is mutable state, not identity (DR re-homing
-- hook), but v1 requires exactly one server.
CREATE TABLE IF NOT EXISTS resources (
    id             TEXT PRIMARY KEY,                 -- res_<hex>
    org_id         TEXT        NOT NULL,
    project_id     TEXT        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment_id TEXT        NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    server_id      TEXT        NOT NULL REFERENCES servers (id),
    name           TEXT        NOT NULL,
    kind           TEXT        NOT NULL,             -- app|postgres|mysql|redis|mongodb|s3|llm
    spec           JSONB       NOT NULL DEFAULT '{}',
    status         JSONB       NOT NULL DEFAULT '{}',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS resources_org_idx ON resources (org_id);
CREATE INDEX IF NOT EXISTS resources_env_idx ON resources (environment_id);
CREATE INDEX IF NOT EXISTS resources_server_idx ON resources (server_id);
CREATE UNIQUE INDEX IF NOT EXISTS resources_env_name_idx ON resources (environment_id, lower(name));

-- Idempotency keys for mutating POSTs: replaying the same (org, key) returns
-- the stored response instead of re-executing. request_hash detects key reuse
-- with a different body (409).
CREATE TABLE IF NOT EXISTS idempotency_keys (
    org_id       TEXT        NOT NULL,
    key          TEXT        NOT NULL,
    request_hash BYTEA       NOT NULL,
    status_code  INT         NOT NULL,
    response     JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, key)
);
