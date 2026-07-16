-- P1-5 opt-out hardening. Desired per-server hardening config drives the typed
-- host.* DSD ops; the agent reports its self-assessed posture over the heartbeat.

CREATE TABLE IF NOT EXISTS server_hardening (
    server_id       TEXT PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
    keep_public_ssh BOOLEAN NOT NULL DEFAULT FALSE, -- opt-out of SSH lockdown
    cis_enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    extra_ports     JSONB   NOT NULL DEFAULT '[]',  -- [{port,proto}] customer exceptions
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Reported posture (from the agent's daily drift re-check), surfaced in the UI.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS hardening_score      INT;
ALTER TABLE servers ADD COLUMN IF NOT EXISTS disk_encrypted       BOOLEAN;
ALTER TABLE servers ADD COLUMN IF NOT EXISTS ssh_locked           BOOLEAN;
ALTER TABLE servers ADD COLUMN IF NOT EXISTS hardening_checked_at TIMESTAMPTZ;
