-- Control-plane audit trail: same shape as the v1 web audit log so the
-- dashboard can eventually merge both streams.
CREATE TABLE IF NOT EXISTS cp_audit_log (
    id         BIGSERIAL PRIMARY KEY,
    org_id     TEXT        NOT NULL,
    actor      TEXT        NOT NULL,
    action     TEXT        NOT NULL,
    target     TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS cp_audit_log_org_created_idx
    ON cp_audit_log (org_id, created_at DESC);
