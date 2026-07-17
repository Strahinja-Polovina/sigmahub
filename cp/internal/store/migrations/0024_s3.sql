-- P2-1 S3 storage resources (MinIO engine). Mirrors db_credentials: the root
-- secret key lives envelope-encrypted under the org DEK; the port is the
-- mesh-bound host port the S3 API is published on. Ports share one per-server
-- space with db_credentials (the allocator takes the same advisory lock and
-- scans both tables).
CREATE TABLE s3_credentials (
    resource_id TEXT PRIMARY KEY REFERENCES resources(id) ON DELETE CASCADE,
    org_id TEXT NOT NULL,
    server_id TEXT NOT NULL REFERENCES servers(id),
    engine TEXT NOT NULL,
    access_key TEXT NOT NULL,
    port INT NOT NULL,
    ciphertext BYTEA NOT NULL,
    nonce BYTEA NOT NULL,
    dek_id TEXT NOT NULL REFERENCES org_deks(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX s3_credentials_server_port_idx ON s3_credentials (server_id, port);
CREATE INDEX s3_credentials_org_idx ON s3_credentials (org_id);
