package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
)

// IssueConfirmToken (phase 1 of the two-phase destructive-op flow) mints a
// single-use, short-lived token authorising exactly one destructive op on one
// server, and audits the request. The plaintext is returned once; only the
// keyed digest is persisted.
func (s *Store) IssueConfirmToken(ctx context.Context, orgID, serverID, opKind, target, createdBy string, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	tok, digest := s.newToken("sct")
	expiresAt = time.Now().Add(ttl)

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO destructive_confirm_tokens (id, org_id, server_id, op_kind, target, token_hash, created_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		newID("sct"), orgID, serverID, opKind, target, digest, createdBy, expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("insert confirm token: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, createdBy, "Destructive-op confirm requested", opKind+" "+target); err != nil {
		return "", time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", time.Time{}, err
	}
	return tok, expiresAt, nil
}

// ConfirmDestructiveOp (phase 2) atomically claims a confirm token and records
// the destructive op as pending so the reconciler renders it into the server's
// DSD. The token must match the requested (server, op_kind, target) exactly —
// a claimed token cannot be redirected to a different target. Returns
// ErrNotFound if the token is missing, expired, or already used.
func (s *Store) ConfirmDestructiveOp(ctx context.Context, orgID, token, serverID, opKind, target, actor string) (pdoID string, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var tokServer, tokKind, tokTarget string
	err = tx.QueryRow(ctx, `
		UPDATE destructive_confirm_tokens
		   SET used_at = now()
		 WHERE token_hash = $1 AND org_id = $2 AND used_at IS NULL AND expires_at > now()
		 RETURNING server_id, op_kind, target`,
		s.hashToken(token), orgID).Scan(&tokServer, &tokKind, &tokTarget)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("claim confirm token: %w", err)
	}
	if tokServer != serverID || tokKind != opKind || tokTarget != target {
		return "", ErrInvalid{Msg: "confirm token does not authorise this operation"}
	}

	pdoID, err = insertPendingDestructiveOpTx(ctx, tx, orgID, serverID, opKind, target, actor)
	if err != nil {
		return "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Destructive-op confirmed", opKind+" "+target); err != nil {
		return "", err
	}
	return pdoID, tx.Commit(ctx)
}

// insertPendingDestructiveOpTx records a confirmed destructive op inside an
// existing transaction. Shared by the interactive confirm and the ephemeral
// system-actor path.
func insertPendingDestructiveOpTx(ctx context.Context, tx pgx.Tx, orgID, serverID, opKind, target, createdBy string) (string, error) {
	id := newID("pdo")
	if _, err := tx.Exec(ctx, `
		INSERT INTO pending_destructive_ops (id, org_id, server_id, op_kind, target, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, orgID, serverID, opKind, target, createdBy); err != nil {
		return "", fmt.Errorf("insert pending destructive op: %w", err)
	}
	return id, nil
}

// resourceVolumeNames extracts the Docker volume names a resource declares, so
// its ephemeral teardown knows what to remove. A spec that declares no volumes
// yields none.
func resourceVolumeNames(resourceID string, spec json.RawMessage) []string {
	var s struct {
		Volumes []struct {
			Name string `json:"name"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(spec, &s); err != nil {
		return nil
	}
	out := make([]string, 0, len(s.Volumes))
	for _, v := range s.Volumes {
		if v.Name != "" {
			out = append(out, dsd.VolumeName(resourceID, v.Name))
		}
	}
	return out
}
