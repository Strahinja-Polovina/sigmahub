package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeleteServer decommissions a server. It refuses (ErrConflict, → 409) while
// resources are still bound so the caller can re-home or remove them first.
// Otherwise it SOFT-DELETES: the row and its mesh_ip are retained (only
// deleted_at is set) so allocateMeshIP never re-issues that address to a new
// registration while stale peer configs may still reference it. The agent
// tokens are revoked (its next heartbeat 401s → the agent exits) and the env
// attachments removed. Audited.
func (s *Store) DeleteServer(ctx context.Context, orgID, serverID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var name string
	// FOR UPDATE locks the server row for the whole tx so a concurrent
	// CreateResource (which takes FOR SHARE on the same row) cannot slip a new
	// resource past the bound-resources check below and orphan it on a
	// tombstoned host (SIGMA-132).
	err = tx.QueryRow(ctx,
		`SELECT name FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`,
		orgID, serverID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load server: %w", err)
	}

	// Bound resources block deletion (guarded-destructive posture).
	rows, err := tx.Query(ctx,
		`SELECT name FROM resources WHERE org_id = $1 AND server_id = $2 ORDER BY name`, orgID, serverID)
	if err != nil {
		return fmt.Errorf("list bound resources: %w", err)
	}
	var bound []string
	for rows.Next() {
		var rn string
		if err := rows.Scan(&rn); err != nil {
			rows.Close()
			return err
		}
		bound = append(bound, rn)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(bound) > 0 {
		return fmt.Errorf("%w: server has %d bound resource(s): %s", ErrConflict, len(bound), strings.Join(bound, ", "))
	}

	// Soft-delete tombstone: keep mesh_ip so the allocator can't re-issue it.
	if _, err := tx.Exec(ctx, `UPDATE servers SET deleted_at = now() WHERE id = $1`, serverID); err != nil {
		return fmt.Errorf("tombstone server: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent_tokens SET revoked_at = now() WHERE server_id = $1 AND revoked_at IS NULL`, serverID); err != nil {
		return fmt.Errorf("revoke agent tokens: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM env_servers WHERE server_id = $1`, serverID); err != nil {
		return fmt.Errorf("detach env: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Server deleted", name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeAgentToken revokes a live server's agent token so its next heartbeat
// 401s and the agent exits for re-bootstrap. Audited.
func (s *Store) RevokeAgentToken(ctx context.Context, orgID, serverID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var name string
	err = tx.QueryRow(ctx,
		`SELECT name FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`,
		orgID, serverID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE agent_tokens SET revoked_at = now() WHERE server_id = $1 AND revoked_at IS NULL`, serverID); err != nil {
		return fmt.Errorf("revoke agent token: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Agent token revoked", name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ServiceTokenInfo is a service token's metadata for the list view. The token
// hash is never exposed.
type ServiceTokenInfo struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
}

// ListServiceTokens returns an org's service tokens (metadata only), newest
// first.
func (s *Store) ListServiceTokens(ctx context.Context, orgID string) ([]ServiceTokenInfo, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, name, role, created_by, created_at, last_used_at, revoked_at
		  FROM service_tokens WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ServiceTokenInfo{}
	for rows.Next() {
		var t ServiceTokenInfo
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.CreatedBy, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeServiceToken marks a service token revoked so AuthenticateServiceToken
// stops matching it (next call 401s). Audited.
func (s *Store) RevokeServiceToken(ctx context.Context, orgID, tokenID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var name string
	err = tx.QueryRow(ctx, `
		UPDATE service_tokens SET revoked_at = now()
		 WHERE org_id = $1 AND id = $2 AND revoked_at IS NULL
		 RETURNING name`, orgID, tokenID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke service token: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Service token revoked", name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RotateServiceToken revokes an existing service token and mints a fresh one
// with the same name and role, all in one transaction. The new plaintext is
// returned exactly once. Audited.
func (s *Store) RotateServiceToken(ctx context.Context, orgID, tokenID, actor string) (string, ServicePrincipal, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", ServicePrincipal{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var name, role string
	err = tx.QueryRow(ctx, `
		UPDATE service_tokens SET revoked_at = now()
		 WHERE org_id = $1 AND id = $2 AND revoked_at IS NULL
		 RETURNING name, role`, orgID, tokenID).Scan(&name, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ServicePrincipal{}, ErrNotFound
	}
	if err != nil {
		return "", ServicePrincipal{}, fmt.Errorf("revoke old token: %w", err)
	}

	tok, digest := s.newToken("sst")
	newTokenID := newID("st")
	if _, err := tx.Exec(ctx, `
		INSERT INTO service_tokens (id, org_id, name, role, token_hash, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		newTokenID, orgID, name, role, digest, actor); err != nil {
		return "", ServicePrincipal{}, fmt.Errorf("insert rotated token: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Service token rotated", name); err != nil {
		return "", ServicePrincipal{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", ServicePrincipal{}, err
	}
	return tok, ServicePrincipal{ID: newTokenID, OrgID: orgID, Name: name, Role: Role(role)}, nil
}
