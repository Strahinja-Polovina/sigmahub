package store

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
)

// LockServerReconcile takes a cluster-wide advisory lock that serializes DSD
// reconciles for one server, so two overlapping reconciles (a nudge racing the
// 60s resync, or multiple replicas) can't lost-update the document to a stale
// snapshot — the later-committing reconcile might otherwise hold an OLDER read
// (SIGMA-94).
//
// It uses pg_TRY_advisory_lock in a bounded retry loop that RELEASES the pool
// connection between attempts (SIGMA-120). The lock is held on a checked-out pool
// connection for the whole reconcile, so BLOCKING on it (pg_advisory_lock) would
// pin that connection while the reconcile needs several MORE pool connections for
// its reads+write; under a burst of same-server reconciles that pins every pool
// connection on lock-waiters and the winner can't get a connection for its own
// queries — a pool-exhaustion deadlock. Blocking-free skip-on-first-contention is
// the other extreme: a synchronous caller that expects the reconcile to have run
// (e.g. right after confirming a destructive op) would silently no-op. So we wait
// a short, bounded time — retrying pg_try_advisory_lock while holding NO
// connection during the sleep — which lets a brief concurrent reconcile finish
// without pinning connections. Only if the lock stays held past the deadline do
// we report acquired=false and let the caller skip (safe: reconcile is
// level-triggered and the 60s resync re-runs it). Returns (unlock, acquired, err);
// call unlock (deferred) only when acquired is true.
func (s *Store) LockServerReconcile(ctx context.Context, serverID string) (func(), bool, error) {
	deadline := time.Now().Add(reconcileLockWait)
	for {
		conn, err := s.Pool.Acquire(ctx)
		if err != nil {
			return nil, false, err
		}
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('reconcile:' || $1))`, serverID).Scan(&got); err != nil {
			conn.Release()
			return nil, false, err
		}
		if got {
			return func() {
				// Best-effort unlock on a fresh context (the reconcile ctx may be done).
				uctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = conn.Exec(uctx, `SELECT pg_advisory_unlock(hashtext('reconcile:' || $1))`, serverID)
				conn.Release()
			}, true, nil
		}
		// Contended: release the connection (never hold it while waiting, or the
		// pool exhausts) and retry until the deadline, then skip.
		conn.Release()
		if time.Now().After(deadline) {
			return func() {}, false, nil
		}
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(reconcileLockRetry):
		}
	}
}

const (
	// reconcileLockWait bounds how long a reconcile waits for a concurrent
	// same-server reconcile to finish before skipping (SIGMA-120).
	reconcileLockWait = 5 * time.Second
	// reconcileLockRetry is the poll interval while waiting for the lock.
	reconcileLockRetry = 25 * time.Millisecond
)

// maxDSDRedrive caps how many times StoreDSD re-issues an UNCHANGED document to
// retry a failed apply (SIGMA-116). At the 60s resync cadence this is ~5 minutes
// of retries for a transient failure; a permanently-failing op then stops
// churning the version/audit log until a real config change resets the budget.
const maxDSDRedrive = 5

const dsdSigningKeyName = "dsd_signing_key"

// LoadDSDSigningKey returns the CP's Ed25519 DSD-signing key, generating and
// wrapping the seed on first boot (same custody path as the token pepper).
// The key is stable across restarts because agents pin its public half at
// enrollment; unwrapping it emits one audit event via the custody sink.
func (s *Store) LoadDSDSigningKey(ctx context.Context, custody kms.KeyCustody) (ed25519.PrivateKey, error) {
	var wrapped []byte
	err := s.Pool.QueryRow(ctx,
		`SELECT wrapped FROM cp_secrets WHERE name = $1`, dsdSigningKeyName).Scan(&wrapped)

	if errors.Is(err, pgx.ErrNoRows) {
		_, priv, genErr := ed25519.GenerateKey(nil)
		if genErr != nil {
			return nil, genErr
		}
		seed := priv.Seed()
		wrapped, err = custody.Wrap(ctx, dsdSigningKeyName, seed)
		if err != nil {
			return nil, fmt.Errorf("wrap dsd key: %w", err)
		}
		if _, err := s.Pool.Exec(ctx, `
			INSERT INTO cp_secrets (name, wrapped) VALUES ($1, $2)
			ON CONFLICT (name) DO NOTHING`, dsdSigningKeyName, wrapped); err != nil {
			return nil, fmt.Errorf("insert dsd key: %w", err)
		}
		if err := s.Pool.QueryRow(ctx,
			`SELECT wrapped FROM cp_secrets WHERE name = $1`, dsdSigningKeyName).Scan(&wrapped); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	seed, err := custody.Unwrap(ctx, dsdSigningKeyName, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrap dsd key: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("dsd key seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// GetDSD returns the current signed DSD for a server, or ErrNotFound when none
// has been rendered yet (version 0).
func (s *Store) GetDSD(ctx context.Context, serverID string) (dsd.Signed, error) {
	var docJSON []byte
	var sig []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT doc, signature FROM server_dsd WHERE server_id = $1 AND version > 0`,
		serverID).Scan(&docJSON, &sig)
	if errors.Is(err, pgx.ErrNoRows) {
		return dsd.Signed{}, ErrNotFound
	}
	if err != nil {
		return dsd.Signed{}, err
	}
	var doc dsd.Document
	if err := json.Unmarshal(docJSON, &doc); err != nil {
		return dsd.Signed{}, err
	}
	return dsd.Signed{Document: doc, Signature: sig}, nil
}

