-- P1-7 Git integration. A GitHub App connection links a repo to a project; a
-- branch→environment map with a per-environment deploy policy routes push
-- events; webhook deliveries are deduped for idempotency; push events on
-- auto-deploy branches enqueue exactly one deploy request (executed by P1-9);
-- PR events are persisted as routing hooks only (deploy semantics are P1-12).

CREATE TABLE IF NOT EXISTS git_connections (
    id              TEXT PRIMARY KEY,
    org_id          TEXT NOT NULL,
    project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL DEFAULT 'github',
    installation_id TEXT NOT NULL,
    repo_full_name  TEXT NOT NULL,            -- owner/name
    token_wrapped   BYTEA,                    -- KMS-wrapped provider token (P1-6 envelope)
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One connection per repo per provider (a repo drives at most one project).
CREATE UNIQUE INDEX IF NOT EXISTS git_connections_repo_idx
    ON git_connections (provider, lower(repo_full_name));

CREATE TABLE IF NOT EXISTS git_branch_map (
    id             TEXT PRIMARY KEY,
    connection_id  TEXT NOT NULL REFERENCES git_connections(id) ON DELETE CASCADE,
    branch         TEXT NOT NULL,
    environment_id TEXT NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    policy         TEXT NOT NULL DEFAULT 'manual', -- 'auto' | 'manual'
    -- Last push seen on this branch. A 'manual' branch records the commit here
    -- WITHOUT enqueuing a deploy; promotion later enqueues that remembered sha.
    last_ref       TEXT,
    last_sha       TEXT,
    last_pushed_at TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS git_branch_map_idx
    ON git_branch_map (connection_id, branch);

-- Idempotency: a redelivered webhook (same provider delivery id) is a no-op.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    delivery_id  TEXT PRIMARY KEY,
    provider     TEXT NOT NULL,
    event_type   TEXT NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Enqueued deploys (P1-9 drains these). PR routing hooks are recorded too, with
-- kind='pr_hook' and no deploy semantics.
CREATE TABLE IF NOT EXISTS deploy_requests (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    connection_id  TEXT NOT NULL REFERENCES git_connections(id) ON DELETE CASCADE,
    environment_id TEXT REFERENCES environments(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL DEFAULT 'deploy', -- 'deploy' | 'pr_hook'
    ref            TEXT NOT NULL,
    sha            TEXT NOT NULL,
    branch         TEXT,
    status         TEXT NOT NULL DEFAULT 'queued',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS deploy_requests_org_idx ON deploy_requests (org_id, created_at);
