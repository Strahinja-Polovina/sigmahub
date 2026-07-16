-- P1-10 database resources: generated credentials (P1-6 envelope) + the
-- backup-policy data-model hook (execution/history/UI are P1-11's).

-- One credential set per database resource. Only the password is secret: it is
-- AES-GCM sealed with the org's active DEK (same envelope as secrets), bound to
-- its row identity via AAD. Username/database name are plain metadata.
CREATE TABLE IF NOT EXISTS db_credentials (
    id          TEXT        PRIMARY KEY,
    org_id      TEXT        NOT NULL,
    resource_id TEXT        NOT NULL UNIQUE REFERENCES resources(id) ON DELETE CASCADE,
    engine      TEXT        NOT NULL,
    username    TEXT        NOT NULL,
    db_name     TEXT        NOT NULL,
    ciphertext  BYTEA       NOT NULL,
    nonce       BYTEA       NOT NULL,
    dek_id      TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The default backup policy written at DB-resource creation. The schedule
-- default keys off the environment's production flag (daily for prod, weekly
-- otherwise). P1-11 owns execution, history, and the no-target warning.
CREATE TABLE IF NOT EXISTS backup_policies (
    id             TEXT        PRIMARY KEY,
    org_id         TEXT        NOT NULL,
    resource_id    TEXT        NOT NULL UNIQUE REFERENCES resources(id) ON DELETE CASCADE,
    schedule       TEXT        NOT NULL, -- 5-field cron
    retention_days INT         NOT NULL DEFAULT 7,
    enabled        BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
