package store

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
		`SELECT COALESCE(server_id,''), name FROM resources WHERE org_id = $1 AND id = $2`,
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
	var failedOps []string
	err = tx.QueryRow(ctx, `
		INSERT INTO server_dsd (server_id, org_id, version, doc_hash)
		VALUES ($1, $2, 0, '')
		ON CONFLICT (server_id) DO UPDATE SET server_id = server_dsd.server_id
		RETURNING version, doc_hash, applied_version, apply_ok, redrive_count, apply_failed_ops`,
		serverID, orgID).Scan(&curVersion, &curHash, &appliedVersion, &applyOK, &redriveCount, &failedOps)
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
		// …and tell somebody (SIGMA-247). Going quiet is the right thing to do to
		// the version counter and the wrong thing to do to the operator: from here
		// on the server heartbeats normally, its status stays 'running', and the
		// ops in this document simply never apply. This is the last moment the CP
		// knows that, so it is where the notice has to be raised.
		//
		// The dedup key carries the stuck VERSION, with an at-most-once-ever
		// window. At the cap the version is frozen (nothing more is issued), so
		// this fires exactly once per stuck document rather than once per 60s
		// resync forever; a real config change resets the budget and any later
		// stall is a different version, hence a new alert.
		if err := s.alertApplyStuckTx(ctx, tx, orgID, serverID, curVersion, failedOps); err != nil {
			return dsd.Signed{}, false, err
		}
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
		   SET version = $2, doc = $3, signature = $4, doc_hash = $5, apply_ok = true, redrive_count = $6,
		       apply_failed_ops = '{}', updated_at = now()
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

// alertApplyStuckTx raises the SIGMA-247 "this server has stopped converging"
// notice inside the reconcile transaction, so it can never be lost between the
// decision to stop re-driving and a separate notify step.
//
// The body names the failing op ids because they are the only actionable part:
// "srv-3 is not converging" sends an operator hunting, "host:nftables:srv_3
// failed" sends them to the firewall. The server NAME is looked up here rather
// than threaded down from the reconciler — this path runs at most once per
// stuck document, so one extra read costs nothing, and a body that says
// "web-1" beats one that says "srv_9f3c1a2b".
func (s *Store) alertApplyStuckTx(ctx context.Context, tx pgx.Tx, orgID, serverID string, version int64, failedOps []string) error {
	var name string
	if err := tx.QueryRow(ctx, `SELECT name FROM servers WHERE id = $1`, serverID).Scan(&name); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		name = serverID
	}
	detail := "no op ids were reported"
	if len(failedOps) > 0 {
		detail = strings.Join(failedOps, ", ")
	}
	body := fmt.Sprintf(
		"%s has failed to apply desired-state version %d %d times; the control plane has stopped retrying it. "+
			"The server is still reachable, so its status will keep reading healthy — but the ops below have not been applied "+
			"and will not be retried until its configuration changes.\n\nFailing ops: %s",
		name, version, maxDSDRedrive, detail)
	return enqueueAlertTx(ctx, tx, orgID, AlertDSDApplyFailed,
		fmt.Sprintf("dsdfail:%s:%d", serverID, version), 0,
		"Server stopped converging: "+name, body)
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
	// ClusterID is set when the resource deploys INTO a cluster, i.e. when it
	// belongs to no server. A node still reads such a resource — see
	// ResourceHostedHere, and the secrets that have to reach it — but it must
	// never be rendered as one of that node's own containers: Kubernetes runs it,
	// on a host the scheduler picks. Without this the same resource rendered a
	// k8s.apply on the control plane AND a container.apply under the SAME op id
	// on every node of the cluster, so each node would additionally have run the
	// workload itself, outside the scheduler that is supposed to own it.
	ClusterID string
	// PublicHost is the hostname SigmaHub routes to this resource without the
	// customer configuring any DNS (SIGMA-351), or "" when this deployment can
	// offer none — no CP_APPS_DOMAIN and a host with no reachable public address.
	// Resolved here rather than stored, because the suffix depends on deployment
	// config and on the host's current address; only the label is durable.
	PublicHost string
}

