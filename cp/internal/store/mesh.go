package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// meshOffsetMax is the last usable host offset in 10.8.0.0/16 (10.8.255.254;
// .0.0 is the network address and .255.255 broadcast).
const meshOffsetMax = 65534

// allocateMeshIP hands out the next free org-scoped address from 10.8.0.0/16.
// Callers must run it inside the registration transaction; the per-org
// advisory lock serializes concurrent registers so MAX+1 can't collide.
func allocateMeshIP(ctx context.Context, tx pgx.Tx, orgID string) (string, error) {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('mesh:' || $1))`, orgID); err != nil {
		return "", fmt.Errorf("mesh lock: %w", err)
	}
	var maxOffset int
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(split_part(mesh_ip, '.', 3)::int * 256 + split_part(mesh_ip, '.', 4)::int), 0)
		  FROM servers
		 WHERE org_id = $1 AND mesh_ip IS NOT NULL`, orgID).Scan(&maxOffset)
	if err != nil {
		return "", fmt.Errorf("mesh max offset: %w", err)
	}
	next := maxOffset + 1
	if next > meshOffsetMax {
		return "", fmt.Errorf("mesh address space exhausted for org %s", orgID)
	}
	return fmt.Sprintf("10.8.%d.%d", next/256, next%256), nil
}

// MeshPeer is one same-org server another agent should peer with.
type MeshPeer struct {
	ServerID string  `json:"serverId"`
	Name     string  `json:"name"`
	Pubkey   string  `json:"pubkey"`
	MeshIP   string  `json:"meshIp"`
	Endpoint *string `json:"endpoint"` // unknown in v0: agents are outbound-only
}

// MeshPeers lists the requesting server's org-mates that have both a pubkey
// and a mesh IP. The org filter is the isolation boundary — a token can only
// ever see peers of the org its server belongs to.
func (s *Store) MeshPeers(ctx context.Context, orgID, selfServerID string) ([]MeshPeer, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, name, pubkey, mesh_ip
		  FROM servers
		 WHERE org_id = $1 AND id <> $2 AND pubkey IS NOT NULL AND mesh_ip IS NOT NULL
		 ORDER BY created_at`, orgID, selfServerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []MeshPeer{}
	for rows.Next() {
		var p MeshPeer
		if err := rows.Scan(&p.ServerID, &p.Name, &p.Pubkey, &p.MeshIP); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
