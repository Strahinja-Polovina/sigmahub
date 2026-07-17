-- P1-11 (restic backups + automated restore-verify). One restic repository per
-- database resource on any S3-compatible target. restic encrypts client-side
-- (AES-256) before a byte leaves the source server; the per-resource repo key
-- is wrapped under the org DEK and released to the executing agent per run
-- through an audited fetch — the CP schedules and verifies but never persists
-- a plaintext repo key.

-- S3-compatible backup targets (AWS or third-party; a SigmaHub-managed S3 on
-- customer servers is the Phase-2 hook behind the same abstraction). The
-- secret key is envelope-encrypted under the org DEK; the access key id is an
-- identifier, not a secret.
CREATE TABLE IF NOT EXISTS backup_targets (
    id                TEXT PRIMARY KEY,      -- bkt_<hex>
    org_id            TEXT  NOT NULL,
    name              TEXT  NOT NULL,
    endpoint          TEXT  NOT NULL DEFAULT '',   -- empty = AWS S3
    bucket            TEXT  NOT NULL,
    region            TEXT  NOT NULL DEFAULT '',
    force_path_style  BOOLEAN NOT NULL DEFAULT TRUE,
    access_key        TEXT  NOT NULL,
    secret_ciphertext BYTEA NOT NULL,
    secret_nonce      BYTEA NOT NULL,
    dek_id            TEXT  NOT NULL REFERENCES org_deks (id),
    created_by        TEXT  NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS backup_targets_org_name_idx
    ON backup_targets (org_id, lower(name));

-- The per-resource restic repository key, generated on first use and wrapped
-- under the org DEK (P1-6 path). Lives on the policy row: one repo per resource.
ALTER TABLE backup_policies ADD COLUMN IF NOT EXISTS repo_key_ciphertext BYTEA;
ALTER TABLE backup_policies ADD COLUMN IF NOT EXISTS repo_key_nonce BYTEA;
ALTER TABLE backup_policies ADD COLUMN IF NOT EXISTS repo_dek_id TEXT REFERENCES org_deks (id);

-- Backup/verify/restore runs. The CP-side cron scheduler inserts pending rows;
-- the reconciler renders pending rows as typed DSD ops (backup.run /
-- backup.verify / backup.restore); the agent executes and reports the result.
-- An unrestored backup counts as no backup: verify outcomes are queryable
-- per-day for the M1 green-streak gate (streak display is P1-13's).
CREATE TABLE IF NOT EXISTS backup_runs (
    id                  TEXT PRIMARY KEY,    -- run_<hex>
    org_id              TEXT NOT NULL,
    resource_id         TEXT NOT NULL REFERENCES resources (id) ON DELETE CASCADE,
    policy_id           TEXT NOT NULL REFERENCES backup_policies (id) ON DELETE CASCADE,
    server_id           TEXT NOT NULL,       -- the executing server (render + credential scope)
    kind                TEXT NOT NULL,       -- backup|verify|restore
    status              TEXT NOT NULL DEFAULT 'pending', -- pending|running|success|failed
    snapshot_id         TEXT NOT NULL DEFAULT '',
    dump_sha256         TEXT NOT NULL DEFAULT '',
    detail              TEXT NOT NULL DEFAULT '',
    restore_resource_id TEXT,                -- restore runs: the NEW resource loaded into
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS backup_runs_resource_idx ON backup_runs (resource_id, created_at DESC);
CREATE INDEX IF NOT EXISTS backup_runs_verify_day_idx ON backup_runs (org_id, kind, finished_at);
CREATE INDEX IF NOT EXISTS backup_runs_open_idx ON backup_runs (server_id) WHERE status IN ('pending', 'running');
