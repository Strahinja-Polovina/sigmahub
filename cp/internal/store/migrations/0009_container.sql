-- P1-3: container runtime support.
--
-- 1) resources.ephemeral marks preview-style resources (P1-12) whose teardown
--    skips interactive destructive-op approval — the per-app preview opt-in is
--    the pre-authorised approval, executed as a token-gated system-actor op.
ALTER TABLE resources ADD COLUMN IF NOT EXISTS ephemeral BOOLEAN NOT NULL DEFAULT FALSE;

-- 2) Two-phase destructive-op confirm tokens. Phase 1 mints a short-lived,
--    single-use token bound to a specific (server, op_kind, target); phase 2
--    atomically claims it (used_at IS NULL AND expires_at > now()) and records
--    the destructive op below. Only the HMAC digest is stored, never plaintext.
CREATE TABLE IF NOT EXISTS destructive_confirm_tokens (
    id         TEXT PRIMARY KEY,               -- sct_<hex>
    org_id     TEXT        NOT NULL,
    server_id  TEXT        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    op_kind    TEXT        NOT NULL,            -- e.g. volume.remove
    target     TEXT        NOT NULL,            -- e.g. the volume name
    token_hash BYTEA       NOT NULL UNIQUE,
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ
);

-- 3) Confirmed destructive ops awaiting agent application. The reconciler emits
--    a matching op into the server's DSD until the agent reports it applied
--    (applied_at set), after which it drops out of future documents.
CREATE TABLE IF NOT EXISTS pending_destructive_ops (
    id         TEXT PRIMARY KEY,               -- pdo_<hex>
    org_id     TEXT        NOT NULL,
    server_id  TEXT        NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    op_kind    TEXT        NOT NULL,
    target     TEXT        NOT NULL,
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at TIMESTAMPTZ
);

-- Fast lookup of a server's still-pending destructive ops during render.
CREATE INDEX IF NOT EXISTS pending_destructive_ops_server_idx
    ON pending_destructive_ops (server_id) WHERE applied_at IS NULL;
