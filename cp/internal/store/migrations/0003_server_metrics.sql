-- Rolling window of per-server resource samples, appended on each heartbeat.
-- Pruned by the CP sweeper (P0-4); not meant to be long-term TSDB storage.
CREATE TABLE IF NOT EXISTS server_metrics (
    id          BIGSERIAL PRIMARY KEY,
    server_id   TEXT             NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    cpu_pct     DOUBLE PRECISION NOT NULL DEFAULT 0,
    mem_pct     DOUBLE PRECISION NOT NULL DEFAULT 0,
    disk_pct    DOUBLE PRECISION NOT NULL DEFAULT 0,
    load1       DOUBLE PRECISION NOT NULL DEFAULT 0,
    recorded_at TIMESTAMPTZ      NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS server_metrics_server_time_idx
    ON server_metrics (server_id, recorded_at DESC);
