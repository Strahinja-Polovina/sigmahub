-- Mesh IPs are allocated per org from 10.8.0.0/16; two servers in one org
-- must never share an address. Allocation serializes on a per-org advisory
-- lock; this index is the backstop.
CREATE UNIQUE INDEX IF NOT EXISTS servers_org_mesh_ip_idx
    ON servers (org_id, mesh_ip)
 WHERE mesh_ip IS NOT NULL;
