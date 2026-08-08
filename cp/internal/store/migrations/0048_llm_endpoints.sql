-- GPU model hosting: one mesh-bound inference endpoint per `llm` resource.
--
-- Symmetric with db_credentials and s3_credentials — the port allocator already
-- scans every port-owning table on a server, so the endpoint joins that set
-- rather than inventing a parallel numbering scheme.
CREATE TABLE IF NOT EXISTS llm_endpoints (
    resource_id TEXT PRIMARY KEY REFERENCES resources(id) ON DELETE CASCADE,
    org_id      TEXT NOT NULL,
    server_id   TEXT NOT NULL REFERENCES servers(id) ON DELETE CASCADE,
    engine      TEXT NOT NULL DEFAULT 'vllm',
    model       TEXT NOT NULL DEFAULT '',
    port        INT  NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Same invariant the other port-owning tables hold: one host port per server.
CREATE UNIQUE INDEX IF NOT EXISTS llm_endpoints_server_port_uniq
    ON llm_endpoints (server_id, port);
CREATE INDEX IF NOT EXISTS llm_endpoints_server_idx ON llm_endpoints (server_id);
