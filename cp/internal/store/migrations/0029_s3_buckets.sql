-- SIGMA-65 S3 bucket/key CRUD + quotas + storage metering. In-dashboard bucket
-- and per-bucket access-key management for a provisioned MinIO/SeaweedFS engine,
-- a per-bucket quota, and a daily storage measurement the billing meter reads.
--
-- The CP is the sole authority: dashboard mutations record a bucket row and a
-- pending op; the reconciler renders open pending_s3_ops as typed s3.configure
-- ops; the agent executes and reports the terminal result. Per-bucket access-key
-- secrets ride envelope-encrypted under the org DEK (never plaintext at rest),
-- released to the executing agent only through the audited s3-op-credential path.
CREATE TABLE s3_buckets (
    id             TEXT PRIMARY KEY,
    resource_id    TEXT NOT NULL REFERENCES resources(id) ON DELETE CASCADE,
    org_id         TEXT NOT NULL,
    name           TEXT NOT NULL,
    quota_bytes    BIGINT NOT NULL DEFAULT 0,
    access_key     TEXT NOT NULL DEFAULT '',
    key_ciphertext BYTEA,
    key_nonce      BYTEA,
    key_dek_id     TEXT,
    status         TEXT NOT NULL DEFAULT 'provisioning', -- provisioning|active|deleting
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (resource_id, name)
);

-- Pending on-demand ops the reconciler renders into a server's DSD. One row per
-- bucket/key/quota/measure action; the reconciler skips servers with no mesh_ip
-- yet. The new per-bucket secret (create-key) rides envelope-encrypted here and
-- is decrypted only for the audited agent credential fetch.
CREATE TABLE pending_s3_ops (
    id                    TEXT PRIMARY KEY,
    org_id                TEXT NOT NULL,
    server_id             TEXT NOT NULL,
    resource_id           TEXT NOT NULL,
    engine                TEXT NOT NULL,
    action                TEXT NOT NULL, -- create-bucket|delete-bucket|set-quota|create-key|measure
    bucket                TEXT NOT NULL DEFAULT '',
    access_key            TEXT NOT NULL DEFAULT '',
    quota_bytes           BIGINT NOT NULL DEFAULT 0,
    new_secret_ciphertext BYTEA,
    new_secret_nonce      BYTEA,
    new_secret_dek_id     TEXT,
    applied_at            TIMESTAMPTZ,
    failed_at             TIMESTAMPTZ,
    detail                TEXT NOT NULL DEFAULT '',
    created_by            TEXT NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The reconciler's hot path: open ops per server. Partial index keeps it small.
CREATE INDEX pending_s3_ops_open_idx ON pending_s3_ops (server_id)
    WHERE applied_at IS NULL AND failed_at IS NULL;

-- Daily per-bucket storage measurement (measure ops write here). The billing
-- meter integrates over (org_id, day); PRIMARY KEY makes each day's measurement
-- an idempotent upsert.
CREATE TABLE s3_storage_bytes (
    resource_id TEXT NOT NULL,
    org_id      TEXT NOT NULL,
    bucket      TEXT NOT NULL,
    day         TIMESTAMPTZ NOT NULL,
    bytes       BIGINT NOT NULL,
    PRIMARY KEY (resource_id, bucket, day)
);

CREATE INDEX s3_storage_bytes_org_day_idx ON s3_storage_bytes (org_id, day);
