package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// meshInterface is the WireGuard link name the agent brings up (sigma0.conf →
// sigma0); the firewall allows all traffic arriving on it.
const meshInterface = "sigma0"

// PortException is a customer-declared inbound firewall allowance.
type PortException struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"` // tcp|udp
}

// HostHardening is the reconciler's render input for the host.* ops: the
// server's mesh/proxy identity plus its (possibly defaulted) hardening config.
type HostHardening struct {
	MeshIP        string
	MeshInterface string
	ProxyRole     bool
	KeepPublicSSH bool
	CISEnabled    bool
	ExtraPorts    []PortException
}

// HostHardeningForServer returns the effective hardening config for a server,
// defaulting (lock down SSH, CIS on, no extra ports) when no row has been set.
func (s *Store) HostHardeningForServer(ctx context.Context, serverID string) (HostHardening, error) {
	var (
		meshIP      *string
		proxyRole   bool
		keepSSH     bool
		cisEnabled  bool
		extraRaw    []byte
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT s.mesh_ip, s.proxy_role,
		       COALESCE(h.keep_public_ssh, FALSE),
		       COALESCE(h.cis_enabled, TRUE),
		       COALESCE(h.extra_ports, '[]'::jsonb)
		  FROM servers s
		  LEFT JOIN server_hardening h ON h.server_id = s.id
		 WHERE s.id = $1 AND s.deleted_at IS NULL`, serverID).
		Scan(&meshIP, &proxyRole, &keepSSH, &cisEnabled, &extraRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return HostHardening{}, ErrNotFound
	}
	if err != nil {
		return HostHardening{}, err
	}
	hh := HostHardening{
		MeshInterface: meshInterface,
		ProxyRole:     proxyRole,
		KeepPublicSSH: keepSSH,
		CISEnabled:    cisEnabled,
	}
	if meshIP != nil {
		hh.MeshIP = *meshIP
	}
	if len(extraRaw) > 0 {
		_ = json.Unmarshal(extraRaw, &hh.ExtraPorts)
	}
	return hh, nil
}

// SetHardeningPosture records the agent's self-assessed posture from a heartbeat.
func (s *Store) SetHardeningPosture(ctx context.Context, serverID string, score int, diskEncrypted, sshLocked bool) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE servers
		   SET hardening_score = $2, disk_encrypted = $3, ssh_locked = $4, hardening_checked_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`,
		serverID, score, diskEncrypted, sshLocked)
	return err
}

// SetHardeningConfig upserts the desired hardening config for a server (used by
// the dashboard to toggle the keep-public-SSH opt-out, CIS, and exceptions).
// Tenant-isolated: the server must belong to the org. Audited.
func (s *Store) SetHardeningConfig(ctx context.Context, orgID, serverID string, keepPublicSSH, cisEnabled bool, extraPorts []PortException, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM servers WHERE id = $1 AND org_id = $2 AND deleted_at IS NULL`, serverID, orgID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	extra, err := json.Marshal(extraPorts)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO server_hardening (server_id, keep_public_ssh, cis_enabled, extra_ports)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (server_id) DO UPDATE
		   SET keep_public_ssh = $2, cis_enabled = $3, extra_ports = $4, updated_at = now()`,
		serverID, keepPublicSSH, cisEnabled, extra); err != nil {
		return fmt.Errorf("upsert hardening: %w", err)
	}
	action := "Hardening updated"
	if keepPublicSSH {
		action = "Hardening updated (public SSH kept open)"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, $2, $3, $4)`, orgID, actor, action, serverID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
