-- Org-scoped, role-carrying credentials for dashboard/backend → CP calls.
-- Only the SHA-256 digest is stored; plaintext is shown once at mint time.
CREATE TABLE IF NOT EXISTS service_tokens (
    id           TEXT PRIMARY KEY,                     -- st_<hex>
    org_id       TEXT        NOT NULL,
    name         TEXT        NOT NULL DEFAULT '',
    role         TEXT        NOT NULL DEFAULT 'Developer', -- Org Admin | Project Admin | Developer
    token_hash   BYTEA       NOT NULL UNIQUE,
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS service_tokens_org_idx ON service_tokens (org_id);
