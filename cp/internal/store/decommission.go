package store

// Graceful decommission (SIGMA-204): the two-phase disconnect.
//
// Phase 1 (BeginDecommission) marks the row and lets the reconciler render the
// agent.uninstall op. Nothing is destroyed control-plane side, and above all
// the agent token is NOT revoked — the agent needs it to tear the host down and
// then to tell us it did.
//
// Phase 2 (CompleteDecommission) is the old DeleteServer: tombstone, revoke,
// detach, audit. It runs on one of three triggers — the agent's ack, the
// sweeper's timeout, or an operator's force disconnect — and it is the same
// transaction in all three so a machine cannot end up half-removed depending on
// which one got there first.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DecommissionState is what the caller needs after phase 1: enough to tell the
// operator what is now happening, and to nudge the reconciler.
type DecommissionState struct {
	ServerID     string    `json:"serverId"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	PurgeVolumes bool      `json:"purgeVolumes"`
	StartedAt    time.Time `json:"startedAt"`
}

// BeginDecommission starts a graceful decommission: the server moves to
// 'decommissioning', the request is stamped on the row, and the reconciler's
// next render replaces the whole document with a single agent.uninstall op.
//
// It refuses with ErrBoundResources (→ 409, carrying the names) while resources
// are still bound — the same first line of defence DeleteServer has always had,
// kept HERE rather than only on the force path so the ordinary disconnect
// button is the one that explains itself.
//
// purgeVolumes is passed through to the op. It defaults off at every layer
// above this one on purpose: named volumes hold database data directories and
// uploaded files, which belong to the customer and outlive the machine they
// happen to sit on.
//
// Re-running it on a server already decommissioning is idempotent — it re-stamps
// the request (so a second press that ticks the volumes box takes effect) and
// does not restart the timeout clock, because a teardown that is already under
// way must still time out on schedule.
func (s *Store) BeginDecommission(ctx context.Context, orgID, serverID string, purgeVolumes bool, actor string) (DecommissionState, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return DecommissionState{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	name, err := lockServerForDecommission(ctx, tx, orgID, serverID)
	if err != nil {
		return DecommissionState{}, err
	}
	// A cluster member is committed in a way `resources` does not record: its
	// workloads are bound to the CLUSTER, not to this row's server_id, so the
	// bound-resources check sees nothing. Without this, decommissioning a
	// cluster's control-plane node returned 200 and the agent then tore down
	// its DOCKER objects only — k3s, /var/lib/rancher/k3s and every cluster
	// workload kept running on a host the dashboard had just removed, which is
	// the exact "we said it was clean and it was not" failure this whole
	// lifecycle exists to end. Membership is the cluster's to end.
	if cluster, err := clusterMembershipTx(ctx, tx, orgID, serverID); err != nil {
		return DecommissionState{}, err
	} else if cluster != "" {
		return DecommissionState{}, fmt.Errorf("%w: this server is a node of the %s cluster — remove it from the cluster before disconnecting it",
			ErrConflict, cluster)
	}
	bound, err := boundResourcesTx(ctx, tx, orgID, serverID)
	if err != nil {
		return DecommissionState{}, err
	}
	if len(bound) > 0 {
		return DecommissionState{}, ErrBoundResources{Names: bound}
	}

	var startedAt time.Time
	if err := tx.QueryRow(ctx, `
		UPDATE servers
		   SET status = $2,
		       decommission_started_at = COALESCE(decommission_started_at, now()),
		       decommission_purge_volumes = $3,
		       decommission_actor = $4
		 WHERE id = $1
		 RETURNING decommission_started_at`,
		serverID, ServerStatusDecommissioning, purgeVolumes, actor).Scan(&startedAt); err != nil {
		return DecommissionState{}, fmt.Errorf("mark decommissioning: %w", err)
	}
	action := "Server decommissioning"
	if purgeVolumes {
		action = "Server decommissioning (application data included)"
	}
	if err := auditTx(ctx, tx, orgID, actor, action, name); err != nil {
		return DecommissionState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DecommissionState{}, err
	}
	return DecommissionState{
		ServerID:     serverID,
		Name:         name,
		Status:       ServerStatusDecommissioning,
		PurgeVolumes: purgeVolumes,
		StartedAt:    startedAt,
	}, nil
}

// CompleteDecommission finishes a graceful decommission on the AGENT'S ack.
//
// It is keyed by server id alone — no org — because the only caller is the
// agent-token-authenticated ack handler, which already resolved the server from
// the credential; requiring an org id there would mean trusting one off the
// wire. The org is read back out of the row for the audit entry.
//
// ok/detail carry what the agent reported. A failed teardown still completes:
// the agent has already removed itself by the time it can tell us anything, so
// refusing to finish would leave the control plane holding a row for a machine
// that will never speak again — the exact hang this feature exists to end. The
// failure detail goes into the audit entry so the operator knows to run
// agent/packaging/uninstall.sh on the host.
//
// Returns ErrNotFound when the row is already gone (a duplicate ack after the
// sweeper's timeout, or after a force disconnect) so the handler can answer
// "already settled" instead of erroring at an agent that did nothing wrong.
func (s *Store) CompleteDecommission(ctx context.Context, serverID string, ok bool, detail string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID, name string
	var startedAt *time.Time
	var requestedBy string
	err = tx.QueryRow(ctx, `
		SELECT org_id, name, decommission_started_at, decommission_actor
		  FROM servers
		 WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, serverID).
		Scan(&orgID, &name, &startedAt, &requestedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if startedAt == nil {
		// An ack for a server nobody asked to decommission. Refuse rather than
		// tombstone: this is the one message on the agent channel that destroys
		// a server, and it must only ever finish work the control plane started.
		return fmt.Errorf("%w: server %s is not decommissioning", ErrConflict, serverID)
	}
	// Attribute the audit row to whoever pressed the button, not to the agent
	// that happens to be delivering the news minutes later.
	actor := requestedBy
	if actor == "" {
		actor = "sigmad"
	}
	action := "Server decommissioned"
	if !ok {
		action = "Server decommissioned with errors — " + clipFact(detail)
	}
	if err := tombstoneServerTx(ctx, tx, orgID, serverID, name, actor, action); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DecommissionTimedOut is one server whose graceful teardown never acked.
type DecommissionTimedOut struct {
	ServerID string
	OrgID    string
	Name     string
}

// TimeoutStaleDecommissions force-completes decommissions that have been in
// flight longer than `timeout` — the sweeper's half of "on ack OR a timeout".
//
// Without it a host that died mid-teardown (or was powered off between the
// operator pressing the button and the agent picking the op up) holds its row
// forever: it is not running, so nothing else transitions it, and the dashboard
// shows a spinner nobody can clear. The tombstone is written exactly as the
// force path writes it, and the audit entry says it was the timeout so the
// difference between "the machine told us" and "we gave up waiting" survives.
//
// The cutoff is computed in SQL (now() - interval) so it stays in the DB clock
// domain that wrote decommission_started_at.
func (s *Store) TimeoutStaleDecommissions(ctx context.Context, timeout time.Duration) ([]DecommissionTimedOut, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH stale AS (
			UPDATE servers
			   SET deleted_at = now()
			 WHERE status = $1
			   AND deleted_at IS NULL
			   AND decommission_started_at IS NOT NULL
			   AND decommission_started_at < now() - make_interval(secs => $2)
			 RETURNING id, org_id, name, decommission_actor
		),
		revoked AS (
			UPDATE agent_tokens t SET revoked_at = now()
			  FROM stale s WHERE t.server_id = s.id AND t.revoked_at IS NULL
		),
		detached AS (
			DELETE FROM env_servers e USING stale s WHERE e.server_id = s.id
		),
		audited AS (
			INSERT INTO cp_audit_log (org_id, actor, action, target)
			SELECT org_id, COALESCE(NULLIF(decommission_actor, ''), 'sweeper'),
			       'Server decommission timed out — removed without the agent''s confirmation',
			       name
			  FROM stale
		)
		SELECT id, org_id, name FROM stale`,
		ServerStatusDecommissioning, timeout.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DecommissionTimedOut
	for rows.Next() {
		var d DecommissionTimedOut
		if err := rows.Scan(&d.ServerID, &d.OrgID, &d.Name); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
