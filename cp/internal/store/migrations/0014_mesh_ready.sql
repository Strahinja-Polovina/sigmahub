-- P1-5 mesh-gated Ready. The agent reports, on the heartbeat, that it has
-- applied its WireGuard peer config and how many peers it covers. Combined with
-- liveness (status=running) and a formable same-org peer, this derives Ready.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS mesh_synced_at  TIMESTAMPTZ;
ALTER TABLE servers ADD COLUMN IF NOT EXISTS mesh_peer_count INT NOT NULL DEFAULT 0;
