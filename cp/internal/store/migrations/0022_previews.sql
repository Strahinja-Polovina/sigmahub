-- P1-12 (preview environments per pull request). A previews-enabled git
-- connection turns PR opened/synchronize events into an ephemeral environment
-- ("pr-<n>") holding one ephemeral app resource deployed at the PR head SHA;
-- PR close tears both down through the existing ephemeral carve-out (volume
-- removal pre-authorised by the preview opt-in, still audited). Wildcard
-- DNS-01 TLS for *.preview domains stays a preserved hook (P1-8's
-- dns_provider_credentials + the reconciler's challenge plumbing).

ALTER TABLE git_connections ADD COLUMN IF NOT EXISTS previews_enabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE git_connections ADD COLUMN IF NOT EXISTS preview_server_id TEXT REFERENCES servers (id);

CREATE TABLE IF NOT EXISTS preview_environments (
    id             TEXT PRIMARY KEY,        -- prv_<hex>
    org_id         TEXT NOT NULL,
    connection_id  TEXT NOT NULL REFERENCES git_connections (id) ON DELETE CASCADE,
    pr_number      INT  NOT NULL,
    -- Deliberately NOT a foreign key: the row is the PR's historical record
    -- and must survive the teardown that deletes the environment itself.
    environment_id TEXT NOT NULL,
    resource_id    TEXT REFERENCES resources (id) ON DELETE SET NULL,
    branch         TEXT NOT NULL DEFAULT '',
    sha            TEXT NOT NULL DEFAULT '',
    status         TEXT NOT NULL DEFAULT 'open', -- open | closed
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at      TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS preview_envs_conn_pr_idx
    ON preview_environments (connection_id, pr_number) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS preview_envs_org_idx ON preview_environments (org_id, created_at DESC);