// ResourceHostedHere decides whether a resource has anything to do with a given
// server. Three ways it can:
//
//   - the server owns it, the common case;
//   - it deploys into a cluster and this server is one of that cluster's nodes,
//     so it belongs to no server at all;
//   - it hosts one of the resource's Compose services under per-service placement.
//
// EVERY read a server makes about a resource has to agree on this — the spec,
// its secrets, its domains, and the endpoints the agent later calls to resolve
// those secrets and report a certificate. They are read separately and combined,
// so a disagreement is not a compile error; it is a service that renders and
// then cannot start, or a workload the API server accepts with an empty Secret.
// Keeping the rule in one place is what stops the next reader from drifting.
//
// serverParam names the placeholder holding the server id, because the callers
// do not agree on argument order. The query must expose the resource as `r`.
func ResourceHostedHere(serverParam string) string {
	p := serverParam
	// The Compose-placement arm reads the indexed resource_service_placements
	// table, NOT r.spec's jsonb (SIGMA-365). The jsonb form
	// (jsonb_array_elements(spec->'compose'->'services') WHERE serverId = $)
	// cannot be index-driven and is un-restrictable on `resources` alone, so it
	// forced a cross-tenant seq scan of the whole resources table — detoasting and
	// parsing every non-owning row's spec — on every reconcile of every server.
	// resource_service_placements is the faithful projection of exactly those
	// placements (maintained by syncServicePlacementsTx / SetComposePlacements,
	// held true by TestComposePlacementsStayProjected), which is why
	// DeployTargetsForServerQuery already reads it. Same rule, an index behind it.
	return `
		(r.server_id = ` + p + `
		 OR (r.cluster_id IS NOT NULL AND EXISTS (
		       SELECT 1 FROM cluster_nodes n
		        WHERE n.cluster_id = r.cluster_id AND n.server_id = ` + p + `))
		 OR EXISTS (
		       SELECT 1 FROM resource_service_placements rsp
		        WHERE rsp.resource_id = r.id AND rsp.server_id = ` + p + `))`
}

func (s *Store) ResourceSpecsForServer(ctx context.Context, serverID string) ([]ResourceSpec, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT r.id, r.project_id, r.kind, r.spec, r.ephemeral, COALESCE(r.cluster_id, ''),
		       COALESCE(r.public_label, ''),
		       COALESCE((SELECT sv.endpoint FROM servers sv WHERE sv.id = $1), '')
		  FROM resources r
		 WHERE`+ResourceHostedHere("$1")+`
		 ORDER BY r.created_at`,
		serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceSpec{}
	for rows.Next() {
		var r ResourceSpec
		var label, endpoint string
		if err := rows.Scan(&r.ResourceID, &r.ProjectID, &r.Kind, &r.Spec, &r.Ephemeral, &r.ClusterID,
			&label, &endpoint); err != nil {
			return nil, err
		}
		r.PublicHost = PublicHost(label, s.appsDomain, endpoint)
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
//
// failedOps are the ids of the ops that did NOT apply (SIGMA-247). `converged`
// alone is enough to drive the re-drive but tells an operator nothing about
// WHAT is broken, so the ids are kept next to the flag, surfaced on the server
// read model and named in the stuck-apply alert. Empty on a converged report,
// which clears whatever was failing before.
func (s *Store) ApplyDSDStatus(ctx context.Context, serverID string, version int64, opStatus map[string]json.RawMessage, converged bool, failedOps []string) (bool, error) {
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
	// Normalized to a non-nil slice so a converged report writes '{}' rather than
	// SQL NULL into a NOT NULL column, and sorted because the caller collects them
	// from a map — without this the same failure reads as a different set on every
	// report and the dashboard flickers through permutations of one list.
	if failedOps == nil {
		failedOps = []string{}
	}
	sort.Strings(failedOps)
	if _, err := tx.Exec(ctx,
		`UPDATE server_dsd SET applied_version = $2, apply_ok = $3, apply_failed_ops = $4 WHERE server_id = $1`,
		serverID, version, converged, failedOps); err != nil {
		return false, err
	}
	for resourceID, st := range opStatus {
		// Only touch resources this server actually hosts (defence in depth
		// against a compromised agent reporting foreign resource ids).
		//
		// "Hosts" has to be the same rule the document was rendered from. A bare
		// server_id matched neither of the two kinds that belong to no single
		// server — a cluster workload, and a Compose service placed on another
		// machine — so their reports updated nothing and the dashboard showed
		// them provisioning forever while they were running fine.
		if _, err := tx.Exec(ctx,
			`UPDATE resources r SET status = $3, updated_at = now()
			  WHERE r.id = $1 AND`+ResourceHostedHere("$2"),
			resourceID, serverID, st); err != nil {
			return false, fmt.Errorf("update resource status: %w", err)
		}
	}
	return true, tx.Commit(ctx)
}
