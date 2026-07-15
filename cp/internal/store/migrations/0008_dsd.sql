-- P1-2: the per-server Desired-State Document the reconciler renders and the
-- agent applies. The reconciler is the ONLY writer; version is monotonic per
-- server so the agent can reject stale/replayed/downgraded state.
CREATE TABLE IF NOT EXISTS server_dsd (
    server_id      TEXT PRIMARY KEY REFERENCES servers (id) ON DELETE CASCADE,
    org_id         TEXT        NOT NULL,
    version        BIGINT      NOT NULL DEFAULT 0,
    -- Stored as TEXT (not JSONB): the exact bytes the reconciler signed must
    -- survive the round-trip verbatim, and JSONB would re-order object keys
    -- inside op specs and break signature verification.
    doc            TEXT        NOT NULL DEFAULT '{}',
    signature      BYTEA,
    doc_hash       TEXT        NOT NULL DEFAULT '',  -- reconciler change detection
    -- Highest DSD version the agent has reported status for; status for a
    -- version below this is ignored (superseded).
    applied_version BIGINT     NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