// CurrentDSDVersion returns the server's current DSD version (0 if none).
func (s *Store) CurrentDSDVersion(ctx context.Context, serverID string) (int64, error) {
	var v int64
	err := s.Pool.QueryRow(ctx,
		`SELECT version FROM server_dsd WHERE server_id = $1`, serverID).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// StoreDSD persists a newly rendered DSD only if docHash changed, bumping the
// version monotonically and signing under key. Returns the stored signed
// document and whether it changed (false → nothing to deliver). One audit row
// is written per issued (changed) DSD. The row is created lazily on first
// render (INSERT ... ON CONFLICT), so the reconciler needs no pre-seeding.
// ForceReapplyResource makes "Redeploy" unconditional for resources with no
// git deployment to replay (databases, object storage, registry apps): it
// clears the server's DSD hash + re-drive budget so the next render issues a
// fresh version even when nothing changed, and the agent re-runs every op —
// handlers are idempotent, and a previously-failed op (the reason the operator
// is clicking Redeploy) gets its retry. Returns the server to re-render.
func (s *Store) ForceReapplyResource(ctx context.Context, orgID, resourceID, actor string) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var serverID *string
	var name string
	err = tx.QueryRow(ctx,
		`SELECT server_id, name FROM resources WHERE org_id = $1 AND id = $2`,
		orgID, resourceID).Scan(&serverID, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if serverID == nil || *serverID == "" {
		return "", ErrInvalid{Msg: "resource is not scheduled on a server"}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE server_dsd SET doc_hash = '', redrive_count = 0
		 WHERE org_id = $1 AND server_id = $2`, orgID, *serverID); err != nil {
		return "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Redeploy forced (re-apply)", name); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return *serverID, nil
}

func (s *Store) StoreDSD(ctx context.Context, orgID, serverID string, ops []dsd.Op, docHash string, priv ed25519.PrivateKey) (dsd.Signed, bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the row so concurrent reconciles (event + resync) serialize and the
	// version never regresses or duplicates.
	var curVersion, appliedVersion int64
	var curHash string
	var applyOK bool
	var redriveCount int
	err = tx.QueryRow(ctx, `
		INSERT INTO server_dsd (server_id, org_id, version, doc_hash)
		VALUES ($1, $2, 0, '')
		ON CONFLICT (server_id) DO UPDATE SET server_id = server_dsd.server_id
		RETURNING version, doc_hash, applied_version, apply_ok, redrive_count`,
		serverID, orgID).Scan(&curVersion, &curHash, &appliedVersion, &applyOK, &redriveCount)
	if err != nil {
		return dsd.Signed{}, false, fmt.Errorf("lock dsd row: %w", err)
	}
	sameDoc := curHash == docHash && curVersion > 0
	// The applied version has fully converged when the agent reported every op
	// applied (apply_ok), or an apply is still in flight (applied_version <
	// curVersion) so the ops may yet succeed — re-issuing mid-apply would churn
	// the version. Only a caught-up, failed apply is a candidate for re-drive.
	converged := applyOK || appliedVersion < curVersion
	if sameDoc && converged {
		return dsd.Signed{}, false, tx.Commit(ctx)
	}
	// Bounded re-drive (SIGMA-104 / SIGMA-116): when the desired ops are unchanged
	// but the last apply failed, re-issue them as a new version so the agent
	// retries (it only applies versions greater than the one it last saw). Cap the
	// retries so a PERMANENTLY-failing op (e.g. a mistyped image tag) does not
	// re-issue a new signed version + audit row on every 60s resync forever; after
	// the cap we suppress and leave the failure visible on resources.status until a
	// real config change (a new doc_hash) resets the budget.
	if sameDoc && !converged && redriveCount >= maxDSDRedrive {
		return dsd.Signed{}, false, tx.Commit(ctx)
	}

	next := curVersion + 1
	doc := dsd.Document{Version: next, OrgID: orgID, ServerID: serverID, Ops: ops}
	sig, err := dsd.Sign(priv, doc)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	docJSON, err := json.Marshal(doc)
	if err != nil {
		return dsd.Signed{}, false, err
	}
	// A re-drive of the SAME failing doc increments the retry budget; any real
	// change (new doc_hash) resets it to 0. apply_ok resets to true for the freshly
	// issued version (not yet known to have failed); the agent's next status report
	// sets it authoritatively.
	nextRedrive := 0
	if sameDoc {
		nextRedrive = redriveCount + 1
	}
	if _, err := tx.Exec(ctx, `
		UPDATE server_dsd
		   SET version = $2, doc = $3, signature = $4, doc_hash = $5, apply_ok = true, redrive_count = $6, updated_at = now()
		 WHERE server_id = $1`,
		serverID, next, docJSON, sig, docHash, nextRedrive); err != nil {
		return dsd.Signed{}, false, fmt.Errorf("update dsd: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, "reconciler", "DSD issued",
		fmt.Sprintf("%s v%d", serverID, next)); err != nil {
		return dsd.Signed{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return dsd.Signed{}, false, err
	}
	return dsd.Signed{Document: doc, Signature: sig}, true, nil
}

// ResourceSpecsForServer returns the rows the reconciler renders a server's DSD
// from. ProjectID drives per-project Docker network naming; Ephemeral flags
// preview resources whose teardown skips interactive approval.
type ResourceSpec struct {
	ResourceID string
	ProjectID  string
	Kind       string
	Spec       json.RawMessage
	Ephemeral  bool
}

func (s *Store) ResourceSpecsForServer(ctx context.Context, serverID string) ([]ResourceSpec, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, project_id, kind, spec, ephemeral FROM resources WHERE server_id = $1 ORDER BY created_at`,
		serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceSpec{}
	for rows.Next() {
		var r ResourceSpec
		if err := rows.Scan(&r.ResourceID, &r.ProjectID, &r.Kind, &r.Spec, &r.Ephemeral); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PendingDestructiveOp is a confirmed destructive action awaiting agent
// application, rendered into the server's DSD until applied.
type PendingDestructiveOp struct {
	ID     string
	OpKind string
	Target string
}

// PendingDestructiveOpsForServer returns a server's still-unapplied destructive
// ops (applied_at IS NULL) for the given org, oldest first for deterministic op
// ordering. The org filter is defence in depth: rows are only written for
// org-owned servers, but rendering must never leak an op across tenants.
func (s *Store) PendingDestructiveOpsForServer(ctx context.Context, orgID, serverID string) ([]PendingDestructiveOp, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, op_kind, target FROM pending_destructive_ops
		WHERE server_id = $1 AND org_id = $2 AND applied_at IS NULL ORDER BY created_at, id`,
		serverID, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingDestructiveOp{}
	for rows.Next() {
		var p PendingDestructiveOp
		if err := rows.Scan(&p.ID, &p.OpKind, &p.Target); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkDestructiveOpApplied records that the agent applied a destructive op so it
// drops out of future DSDs.
func (s *Store) MarkDestructiveOpApplied(ctx context.Context, serverID, id string) error {
	// Scope by server_id (SIGMA-74): a compromised agent must not be able to mark
	// another server's destructive op applied — matches every sibling agent-status
	// write. server_id is the executing server the op was rendered for.
	_, err := s.Pool.Exec(ctx,
		`UPDATE pending_destructive_ops SET applied_at = now() WHERE id = $1 AND server_id = $2 AND applied_at IS NULL`, id, serverID)
	return err
}

// AllServerIDs lists every non-deleted server id, for the periodic resync. The
// deleted_at filter is load-bearing (SIGMA-107): DeleteServer is a soft-delete
// tombstone and there is no hard delete, so without it the resync would
// reconcile every server ever deleted on each cycle — each failing at
// HostHardeningForServer (which filters deleted_at) and logging an error.
func (s *Store) AllServerIDs(ctx context.Context) ([]struct{ ServerID, OrgID string }, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, org_id FROM servers WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct{ ServerID, OrgID string }
	for rows.Next() {
		var id, org string
		if err := rows.Scan(&id, &org); err != nil {
			return nil, err
		}
		out = append(out, struct{ ServerID, OrgID string }{id, org})
	}
	return out, rows.Err()
}

// ApplyDSDStatus records agent-reported op results for a server at a given DSD
// version. Status for a version below the last recorded one is ignored
// (superseded). Per-resource op states in opStatus are written into
// resources.status. `converged` is the caller's whole-document convergence
// signal — true only when EVERY reported op (resource, host, proxy, volume.remove,
// …) applied; it drives apply_ok and thus the SIGMA-104 resync re-drive. It must
// be computed from the full op-status set, not just the resource-scoped opStatus
// map (SIGMA-117) — otherwise a failed host/proxy/volume.remove op would report
// false convergence and never be retried. Returns false when the report was
// superseded (ignored).
func (s *Store) ApplyDSDStatus(ctx context.Context, serverID string, version int64, opStatus map[string]json.RawMessage, converged bool) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var applied, issued int64
	err = tx.QueryRow(ctx,
		`SELECT applied_version, version FROM server_dsd WHERE server_id = $1 FOR UPDATE`, serverID).Scan(&applied, &issued)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	// Reject superseded (below last-applied) and bogus (above the version the
	// CP has actually issued) reports. The reported version is agent-supplied,
	// so without the upper bound a compromised/buggy agent could set
	// applied_version arbitrarily high and permanently freeze this server's
	// status — every honest report would then read as superseded.
	if version < applied || version > issued {
		return false, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE server_dsd SET applied_version = $2, apply_ok = $3 WHERE server_id = $1`, serverID, version, converged); err != nil {
		return false, err
	}
	for resourceID, st := range opStatus {
		// Only touch resources that belong to this server (defence in depth
		// against a compromised agent reporting foreign resource ids).
		if _, err := tx.Exec(ctx,
			`UPDATE resources SET status = $3, updated_at = now() WHERE id = $1 AND server_id = $2`,
			resourceID, serverID, st); err != nil {
			return false, fmt.Errorf("update resource status: %w", err)
		}
	}
	return true, tx.Commit(ctx)
}
