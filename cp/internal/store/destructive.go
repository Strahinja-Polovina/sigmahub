package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// k8sTeardownWorkloads is every Kubernetes workload name a resource could have
// rendered into its cluster: the single-container name, and one per declared
// Compose service. Both shapes are named on purpose — an app that was a plain
// image before it grew a Compose file left a manifest under the plain name, and
// a delete is the last moment anything will ever look for it. Names that were
// never applied simply match no file on the node.
func k8sTeardownWorkloads(resourceID string, spec json.RawMessage) []string {
	names := []string{dsd.K8sWorkloadName(resourceID, "")}
	var s struct {
		Compose *struct {
			Services []struct {
				Name string `json:"name"`
			} `json:"services"`
		} `json:"compose"`
	}
	if err := json.Unmarshal(spec, &s); err == nil && s.Compose != nil {
		for _, svc := range s.Compose.Services {
			if svc.Name == "" {
				continue
			}
			if n := dsd.K8sWorkloadName(resourceID, svc.Name); n != "" {
				names = append(names, n)
			}
		}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return out
}

// insertK8sTeardownTx queues the removal of a cluster-deployed resource's
// Kubernetes manifests on the cluster's control-plane node (SIGMA-312), and
// returns that node so the caller can re-render it. A resource in no cluster
// (clusterID empty) queues nothing and returns "".
//
// The teardown is pre-authorised the way an ephemeral resource's volume removal
// is: the operator deleted the thing, and what is being removed is compute, not
// data — a manifest file whose objects the API server then garbage-collects. It
// still goes through pending_destructive_ops so it is audited, is re-rendered
// until the node reports it applied, and drops out of the document afterwards.
//
// MUST run while the cluster still exists: deleting a cluster cascades its
// cluster_nodes rows away, and after that there is no control-plane node left to
// address the teardown to.
func insertK8sTeardownTx(ctx context.Context, tx pgx.Tx, orgID, clusterID, resourceID string, spec json.RawMessage) (cpServerID string, err error) {
	if clusterID == "" {
		return "", nil
	}
	err = tx.QueryRow(ctx,
		`SELECT server_id FROM cluster_nodes WHERE cluster_id = $1 AND role = $2`,
		clusterID, NodeRoleControlPlane).Scan(&cpServerID)
	if errors.Is(err, pgx.ErrNoRows) {
		// A cluster with no control-plane node has no applier and therefore no
		// manifests to remove; nothing to queue, and nothing to re-render.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find control-plane node: %w", err)
	}
	names := k8sTeardownWorkloads(resourceID, spec)
	if len(names) == 0 {
		return cpServerID, nil
	}
	// One row per resource carrying every workload name. Kubernetes object names
	// are [a-z0-9-] (dsd.K8sName guarantees it), so a comma is an unambiguous
	// separator for the single-column target the table already models.
	target := strings.Join(names, ",")
	if _, err := insertPendingDestructiveOpTx(ctx, tx, orgID, cpServerID, dsd.KindK8sRemove, target, "system"); err != nil {
		return "", err
	}
	if err := auditTx(ctx, tx, orgID, "system", "Cluster workload teardown queued", target); err != nil {
		return "", err
	}
	return cpServerID, nil
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
