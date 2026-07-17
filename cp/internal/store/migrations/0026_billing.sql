-- P2-4 usage-based billing (Paddle). Two pieces:
--
-- server_hours: the time-integrated connected-server meter (mirrors
-- usage_hours' idempotent hourly sweep), so a month's billable server-months
-- come from actual connected time, not a point-in-time snapshot. "Connected"
-- = status 'running' (the state the CP can actually manage), matching
-- ConnectedServerCount.
CREATE TABLE server_hours (
    org_id    TEXT NOT NULL,
    server_id TEXT NOT NULL,
    hour      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (server_id, hour)
);
CREATE INDEX server_hours_org_idx ON server_hours (org_id, hour);

-- org_billing: per-org subscription state, keyed by the bare org string
-- (mirrors org_tenants — the CP has no orgs table). Rows are created lazily
-- on first checkout/webhook. status: none|active|past_due|canceled.
CREATE TABLE org_billing (
    org_id                 TEXT PRIMARY KEY,
    paddle_customer_id     TEXT NOT NULL DEFAULT '',
    paddle_subscription_id TEXT NOT NULL DEFAULT '',
    status                 TEXT NOT NULL DEFAULT 'none',
    quantity               INT NOT NULL DEFAULT 0,
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
