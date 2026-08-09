package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// SetDesiredAgentVersion records the version the operator wants a server's
// agent upgraded to; the reconciler renders an agent.update op until the
// agent's heartbeat converges on it. Audited.
func (s *Store) SetDesiredAgentVersion(ctx context.Context, orgID, serverID, version, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var name string
	err = tx.QueryRow(ctx, `
		UPDATE servers SET desired_agent_version = $1
		 WHERE id = $2 AND org_id = $3 AND deleted_at IS NULL
		 RETURNING name`, version, serverID, orgID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, $2, $3, $4)`,
		orgID, actor, "Agent update requested ("+version+")", name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ErrBoundResources is the 409 a disconnect answers while resources are still
// bound to the host. It carries the NAMES rather than only a sentence: the
// dialog that raised the refusal has to list what is in the way so the operator
// can go re-home or delete those resources, and parsing them back out of an
// error string is how a UI ends up printing a raw Go error at a customer
// (SIGMA-205). It satisfies errors.Is(err, ErrConflict) so every existing
// caller — writeStoreErr's 409 branch included — keeps working untouched.
type ErrBoundResources struct {
	Names []string
}

func (e ErrBoundResources) Error() string {
	return fmt.Sprintf("%s: server has %d bound resource(s): %s",
		ErrConflict.Error(), len(e.Names), strings.Join(e.Names, ", "))
}

// Is makes errors.Is(err, ErrConflict) true, so this type can be introduced
// under existing 409 handling without touching it.
func (e ErrBoundResources) Is(target error) bool { return target == ErrConflict }

// lockServerForDecommission takes the row lock every disconnect path needs and
// returns the server's name.
//
// FOR UPDATE locks the server row for the whole tx so a concurrent
// CreateResource (which takes FOR SHARE on the same row) cannot slip a new
// resource past the bound-resources check and orphan it on a tombstoned host
// (SIGMA-132).
func lockServerForDecommission(ctx context.Context, tx pgx.Tx, orgID, serverID string) (string, error) {
	var name string
	err := tx.QueryRow(ctx,
		`SELECT name FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL FOR UPDATE`,
		orgID, serverID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("load server: %w", err)
	}
	return name, nil
}

// boundResourcesTx lists the resources still bound to a server, in name order.
//
// "Bound" is ResourceHostedHere's question, asked with ResourceHostedHere's
// answer, and that is the whole point (SIGMA-229): the guard that decides
// whether a host may be torn down has to count the same workloads the renderer
// would put in that host's document. It used to ask only `resources.server_id`,
// which misses two of the three ways a resource lands on a machine.
//
// The one that bit was per-service Compose placement. A Compose app owned by
// srv_app can have its worker and redis services dragged onto srv_data; the
// placement lives in the app's SPEC, not in a resources row, so srv_data's
// Resources tab was empty and the disconnect dialog reported zero blockers. The
// graceful teardown then ran to completion — containers force-removed, managed
// network gone, row tombstoned — while the spec still named srv_data as those
// services' serverId, so no other document ever rendered them again. The app
// stayed permanently half-running and nothing anywhere reported an error.
//
// Cluster membership is the third case. The graceful path refuses a cluster
// member earlier and with a better sentence (clusterMembershipTx), so this only
// changes the FORCE path, where a node carrying cluster workloads now has to be
// removed from its cluster first — RemoveClusterNode works on an unreachable
// host, so the escape hatch is still reachable, it just no longer leaves k3s
// workloads pointing at a server the product has forgotten.
func boundResourcesTx(ctx context.Context, tx pgx.Tx, orgID, serverID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT r.name FROM resources r
		  WHERE r.org_id = $1 AND`+ResourceHostedHere("$2")+`
		  ORDER BY r.name`, orgID, serverID)
	if err != nil {
		return nil, fmt.Errorf("list bound resources: %w", err)
	}
	defer rows.Close()
	var bound []string
	for rows.Next() {
		var rn string
		if err := rows.Scan(&rn); err != nil {
			return nil, err
		}
		bound = append(bound, rn)
	}
	return bound, rows.Err()
}

