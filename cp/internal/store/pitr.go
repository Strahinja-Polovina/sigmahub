package store

// P2-5 phase A: WAL-archiving support. The agent's WAL shipper periodically
// snapshots a postgres resource's spool volume into the same per-resource
// restic repository the daily dumps use (tagged "wal"), then reports the
// high-water mark here. Credentials are released per shipper cycle through
// the same audited path as backup runs.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// WALTarget identifies one PITR-enabled postgres resource on a server, for
// the agent's shipper loop.
type WALTarget struct {
	ResourceID string `json:"resourceId"`
}

// WALTargetsForServer lists the resources whose WAL the server's agent should
// be shipping (BOLA: scoped to the requesting server).
func (s *Store) WALTargetsForServer(ctx context.Context, serverID string) ([]WALTarget, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT dc.resource_id
		  FROM db_credentials dc
		  JOIN backup_policies bp ON bp.resource_id = dc.resource_id
		 WHERE dc.server_id = $1 AND dc.engine = 'postgres'
		   AND bp.enabled AND bp.pitr_enabled AND bp.target_id IS NOT NULL`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WALTarget{}
	for rows.Next() {
		var t WALTarget
		if err := rows.Scan(&t.ResourceID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// WALCredentialForResource releases the restic credential for a resource's
// WAL shipping cycle. Same custody path as BackupCredentialForRun (target
// secret + repo key under the org DEK), scoped to the REQUESTING server, and
// audited — the shipper caches it, so the audit cadence is roughly hourly,
// not per segment.
func (s *Store) WALCredentialForResource(ctx context.Context, serverID, resourceID string) (BackupCredential, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BackupCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID, policyID, endpoint, bucket, region, accessKey string
	var forcePathStyle bool
	var targetCT, targetNonce []byte
	var targetDekID string
	var repoCT, repoNonce []byte
	var repoDekID *string
	err = tx.QueryRow(ctx, `
		SELECT p.org_id, p.id, t.endpoint, t.bucket, t.region, t.access_key, t.force_path_style,
		       t.secret_ciphertext, t.secret_nonce, t.dek_id,
		       p.repo_key_ciphertext, p.repo_key_nonce, p.repo_dek_id
		  FROM backup_policies p
		  JOIN backup_targets t ON t.id = p.target_id
		  JOIN db_credentials dc ON dc.resource_id = p.resource_id
		 WHERE p.resource_id = $1 AND dc.server_id = $2
		   AND p.enabled AND p.pitr_enabled`,
		resourceID, serverID).Scan(&orgID, &policyID, &endpoint, &bucket, &region, &accessKey, &forcePathStyle,
		&targetCT, &targetNonce, &targetDekID, &repoCT, &repoNonce, &repoDekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupCredential{}, ErrNotFound
	}
	if err != nil {
		return BackupCredential{}, err
	}
	if repoDekID == nil || len(repoCT) == 0 {
		// The repo key materializes with the first scheduled run; until then
		// there is nothing to ship into.
		return BackupCredential{}, ErrNotFound
	}

	targetDek, err := s.dekPlaintext(ctx, tx, targetDekID)
	if err != nil {
		return BackupCredential{}, err
	}
	var targetID string
	if err := tx.QueryRow(ctx,
		`SELECT target_id FROM backup_policies WHERE id = $1`, policyID).Scan(&targetID); err != nil {
		return BackupCredential{}, err
	}
	secretKey, err := gcmOpen(targetDek, targetAAD(orgID, targetID), targetNonce, targetCT)
	if err != nil {
		return BackupCredential{}, fmt.Errorf("decrypt target secret: %w", err)
	}
	repoDek, err := s.dekPlaintext(ctx, tx, *repoDekID)
	if err != nil {
		return BackupCredential{}, err
	}
	repoKey, err := gcmOpen(repoDek, repoKeyAAD(orgID, policyID), repoNonce, repoCT)
	if err != nil {
		return BackupCredential{}, fmt.Errorf("decrypt repo key: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, "agent:"+serverID, "WAL repo key unwrapped (agent)", resourceID); err != nil {
		return BackupCredential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BackupCredential{}, err
	}
	return BackupCredential{
		Repository:     resticRepository(endpoint, bucket, resourceID),
		RepoKey:        string(repoKey),
		AccessKey:      accessKey,
		SecretKey:      string(secretKey),
		Region:         region,
		ForcePathStyle: forcePathStyle,
	}, nil
}

// SetWALStatus records a successful shipping cycle's high-water mark (BOLA:
// the resource must live on the reporting server).
func (s *Store) SetWALStatus(ctx context.Context, serverID, resourceID, lastSegment string, at time.Time) error {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO wal_archive_status (resource_id, org_id, last_segment, last_shipped_at, updated_at)
		SELECT dc.resource_id, dc.org_id, $3, $4, now()
		  FROM db_credentials dc
		 WHERE dc.resource_id = $1 AND dc.server_id = $2
		ON CONFLICT (resource_id) DO UPDATE
		   SET last_segment = EXCLUDED.last_segment,
		       last_shipped_at = EXCLUDED.last_shipped_at,
		       updated_at = now()`,
		resourceID, serverID, lastSegment, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
