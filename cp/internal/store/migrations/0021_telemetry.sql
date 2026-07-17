-- P1-13 (basic monitoring + M1 instrumentation). The customer-telemetry
-- pipeline is VictoriaMetrics (metrics) + Loki (logs); agents remote-write
-- over the outbound authenticated agent channel and the CP forwards with
-- tenant isolation. Recorded decisions on SIGMA-52: alerting follows the
-- roadmap (Phase 2; pipeline stays vmalert-compatible) and uptime checks are
-- deferred; heartbeat-embedded gauges REMAIN (the staleness sweeper and
-- connected-server counts depend on them).

-- VictoriaMetrics cluster accountIDs are numeric; orgs are strings. One stable
-- tenant number per org, allocated on first telemetry write.
CREATE TABLE IF NOT EXISTS org_tenants (
    org_id  TEXT PRIMARY KEY,
    tenant  SERIAL
);

-- Hourly idempotent (resource, hour) usage aggregates — the A-4 metering hook
-- Phase 2's billing pipeline reads. Insert-only, conflict-free by design.
CREATE TABLE IF NOT EXISTS usage_hours (
    org_id      TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    hour        TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (resource_id, hour)
);
CREATE INDEX IF NOT EXISTS usage_hours_org_idx ON usage_hours (org_id, hour);
