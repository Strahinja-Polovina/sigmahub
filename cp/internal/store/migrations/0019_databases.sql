-- P1-10 (Database resources): engine-provisioned databases as pinned-version
-- containers with named volumes. Credentials are generated CP-side and
-- envelope-encrypted under the org DEK (the P1-6 machinery); DSDs carry
-- references only. Databases are mesh-only in v1: the container port publishes
-- exclusively on the server's WireGuard mesh address, never a public interface
-- (the public-exposure API ships as a typed not-enabled error).

CREATE TABLE IF NOT EXISTS db_credentials (
    resource_id TEXT PRIMARY KEY REFERENCES resources (id) ON DELETE CASCADE,
    org_id      TEXT  NOT NULL,
    server_id   TEXT  NOT NULL REFERENCES servers (id),
    engine      TEXT  NOT NULL,             -- postgres|mysql|redis|mongodb
    username    TEXT  NOT NULL,
    dbname      TEXT  NOT NULL,
    port        INT   NOT NULL,             -- mesh-bound host port
    ciphertext  BYTEA NOT NULL,             -- AES-256-GCM over the credentials JSON
    nonce       BYTEA NOT NULL,
    dek_id      TEXT  NOT NULL REFERENCES org_deks (id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- One mesh port per server: the allocator hands out sequential ports under a
-- per-server advisory lock; this unique index is the backstop against a race.
CREATE UNIQUE INDEX IF NOT EXISTS db_credentials_server_port_idx
    ON db_credentials (server_id, port);
CREATE INDEX IF NOT EXISTS db_credentials_org_idx ON db_credentials (org_id);

-- P1-11 hook (data model only): creating a database writes its default backup
-- policy row. Schedule defaults key off the environment's production flag —
-- production keeps 30 dailies, everything else the GFS 7/4/6 default.
-- Execution, backup history and the no-target warning are wholly P1-11's.
CREATE TABLE IF NOT EXISTS backup_policies (
    id           TEXT PRIMARY KEY,          -- bkp_<hex>
    org_id       TEXT NOT NULL,
    resource_id  TEXT NOT NULL UNIQUE REFERENCES resources (id) ON DELETE CASCADE,
    schedule     TEXT NOT NULL DEFAULT 'daily',
    keep_daily   INT  NOT NULL DEFAULT 7,
    keep_weekly  INT  NOT NULL DEFAULT 4,
    keep_monthly INT  NOT NULL DEFAULT 6,
    target_id    TEXT,                      -- P1-11 backup target; NULL = not configured
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS backup_policies_org_idx ON backup_policies (org_id);
