-- P2-6 alerting: per-org notification channels, event→channel rules, and a
-- delivery outbox. Producers enqueue outbox rows inside the same transaction
-- as the state change they alert on (an alert is never lost to a crash
-- between commit and notify), and a dispatcher loop drains the outbox with
-- retry/backoff — sending never blocks or fails the originating operation.

CREATE TABLE alert_channels (
    id TEXT PRIMARY KEY,
    org_id TEXT NOT NULL,
    kind TEXT NOT NULL, -- email | slack | telegram | webhook
    name TEXT NOT NULL,
    -- Non-secret destination config (recipients, chat id, webhook URL...).
    config JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- The channel secret (SMTP password, Slack webhook URL, bot token, HMAC
    -- signing key) under the org-DEK envelope; never returned by the API.
    secret_ciphertext BYTEA,
    secret_nonce BYTEA,
    dek_id TEXT REFERENCES org_deks(id),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    last_ok_at TIMESTAMPTZ,
    last_error TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX alert_channels_org_idx ON alert_channels (org_id);

-- Which events a channel receives. Rows are the enable switch: no row = off.
CREATE TABLE alert_rules (
    org_id TEXT NOT NULL,
    event TEXT NOT NULL,
    channel_id TEXT NOT NULL REFERENCES alert_channels(id) ON DELETE CASCADE,
    PRIMARY KEY (org_id, event, channel_id)
);

CREATE TABLE alert_outbox (
    id BIGSERIAL PRIMARY KEY,
    org_id TEXT NOT NULL,
    channel_id TEXT NOT NULL REFERENCES alert_channels(id) ON DELETE CASCADE,
    event TEXT NOT NULL,
    -- Dedup scope: an identical dedup_key on the same channel inside the
    -- producer's window is not re-enqueued (flapping servers alert once per
    -- cooldown; a deployment failure alerts exactly once per deployment).
    dedup_key TEXT NOT NULL,
    title TEXT NOT NULL,
    body TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending', -- pending | sent | failed
    attempts INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at TIMESTAMPTZ
);
CREATE INDEX alert_outbox_due_idx ON alert_outbox (status, next_attempt_at);
CREATE INDEX alert_outbox_dedup_idx ON alert_outbox (channel_id, dedup_key, created_at);
