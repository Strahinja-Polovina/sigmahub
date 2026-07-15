-- P1-6 (Secrets v1): per-tenant envelope encryption on the P0-9 KeyCustody
-- boundary. Tenant secret ciphertexts live in the CP database under per-org
-- data-encryption keys (DEKs) which are themselves wrapped via KeyCustody; a
-- recorded deviation from the "secrets in a cloud secret manager" wording,
-- flagged for the security design review. Secret Manager is not introduced in
-- Phase 1.

-- Per-org DEKs. The DEK plaintext (32-byte AES-256-GCM key) is NEVER stored —
-- only its custody-wrapped form. wrap_version advances on KEK rotation (the DEK
-- is re-wrapped, the secret data is untouched). retired_at is set once a DEK
-- holds zero secrets after a DEK rotation's lazy re-encrypt drains it.
CREATE TABLE IF NOT EXISTS org_deks (
    id           TEXT PRIMARY KEY,          -- dek_<hex>
    org_id       TEXT        NOT NULL,
    wrapped_dek  BYTEA       NOT NULL,
    wrap_version INT         NOT NULL DEFAULT 1,
    -- active = the target of new writes. During a DEK rotation the old DEK is
    -- deactivated but stays until a lazy re-encrypt drains its rows, then it is
    -- retired — so two DEKs can be live (one active, one draining) at once.
    active       BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    retired_at   TIMESTAMPTZ
);
-- Exactly one active DEK per org — the target of new writes.
CREATE UNIQUE INDEX IF NOT EXISTS org_deks_active_idx
    ON org_deks (org_id) WHERE active;

-- Tenant secrets. The value is AES-256-GCM encrypted under the org DEK with a
-- per-secret random nonce and AAD bound to (org, project, secret id), so a
-- ciphertext moved to another row fails to decrypt. Scoped to a project
-- (environment_id NULL) or an environment; resources inherit their env secrets.
CREATE TABLE IF NOT EXISTS secrets (
    id             TEXT PRIMARY KEY,        -- sec_<hex>
    org_id         TEXT        NOT NULL,
    project_id     TEXT        NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    environment_id TEXT        REFERENCES environments (id) ON DELETE CASCADE,
    name           TEXT        NOT NULL,
    ciphertext     BYTEA       NOT NULL,
    nonce          BYTEA       NOT NULL,
    dek_id         TEXT        NOT NULL REFERENCES org_deks (id),
    -- Injection mode: false = tmpfs file (default), true = env var (explicit
    -- opt-in with the documented Docker plaintext-on-host caveat).
    env_var        BOOLEAN     NOT NULL DEFAULT FALSE,
    created_by     TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Names are unique within a scope; env-scoped and project-scoped are distinct
-- namespaces (env secrets override project defaults at resolve time).
CREATE UNIQUE INDEX IF NOT EXISTS secrets_env_name_idx
    ON secrets (environment_id, lower(name)) WHERE environment_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS secrets_project_name_idx
    ON secrets (project_id, lower(name)) WHERE environment_id IS NULL;