// buildServerUseTx reports whether a server is still named as somebody's
// dedicated BUILD server, and by what, or "" when nothing names it.
//
// A build server hosts no resource of its own — that is the entire idea — so
// the bound-resources guard, however widened, cannot see it: what puts the
// clone+build ops in its document is a deployment or a branch map naming it as
// build_server_id (see deploymentReporterClause, which counts it as one of the
// four ways a server owns part of a deployment). Disconnecting it therefore
// looked completely clean and left two different wounds:
//
//   - an in-flight deployment whose build ops were rendered into a machine that
//     is being torn down under them. Nothing else can run or report those ops,
//     so the pipeline sits in 'building' until it times out, if it ever does;
//   - a branch map still pointing at a tombstoned server, which is silent until
//     the next push to that branch and then breaks every auto-deploy on it.
//
// Only LIVE deployments count — the same statuses ClusterBuildSpecsForServer
// renders for. A finished build is history, and blocking on history would wedge
// the force path forever. A branch map counts unconditionally: it is standing
// configuration, and the operator clears it by picking another builder.
func buildServerUseTx(ctx context.Context, tx pgx.Tx, orgID, serverID string) (string, error) {
	var deployments, branches int
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM deployments
		         WHERE org_id = $1 AND build_server_id = $2
		           AND status IN ('queued','building','deploying')),
		       (SELECT count(*) FROM git_branch_map m
		          JOIN git_connections c ON c.id = m.connection_id
		         WHERE c.org_id = $1 AND m.build_server_id = $2)`,
		orgID, serverID).Scan(&deployments, &branches); err != nil {
		return "", fmt.Errorf("check build server use: %w", err)
	}
	switch {
	case deployments > 0 && branches > 0:
		return fmt.Sprintf("%d deployment(s) are building on it and %d branch(es) are configured to build on it",
			deployments, branches), nil
	case deployments > 0:
		return fmt.Sprintf("%d deployment(s) are building on it", deployments), nil
	case branches > 0:
		return fmt.Sprintf("%d branch(es) are configured to build on it", branches), nil
	}
	return "", nil
}

// refuseIfBuildServerTx turns buildServerUseTx into the 409 both disconnect
// paths answer with.
func refuseIfBuildServerTx(ctx context.Context, tx pgx.Tx, orgID, serverID string) error {
	use, err := buildServerUseTx(ctx, tx, orgID, serverID)
	if err != nil {
		return err
	}
	if use != "" {
		return fmt.Errorf("%w: this server is a build server — %s; point them at another server first",
			ErrConflict, use)
	}
	return nil
}

// tombstoneServerTx is the terminal half of a disconnect: the soft-delete
// tombstone, the token revocation and the env detach, with `action` as the
// audit verb (the graceful path and the force path record different sentences
// for the same row change, and an operator reading the log needs to know which
// happened).
//
// Soft-delete keeps the row AND its mesh_ip so allocateMeshIP never re-issues
// that address to a new registration while stale peer configs may still
// reference it. Revoking the agent tokens is what makes the agent exit: its
// next heartbeat 401s. That is also why nothing in the graceful path may call
// this before the agent has acked — a revoked token is an agent that can no
// longer report, which is precisely the state that leaves the control plane
// waiting on a machine that already finished.
func tombstoneServerTx(ctx context.Context, tx pgx.Tx, orgID, serverID, name, actor, action string) error {
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
	// Cluster membership too. The graceful path refuses a cluster member
	// outright, but the FORCE path exists precisely for hosts that cannot be
	// asked nicely — and leaving the row behind left the cluster reporting a
	// node count that included a tombstoned server, with
	// ControlPlaneServerForCluster still handing that id to the renderer.
	if _, err := tx.Exec(ctx, `DELETE FROM cluster_nodes WHERE server_id = $1`, serverID); err != nil {
		return fmt.Errorf("detach cluster: %w", err)
	}
	return auditTx(ctx, tx, orgID, actor, action, name)
}

// DeleteServer is the FORCE disconnect: tombstone + token revocation, with no
// teardown on the host. It refuses (ErrBoundResources, → 409) while resources
// are still bound so the caller can re-home or remove them first.
//
// Since SIGMA-204 this is no longer the ordinary disconnect — BeginDecommission
// is — because everything the platform installed on the machine survives this
// call. It stays as the escape hatch for the two cases a graceful decommission
// cannot serve: a host that is already unreachable (nothing is listening to
// take the uninstall op) and one whose teardown timed out. Both hand the
// operator agent/packaging/uninstall.sh to finish the job by hand. Audited.
func (s *Store) DeleteServer(ctx context.Context, orgID, serverID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	name, err := lockServerForDecommission(ctx, tx, orgID, serverID)
	if err != nil {
		return err
	}
	// Bound resources block deletion (guarded-destructive posture).
	bound, err := boundResourcesTx(ctx, tx, orgID, serverID)
	if err != nil {
		return err
	}
	if len(bound) > 0 {
		return ErrBoundResources{Names: bound}
	}
	// A build server owns no resource, so the check above cannot see it.
	if err := refuseIfBuildServerTx(ctx, tx, orgID, serverID); err != nil {
		return err
	}
	if err := tombstoneServerTx(ctx, tx, orgID, serverID, name, actor, "Server deleted"); err != nil {
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

// clusterMembershipTx returns the name of the cluster this server is a node of,
// or "" when it is a member of none.
//
// Two callers refuse on it — changing a server's type and disconnecting it —
// because a cluster member is committed in a way `resources` does not record:
// its workloads are bound to the CLUSTER, not to this row's server_id, so a
// bound-resources check sees nothing and waves the change through. Re-filing
// the type that way halved a control-plane node's bill while it was still
// running the cluster; disconnecting that way tore down the host's DOCKER
// objects only and left k3s, /var/lib/rancher/k3s and every cluster workload
// running on a machine the dashboard had just removed.
func clusterMembershipTx(ctx context.Context, tx pgx.Tx, orgID, serverID string) (string, error) {
	var name string
	err := tx.QueryRow(ctx, `
		SELECT c.name FROM cluster_nodes n
		  JOIN clusters c ON c.id = n.cluster_id
		 WHERE n.server_id = $1 AND c.org_id = $2
		 LIMIT 1`, serverID, orgID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check cluster membership: %w", err)
	}
	return name, nil
}
