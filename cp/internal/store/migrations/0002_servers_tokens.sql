-- Server registry: the control plane owns server identity.
CREATE TABLE IF NOT EXISTS servers (
    id            TEXT PRIMARY KEY,                       -- srv_<hex>
    org_id        TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    type          TEXT        NOT NULL DEFAULT 'general',  -- general|database|storage|gpu
    provider      TEXT        NOT NULL DEFAULT '',
    region        TEXT        NOT NULL DEFAULT '',
    status        TEXT        NOT NULL DEFAULT 'provisioning', -- provisioning|running|unreachable
    agent_version TEXT        NOT NULL DEFAULT '',
    facts         JSONB       NOT NULL DEFAULT '{}',
    mesh_ip       TEXT,
    pubkey        TEXT,
    last_seen_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS servers_org_idx ON servers (org_id);

-- One-time bootstrap tokens: issued by the dashboard, consumed by the agent
-- exactly once. Only the SHA-256 digest is stored.
CREATE TABLE IF NOT EXISTS bootstrap_tokens (
    id          TEXT PRIMARY KEY,                          -- bt_<hex>
    org_id      TEXT        NOT NULL,
    token_hash  BYTEA       NOT NULL UNIQUE,
    server_name TEXT        NOT NULL DEFAULT '',
    server_type TEXT        NOT NULL DEFAULT 'general',
    provider    TEXT        NOT NULL DEFAULT '',
    region      TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL DEFAULT '',
    expires_at  TIMESTAMPTZ NOT NULL,
    used_at     TIMESTAMPTZ,
    server_id   TEXT REFERENCES servers (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Long-lived per-server agent credentials (digest only).
CREATE TABLE IF NOT EXISTS agent_tokens (
    id         TEXT PRIMARY KEY,                           -- at_<hex>
    server_id  TEXT        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    token_hash BYTEA       NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);
