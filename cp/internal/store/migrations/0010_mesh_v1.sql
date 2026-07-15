-- P1-4 (mesh v1): reachable endpoints + server soft-delete tombstones.
--
-- endpoint: the agent's discovered reachable WireGuard endpoint (public
-- ip:port), reported on heartbeat and served in the peer list so tunnels form
-- across NAT. Nullable — a strict-NAT host may never report one.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS endpoint TEXT;

-- deleted_at: soft-delete tombstone. A decommissioned server keeps its row AND
-- its mesh_ip so allocateMeshIP's MAX+1 allocator never re-issues that address
-- to a new registration (a still-cached peer config would otherwise collide).
-- Every peer-serving / auth read filters deleted_at IS NULL to hide tombstones;
-- the allocator's MAX(mesh_ip) query deliberately does NOT — that is the whole
-- point of retaining the tombstone.
ALTER TABLE servers ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;
