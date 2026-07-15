-- Wrapped secrets at rest. The token pepper (HMAC key for every token hash)
-- lives here encrypted; it is unwrapped via the KMS custody at boot, and each
-- unwrap is audited. Never store a plaintext secret in this table.
CREATE TABLE IF NOT EXISTS cp_secrets (
    name       TEXT PRIMARY KEY,   -- e.g. 'token_pepper'
    wrapped    BYTEA       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
