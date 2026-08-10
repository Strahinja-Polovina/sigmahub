package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Deployment is one immutable release-history row: the git SHA + built image
// digest + config hash that were rolled out, plus the outcome and duration.
type Deployment struct {
	ID              string     `json:"id"`
	OrgID           string     `json:"orgId"`
	ResourceID      string     `json:"resourceId"`
	EnvironmentID   string     `json:"environmentId,omitempty"`
	ServerID        string     `json:"serverId,omitempty"`
	ConnectionID    string     `json:"connectionId,omitempty"`
	Trigger         string     `json:"trigger"` // git | manual | rollback
	GitRef          string     `json:"gitRef,omitempty"`
	GitSHA          string     `json:"gitSha,omitempty"`
	ImageDigest     string     `json:"imageDigest,omitempty"`
	ConfigHash      string     `json:"configHash,omitempty"`
	Status          string     `json:"status"`
	Detail          string     `json:"detail,omitempty"`
	RollbackOf      string     `json:"rollbackOf,omitempty"`
	BuildSeconds    *int       `json:"buildSeconds,omitempty"`
	DurationSeconds *int       `json:"durationSeconds,omitempty"`
	CreatedBy       string     `json:"createdBy,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	// ServiceCount + ServiceStatus describe a Compose multi-service deploy (0/empty
	// for a single-container app): how many services the deploy spans and each
	// service's current state (deploying|success|failed).
	ServiceCount  int               `json:"serviceCount,omitempty"`
	ServiceStatus map[string]string `json:"serviceStatus,omitempty"`
}

// Build tracks a dedup-keyed image build so a retry of the same inputs reuses the
// already-built image instead of rebuilding.
type Build struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"orgId"`
	ResourceID  string    `json:"resourceId"`
	ServerID    string    `json:"serverId,omitempty"`
	DedupKey    string    `json:"dedupKey"`
	GitSHA      string    `json:"gitSha,omitempty"`
	ImageRef    string    `json:"imageRef,omitempty"`
	ImageDigest string    `json:"imageDigest,omitempty"`
	Status      string    `json:"status"` // pending|building|built|failed
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// terminalDeployStatus is true for statuses that freeze the row (the immutable
// history invariant): a terminal deployment is never transitioned again.
func terminalDeployStatus(s string) bool {
	switch s {
	case "success", "failed", "superseded", "rolled_back":
		return true
	}
	return false
}

// deployStatusRank orders the non-terminal → success progression so a status
// advance is monotonic: a single agent status POST reports clone+build+rollout in
// arbitrary map order, and only forward moves may apply (no regression).
func deployStatusRank(s string) int {
	switch s {
	case "queued":
		return 0
	case "building":
		return 1
	case "deploying":
		return 2
	case "success":
		return 3
	}
	return -1
}

// deployInFlight reports whether a deployment is still on its way. It is the
// same set the SQL in this file spells as IN ('queued','building','deploying'),
// named once so a caller in Go and a caller in SQL cannot drift apart.
func deployInFlight(status string) bool {
	switch status {
	case "queued", "building", "deploying":
		return true
	}
	return false
}

// supersedeInFlightTx freezes any still-in-flight deployment for a resource as
// 'superseded' — called in the same tx that creates a newer deployment, so there
// is at most ONE in-flight deployment per resource. That single-in-flight
// invariant is what lets the op-status path advance "the in-flight deployment"
// unambiguously, and it makes the promised "superseded when a newer deploy wins
// the race" state actually hold (no lingering orphan rows).
//
// A cluster-deployed resource has no server of its own: its rows are inserted
// with server_id NULL. Returning early on an empty server id therefore meant the
// invariant simply did not hold for cluster workloads (SIGMA-232) — two pushes a
// few minutes apart left the FIRST deployment in flight forever while every
// subsequent op status advanced the second, and 45 minutes later
// TimeoutStaleDeployments failed the abandoned row, which enqueues a
// deploy_failed alert. The operator got paged, and a red entry in the deploy
// feed, for a deploy that was correctly replaced and whose successor shipped
// fine. Every rapid push pair on every cluster app produced one, which is the
// fastest way to teach a team to ignore deploy alerts.
//
// So the key is (org, resource) with the server-bound and serverless cases kept
// apart: a server-bound deploy still supersedes only rows on ITS server (two
// hosts running the same resource are two independent pipelines), while a
// serverless deploy supersedes the resource's serverless rows.
func supersedeInFlightTx(ctx context.Context, tx pgx.Tx, orgID, serverID, resourceID string) error {
	if resourceID == "" {
		return nil
	}
	// Serialize concurrent creators of a new deployment for this key. Without
	// this, a git-drain and a manual redeploy can each run their supersede
	// BEFORE the other's insert is visible, both pass, and two in-flight
	// deployments survive — breaking the at-most-one-in-flight invariant the
	// op-status path relies on (SIGMA-131). The lock is transaction-scoped,
	// released only after the caller's INSERT commits, so the next creator
	// blocks until it can see the freshly-queued row. A serverless (cluster)
	// deploy locks on the resource alone, which is its whole identity.
	lockKey := "deploy:" + resourceID
	if serverID != "" {
		lockKey = "deploy:" + serverID + ":" + resourceID
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE deployments SET
			status = 'superseded',
			finished_at = now(),
			duration_seconds = GREATEST(0, EXTRACT(EPOCH FROM (now() - COALESCE(started_at, created_at)))::int)
		 WHERE org_id = $1 AND resource_id = $3
		   AND (($2 = '' AND server_id IS NULL) OR server_id = NULLIF($2,''))
		   AND status IN ('queued','building','deploying')`,
		orgID, serverID, resourceID)
	return err
}

// TimeoutStaleDeployments fails deployments that have been in flight past
// `timeout` without reaching a terminal state, returning how many were failed.
//
// Nothing else ever transitions them: the only exits are an agent op-status
// report and 'superseded' (when a NEWER deployment is created for the same
// server+resource). So an agent that dies, loses power, or is disconnected
// mid-deploy left the row "building" forever — no deploy_failed alert (that
// only fires on a transition TO failed), a duration stuck on "…", and a log
// pane that streams indefinitely because `done` is derived from the status
// (SIGMA-182). Backup runs have had this net since P1-11.
//
// Each row goes through setDeploymentStatusTx so it gets the same terminal
// stamping and the same deploy_failed alert as any other failure — that
// function's own comment already names "timeout" as one of its callers.
func (s *Store) TimeoutStaleDeployments(ctx context.Context, timeout time.Duration) (int64, error) {
	if timeout <= 0 {
		return 0, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Measure from the last sign of life: started_at once the agent reports its
	// first phase, else the row's creation.
	rows, err := tx.Query(ctx, `
		SELECT id FROM deployments
		 WHERE status IN ('queued','building','deploying')
		   AND COALESCE(started_at, created_at) < now() - $1::interval
		 ORDER BY created_at`, timeout.String())
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var n int64
	for _, id := range ids {
		err := setDeploymentStatusTx(ctx, tx, id, DeploymentStatusUpdate{
			Status:       "failed",
			Detail:       "timed out — the agent stopped reporting progress",
			MarkFinished: true,
		})
		if errors.Is(err, ErrConflict) {
			continue // raced to terminal in the meantime
		}
		if err != nil {
			return 0, err
		}
		n++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

// StampDeploymentDSDVersion records the DSD version that first rendered each of
// the given in-flight deployments (SIGMA-134). It stamps only rows still in
// flight and not yet stamped (dsd_version = 0), so a deployment keeps the
// version it FIRST appeared at even as the DSD re-renders for unrelated changes —
// which is exactly what lets a late op-status report be recognized as stale.
func (s *Store) StampDeploymentDSDVersion(ctx context.Context, deploymentIDs []string, version int64) error {
	if len(deploymentIDs) == 0 || version <= 0 {
		return nil
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE deployments SET dsd_version = $2
		 WHERE id = ANY($1) AND dsd_version = 0
		   AND status IN ('queued','building','deploying')`, deploymentIDs, version)
	return err
}

// deployImageTag mirrors dsd.DeployImageTag — the deterministic image reference a
// git-deployed resource's build produces and its rollout runs. Kept inline (not
// an import) to avoid a store→dsd dependency; the two MUST stay in sync.
func deployImageTag(resourceID, sha string) string {
	return "sigmahub/" + resourceID + ":" + sha
}

// deployPin mirrors dsd.DeployPin (same no-import rule as deployImageTag): the
// short unique suffix under which a deployment's images are tagged, so no tag
// is ever rebuilt in place and a rollback can re-ship a release's exact bytes
// (SIGMA-173). Rollback/config rows COPY their source release's pin instead of
// deriving their own — they ship existing images, they don't build.
func deployPin(deploymentID string) string {
	pin := deploymentID
	if i := strings.LastIndex(pin, "_"); i >= 0 {
		pin = pin[i+1:]
	}
	if len(pin) > 6 {
		pin = pin[len(pin)-6:]
	}
	return pin
}

// pinnedImageTag mirrors dsd.PinnedImageTag.
func pinnedImageTag(resourceID, sha, pin string) string {
	if pin == "" {
		return deployImageTag(resourceID, sha)
	}
	return deployImageTag(resourceID, sha) + "-" + pin
}

// CreateDeploymentInput queues a new deploy.
type CreateDeploymentInput struct {
	ResourceID    string
	EnvironmentID string
	ServerID      string
	ConnectionID  string
	Trigger       string // git | manual | rollback
	GitRef        string
	GitSHA        string
	ConfigHash    string
	RollbackOf    string
	ImageDigest   string // set immediately for a rollback (reuses a built image)
}

// CreateDeployment appends a queued deployment row (org-scoped via the resource).
// Audited.
func (s *Store) CreateDeployment(ctx context.Context, orgID string, in CreateDeploymentInput, actor string) (Deployment, error) {
	trigger := in.Trigger
	if trigger == "" {
		trigger = "manual"
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Deployment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The resource must belong to the org; capture its server if not supplied,
	// and how many Compose services have to succeed before the deploy is done.
	var serverID string
	var resourceSpec []byte
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(server_id,''), spec FROM resources WHERE org_id = $1 AND id = $2`,
		orgID, in.ResourceID).Scan(&serverID, &resourceSpec)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	if in.ServerID != "" {
		serverID = in.ServerID
	}
	// Without this a Compose deploy can never finish: completion is "every
	// declared service succeeded", and with a denominator of zero the check
	// `success >= service_count` is gated behind `service_count > 0` and never
	// fires. The webhook path computed it; this one — manual deploys, redeploys
	// and rollbacks — did not, so those ran perfectly and then sat in
	// 'deploying' forever, which also keeps the release out of the rollback
	// targets that require a success.
	svcCount := composeServiceCount(resourceSpec)

	d := Deployment{
		ID: newID("dep"), OrgID: orgID, ResourceID: in.ResourceID, EnvironmentID: in.EnvironmentID,
		ServerID: serverID, ConnectionID: in.ConnectionID, Trigger: trigger, GitRef: in.GitRef,
		GitSHA: in.GitSHA, ConfigHash: in.ConfigHash, ImageDigest: in.ImageDigest,
		RollbackOf: in.RollbackOf, Status: "queued", CreatedBy: actor,
		ServiceCount: svcCount,
	}
	// A building trigger gets its own pin (its images are tagged under it); a
	// rollback ships an existing image and derives nothing (SIGMA-173).
	pin := deployPin(d.ID)
	if trigger == "rollback" {
		pin = ""
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id, trigger,
		                         git_ref, git_sha, image_digest, image_pin, config_hash, rollback_of, status, created_by,
		                         service_count)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,$8,$9,NULLIF($10,''),$11,$12,NULLIF($13,''),'queued',$14,$15)
		RETURNING created_at`,
		d.ID, orgID, in.ResourceID, in.EnvironmentID, serverID, in.ConnectionID, trigger,
		in.GitRef, in.GitSHA, in.ImageDigest, pin, in.ConfigHash, in.RollbackOf, actor, svcCount).Scan(&d.CreatedAt)
	if err != nil {
		return Deployment{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Deploy queued ("+trigger+")", in.ResourceID); err != nil {
		return Deployment{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, err
	}
	return d, nil
}

// DeploymentStatusUpdate carries an outcome transition. Fields left nil/empty are
// unchanged; timings are stamped by the DB.
type DeploymentStatusUpdate struct {
	Status       string
	Detail       string
	ImageDigest  string
	BuildSeconds *int
	MarkStarted  bool
	MarkFinished bool
}

// SetDeploymentStatus transitions a deployment. A terminal row is frozen (the
// immutable-history invariant): transitioning it is a no-op that returns
// ErrConflict, so a late/duplicate report can't rewrite a finished release.
func (s *Store) SetDeploymentStatus(ctx context.Context, deploymentID string, up DeploymentStatusUpdate) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setDeploymentStatusTx(ctx, tx, deploymentID, up); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// setDeploymentStatusTx applies a status transition within an existing tx (a
// terminal row returns ErrConflict, unchanged). Callers already inside a tx —
// AdvanceDeploymentService — reuse it so the per-service and overall updates
// commit atomically.
func setDeploymentStatusTx(ctx context.Context, tx pgx.Tx, deploymentID string, up DeploymentStatusUpdate) error {
	var cur string
	var startedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT status, started_at FROM deployments WHERE id = $1 FOR UPDATE`, deploymentID).Scan(&cur, &startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if terminalDeployStatus(cur) {
		return ErrConflict
	}
	_, err = tx.Exec(ctx, `
		UPDATE deployments SET
			status = $2,
			detail = CASE WHEN $3 = '' THEN detail ELSE $3 END,
			image_digest = COALESCE(NULLIF($4,''), image_digest),
			build_seconds = COALESCE($5, build_seconds),
			started_at = CASE WHEN $6 AND started_at IS NULL THEN now() ELSE started_at END,
			finished_at = CASE WHEN $7 THEN now() ELSE finished_at END,
			duration_seconds = CASE WHEN $7 THEN
				GREATEST(0, EXTRACT(EPOCH FROM (now() - COALESCE(started_at, created_at)))::int)
				ELSE duration_seconds END
		WHERE id = $1`,
		deploymentID, up.Status, up.Detail, up.ImageDigest, up.BuildSeconds, up.MarkStarted, up.MarkFinished)
	if err != nil {
		return err
	}
	// P2-6: a deployment reaching failed alerts once (dedup key = deployment
	// id, no window). This is the single choke point every failure path —
	// single-container, per-service compose, timeout — funnels through.
	if up.Status == "failed" {
		var orgID, resName, sha string
		if err := tx.QueryRow(ctx, `
			SELECT d.org_id, r.name, COALESCE(d.git_sha, '')
			  FROM deployments d JOIN resources r ON r.id = d.resource_id
			 WHERE d.id = $1`, deploymentID).Scan(&orgID, &resName, &sha); err != nil {
			return err
		}
		detail := up.Detail
		if detail == "" {
			detail = "no failure detail reported"
		}
		if len(sha) > 7 {
			sha = sha[:7]
		}
		title := "Deploy of " + resName + " failed"
		if sha != "" {
			title += " (" + sha + ")"
		}
		if err := enqueueAlertTx(ctx, tx, orgID, AlertDeployFailed,
			"dep:"+deploymentID, 0, title, detail); err != nil {
			return err
		}
	}
	return nil
}

// ListDeployments returns a resource's release history newest-first (org-scoped).
func (s *Store) ListDeployments(ctx context.Context, orgID, resourceID string, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, resource_id, environment_id, server_id, connection_id, trigger, git_ref, git_sha,
		       image_digest, config_hash, status, detail, rollback_of, build_seconds, duration_seconds,
		       created_by, created_at, started_at, finished_at, service_count, service_status
		  FROM deployments WHERE org_id = $1 AND resource_id = $2
		 ORDER BY created_at DESC LIMIT $3`, orgID, resourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeployments(rows)
}

// OrgDeployments is the dashboard's org-wide deploy feed: the most recent
// deployments (the activity stream) plus the latest deployment per resource —
// however old — so "Last deploy" / "Version" / "Active deploys" don't depend
// on a recency window. Before this endpoint the web's CP mode had no org-wide
// deploy source at all and rendered a permanently empty feed with columns
// frozen at resource-creation time (SIGMA-161).
type OrgDeployments struct {
	Recent []Deployment `json:"recent"`
	Latest []Deployment `json:"latest"`
}

const deploymentCols = `id, org_id, resource_id, environment_id, server_id, connection_id, trigger, git_ref, git_sha,
	       image_digest, config_hash, status, detail, rollback_of, build_seconds, duration_seconds,
	       created_by, created_at, started_at, finished_at, service_count, service_status`

// ListOrgDeployments returns the org's deploy feed (see OrgDeployments).
func (s *Store) ListOrgDeployments(ctx context.Context, orgID string, recentLimit int) (OrgDeployments, error) {
	if recentLimit <= 0 || recentLimit > 100 {
		recentLimit = 50
	}
	var out OrgDeployments
	rows, err := s.Pool.Query(ctx, `
		SELECT `+deploymentCols+` FROM deployments
		 WHERE org_id = $1 ORDER BY created_at DESC LIMIT $2`, orgID, recentLimit)
	if err != nil {
		return OrgDeployments{}, err
	}
	out.Recent, err = scanDeployments(rows)
	rows.Close()
	if err != nil {
		return OrgDeployments{}, err
	}
	rows, err = s.Pool.Query(ctx, `
		SELECT DISTINCT ON (resource_id) `+deploymentCols+` FROM deployments
		 WHERE org_id = $1 ORDER BY resource_id, created_at DESC`, orgID)
	if err != nil {
		return OrgDeployments{}, err
	}
	out.Latest, err = scanDeployments(rows)
	rows.Close()
	if err != nil {
		return OrgDeployments{}, err
	}
	return out, nil
}

// RollbackTargets returns a resource's last N SUCCESSFUL releases whose exact
// images can be re-shipped, newest-first — the candidates a &lt;30s rebuild-free
// rollback can pick from. Eligible: pinned releases (image_pin — per-deployment
// immutable tags, SIGMA-173), or legacy single-container releases whose
// recorded tag was really built. Legacy Compose releases are excluded — their
// image_digest was a tag no Compose build ever produced (SIGMA-168).
func (s *Store) RollbackTargets(ctx context.Context, orgID, resourceID string, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 10 {
		limit = 10
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, resource_id, environment_id, server_id, connection_id, trigger, git_ref, git_sha,
		       image_digest, config_hash, status, detail, rollback_of, build_seconds, duration_seconds,
		       created_by, created_at, started_at, finished_at, service_count, service_status
		  FROM deployments
		 WHERE org_id = $1 AND resource_id = $2 AND status = 'success'
		   AND (COALESCE(image_pin,'') <> ''
		        OR (image_digest IS NOT NULL AND COALESCE(service_count,0) = 0))
		 ORDER BY created_at DESC LIMIT $3`, orgID, resourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeployments(rows)
}

func scanDeployments(rows pgx.Rows) ([]Deployment, error) {
	out := []Deployment{}
	for rows.Next() {
		var d Deployment
		var env, srv, conn, ref, sha, digest, cfg, rollback *string
		var svcStatus []byte
		if err := rows.Scan(&d.ID, &d.OrgID, &d.ResourceID, &env, &srv, &conn, &d.Trigger, &ref, &sha,
			&digest, &cfg, &d.Status, &d.Detail, &rollback, &d.BuildSeconds, &d.DurationSeconds,
			&d.CreatedBy, &d.CreatedAt, &d.StartedAt, &d.FinishedAt, &d.ServiceCount, &svcStatus); err != nil {
			return nil, err
		}
		d.EnvironmentID = deref(env)
		d.ServerID = deref(srv)
		d.ConnectionID = deref(conn)
		d.GitRef = deref(ref)
		d.GitSHA = deref(sha)
		d.ImageDigest = deref(digest)
		d.ConfigHash = deref(cfg)
		d.RollbackOf = deref(rollback)
		if len(svcStatus) > 0 {
			m := map[string]string{}
			if json.Unmarshal(svcStatus, &m) == nil && len(m) > 0 {
				d.ServiceStatus = m
			}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GetDeployment returns one deployment scoped to the org (BOLA guard for the
// log-stream endpoint: a caller can only read logs for a deployment in its org).
func (s *Store) GetDeployment(ctx context.Context, orgID, deploymentID string) (Deployment, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, resource_id, environment_id, server_id, connection_id, trigger, git_ref, git_sha,
		       image_digest, config_hash, status, detail, rollback_of, build_seconds, duration_seconds,
		       created_by, created_at, started_at, finished_at, service_count, service_status
		  FROM deployments WHERE org_id = $1 AND id = $2`, orgID, deploymentID)
	if err != nil {
		return Deployment{}, err
	}
	defer rows.Close()
	ds, err := scanDeployments(rows)
	if err != nil {
		return Deployment{}, err
	}
	if len(ds) == 0 {
		return Deployment{}, ErrNotFound
	}
	return ds[0], nil
}

// CreateRollback queues a rebuild-free rollback: it validates the target is a
// SUCCESSFUL release of the same resource with a retained image, then appends a
// new deployment that reuses that image digest (trigger=rollback, rollback_of set)
// so the reconciler renders only the rollout op — no clone/build. Returns the new
// deployment and the server to re-render. Audited.
func (s *Store) CreateRollback(ctx context.Context, orgID, resourceID, targetDeploymentID, actor string) (Deployment, string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Deployment{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var t Deployment
	var env, srv, conn, ref, sha, digest, cfg *string
	var srcPin, srcBuild string
	var srcSvcCount int
	err = tx.QueryRow(ctx, `
		SELECT environment_id, server_id, connection_id, git_ref, git_sha, image_digest,
		       COALESCE(image_pin,''), COALESCE(service_count,0), config_hash, status,
		       COALESCE(build_server_id,'')
		  FROM deployments WHERE org_id = $1 AND resource_id = $2 AND id = $3`,
		orgID, resourceID, targetDeploymentID).Scan(&env, &srv, &conn, &ref, &sha, &digest, &srcPin, &srcSvcCount, &cfg, &t.Status, &srcBuild)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, "", ErrNotFound
	}
	if err != nil {
		return Deployment{}, "", err
	}
	// Eligible: a pinned release (its exact images are re-derivable), or a legacy
	// single-container release whose recorded tag was at least really built. A
	// legacy Compose release is NEITHER — its image_digest is the single-container
	// tag no Compose build ever produced (SIGMA-168) — so it never qualifies.
	pinned := srcPin != ""
	legacySingle := digest != nil && *digest != "" && srcSvcCount == 0
	if t.Status != "success" || (!pinned && !legacySingle) {
		return Deployment{}, "", ErrInvalid{Msg: "rollback target must be a successful release with a retained image"}
	}
	// The per-service denominator comes from the CURRENT resource spec (what the
	// reconciler will render), not the prior deployment's stale copy.
	svcCount, err := resourceServiceCountTx(ctx, tx, orgID, resourceID)
	if err != nil {
		return Deployment{}, "", err
	}

	d := Deployment{
		ID: newID("dep"), OrgID: orgID, ResourceID: resourceID, EnvironmentID: deref(env),
		ServerID: deref(srv), ConnectionID: deref(conn), Trigger: "rollback", GitRef: deref(ref),
		GitSHA: deref(sha), ImageDigest: deref(digest), ConfigHash: deref(cfg),
		RollbackOf: targetDeploymentID, Status: "queued", CreatedBy: actor,
	}
	// The rollback supersedes any in-flight deploy for this resource.
	if err := supersedeInFlightTx(ctx, tx, orgID, d.ServerID, resourceID); err != nil {
		return Deployment{}, "", err
	}
	// Where this resource builds travels with the row (SIGMA-231). A rollback
	// re-ships a retained image and renders no build of its own, but it becomes
	// the resource's newest deployment — and the next redeploy copies from the
	// newest deployment, so dropping the column here silently loses the build
	// server for every deploy after it.
	buildServer, err := resolveBuildServerTx(ctx, tx, srcBuild, d.ConnectionID, d.EnvironmentID)
	if err != nil {
		return Deployment{}, "", err
	}
	// COPY the source's pin — a rollback of a rollback keeps pointing at the
	// original build's images, however long the chain (SIGMA-173).
	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id, trigger,
		                         git_ref, git_sha, image_digest, image_pin, config_hash, service_count, rollback_of, status, created_by,
		                         build_server_id)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),'rollback',NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10,NULLIF($11,''),$12,$13,'queued',$14,NULLIF($15,''))
		RETURNING created_at`,
		d.ID, orgID, resourceID, d.EnvironmentID, d.ServerID, d.ConnectionID,
		d.GitRef, d.GitSHA, d.ImageDigest, srcPin, d.ConfigHash, svcCount, targetDeploymentID, actor,
		buildServer).Scan(&d.CreatedAt)
	if err != nil {
		return Deployment{}, "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Rollback queued to "+targetDeploymentID, resourceID); err != nil {
		return Deployment{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, "", err
	}
	return d, d.ServerID, nil
}

// CreateManualRedeploy queues a manual redeploy of a git-deployed resource: it
// copies the git coordinates (connection/ref/sha/config) of the resource's most
// recent deployment into a new deployment (trigger=manual) with NO image digest,
// so the reconciler renders a fresh clone→build→rollout — a rebuild of the same
// commit that picks up base-image changes. Errors when the resource has never
// been deployed (nothing to redeploy). Returns the server to re-render. Audited.
func (s *Store) CreateManualRedeploy(ctx context.Context, orgID, resourceID, actor string) (Deployment, string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Deployment{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var env, srv, conn, ref, sha, cfg *string
	var srcBuild string
	err = tx.QueryRow(ctx, `
		SELECT environment_id, server_id, connection_id, git_ref, git_sha, config_hash,
		       COALESCE(build_server_id,'')
		  FROM deployments WHERE org_id = $1 AND resource_id = $2
		 ORDER BY created_at DESC LIMIT 1`, orgID, resourceID).Scan(&env, &srv, &conn, &ref, &sha, &cfg, &srcBuild)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, "", ErrInvalid{Msg: "nothing to redeploy — connect a repo and push first"}
	}
	if err != nil {
		return Deployment{}, "", err
	}
	// Fresh denominator from the current resource spec (see CreateRollback).
	svcCount, err := resourceServiceCountTx(ctx, tx, orgID, resourceID)
	if err != nil {
		return Deployment{}, "", err
	}

	d := Deployment{
		ID: newID("dep"), OrgID: orgID, ResourceID: resourceID, EnvironmentID: deref(env),
		ServerID: deref(srv), ConnectionID: deref(conn), Trigger: "manual", GitRef: deref(ref),
		GitSHA: deref(sha), ConfigHash: deref(cfg), Status: "queued", CreatedBy: actor,
	}
	// The manual redeploy supersedes any in-flight deploy for this resource.
	if err := supersedeInFlightTx(ctx, tx, orgID, d.ServerID, resourceID); err != nil {
		return Deployment{}, "", err
	}
	// A redeploy is a REBUILD, so where it builds is not optional (SIGMA-231).
	// For a cluster app this column is the only thing that renders its clone and
	// build ops anywhere, so a redeploy that drops it produces a deployment no
	// machine can advance — queued with an empty build log until the stale-deploy
	// sweeper fails it 45 minutes later and blames the agent.
	buildServer, err := resolveBuildServerTx(ctx, tx, srcBuild, d.ConnectionID, d.EnvironmentID)
	if err != nil {
		return Deployment{}, "", err
	}
	// Its own pin: the forced rebuild lands on fresh per-deployment tags instead
	// of overwriting the prior release's (SIGMA-173 — the overwrite is what made
	// rollback silently re-ship the current image).
	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id, trigger,
		                         git_ref, git_sha, image_pin, config_hash, service_count, status, created_by,
		                         build_server_id)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),'manual',NULLIF($7,''),NULLIF($8,''),$9,NULLIF($10,''),$11,'queued',$12,NULLIF($13,''))
		RETURNING created_at`,
		d.ID, orgID, resourceID, d.EnvironmentID, d.ServerID, d.ConnectionID, d.GitRef, d.GitSHA, deployPin(d.ID), d.ConfigHash, svcCount, actor,
		buildServer).Scan(&d.CreatedAt)
	if err != nil {
		return Deployment{}, "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Manual redeploy queued", resourceID); err != nil {
		return Deployment{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Deployment{}, "", err
	}
	return d, d.ServerID, nil
}

// CreateConfigDeployments mints a queued 'config' deployment for each given app
// resource whose standing deploy target is a SUCCESSFUL release (SIGMA-166).
//
// Why it exists: the rollout generation is a function of (sha, deployment id),
// and after a success the same deployment lingers as the standing target — so a
// domain attach or secret change re-rendered a DIFFERENT container spec under
// the SAME generation name, which the agent's never-cut guard refuses. The app
// showed "Error" while still serving the old config, and an attached domain
// never routed. A config deployment gives the change its own deployment id —
// hence a fresh generation and a normal blue-green swap — while copying the
// source release's image pin so the render re-ships the running release's
// exact image with no clone and no build.
//
// Two states are skipped, both because something else will carry the change: a
// resource that has NEVER been deployed (the first deploy renders the new config
// anyway) and one with a deployment still IN FLIGHT (it resolves secret values
// at rollout time, and superseding it could discard a commit mid-build).
//
// Every other state mints a row. A successful latest deployment lends its pin,
// so the change re-ships the exact running image with no clone and no build. A
// SETTLED-BUT-UNSUCCESSFUL one — failed, superseded, rolled back — falls back to
// the last successful release, because that is what is actually serving: a
// rollout that fails its health gate keeps its predecessor up. Only when nothing
// has ever succeeded does the render rebuild, from the failed attempt's commit
// and never its image.
//
// That fallback is the whole point. This used to read
// `if status != "success" { continue }`, on the assumption that "the next real
// deploy renders the new config anyway" — and for a config-only fix there is no
// next real deploy. The user edited an env var or attached a domain, the
// dashboard reported success, and nothing happened, for good, unless somebody
// pushed a commit or found the Deploy button. It failed hardest exactly where it
// hurt most: the latest deployment is `failed` precisely when the user is trying
// to fix what broke it.
//
// Returns the distinct servers to re-render. Audited per resource.
func (s *Store) CreateConfigDeployments(ctx context.Context, orgID string, resourceIDs []string, actor, reason string) ([]ServerRef, error) {
	if len(resourceIDs) == 0 {
		return nil, nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	seen := map[string]ServerRef{}
	for _, resID := range resourceIDs {
		var env, srv, conn, ref, sha, digest, cfg *string
		var pin, status, srcBuild string
		err := tx.QueryRow(ctx, `
			SELECT environment_id, server_id, connection_id, git_ref, git_sha, image_digest,
			       COALESCE(image_pin,''), config_hash, status, COALESCE(build_server_id,'')
			  FROM deployments WHERE org_id = $1 AND resource_id = $2
			 ORDER BY created_at DESC LIMIT 1`, orgID, resID).
			Scan(&env, &srv, &conn, &ref, &sha, &digest, &pin, &cfg, &status, &srcBuild)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// Something is already on its way to this resource. Let it land: it will
		// resolve secret values at rollout time, and superseding it could throw
		// away a commit that is mid-build.
		if deployInFlight(status) {
			continue
		}
		svcCount, err := resourceServiceCountTx(ctx, tx, orgID, resID)
		if err != nil {
			return nil, err
		}
		depID := newID("dep")
		// The latest deployment is settled but not successful — failed, superseded
		// or rolled back. What is actually SERVING is the last successful release,
		// because a rollout that fails its health gate keeps its predecessor up. So
		// re-ship that one with the new config rather than skipping: this is the
		// case where the user is trying to fix what broke, and it is the one that
		// used to swallow the change whole.
		if status != "success" {
			var okRef, okSHA, okDigest *string
			var okPin string
			err := tx.QueryRow(ctx, `
				SELECT git_ref, git_sha, image_digest, COALESCE(image_pin,'')
				  FROM deployments
				 WHERE org_id = $1 AND resource_id = $2 AND status = 'success'
				 ORDER BY created_at DESC LIMIT 1`, orgID, resID).
				Scan(&okRef, &okSHA, &okDigest, &okPin)
			switch {
			case err == nil:
				ref, sha, digest, pin = okRef, okSHA, okDigest, okPin
			case errors.Is(err, pgx.ErrNoRows):
				// Nothing has ever served. Keep the failed attempt's commit but not
				// its image: that image never passed a health gate. The rebuild takes
				// its OWN pin (SIGMA-173) so it cannot overwrite another release's tag.
				digest, pin = nil, deployPin(depID)
			default:
				return nil, err
			}
		}
		// Where this resource BUILDS travels with the row (SIGMA-231). A pinned
		// config deploy re-ships and renders no build — but a pinless one falls
		// back to the full clone→build→rollout below, and for a cluster app that
		// pipeline exists only in the build server's document. Carrying it also
		// keeps the column alive for the next redeploy, which copies from this row.
		//
		// Resolved AFTER the fallback above, deliberately: when the latest attempt
		// failed and we re-ship the last successful release instead, the build
		// server that matters is still the one configured for this resource today,
		// not whichever host built a release months ago.
		buildServer, err := resolveBuildServerTx(ctx, tx, srcBuild, deref(conn), deref(env))
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id, trigger,
			                         git_ref, git_sha, image_digest, image_pin, config_hash, service_count, status, created_by,
			                         build_server_id)
			VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),'config',NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10,NULLIF($11,''),$12,'queued',$13,NULLIF($14,''))`,
			depID, orgID, resID, deref(env), deref(srv), deref(conn),
			deref(ref), deref(sha), deref(digest), pin, deref(cfg), svcCount, actor,
			buildServer); err != nil {
			return nil, err
		}
		if err := auditTx(ctx, tx, orgID, actor, "Config deploy queued ("+reason+")", resID); err != nil {
			return nil, err
		}
		if sid := deref(srv); sid != "" {
			seen[sid] = ServerRef{ServerID: sid, OrgID: orgID}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	out := make([]ServerRef, 0, len(seen))
	for _, sr := range seen {
		out = append(out, sr)
	}
	return out, nil
}

// LookupBuild returns a resource's build for a dedup key, if one exists — the
// retry short-circuit: a 'built' row means the image is already present.
func (s *Store) LookupBuild(ctx context.Context, resourceID, dedupKey string) (Build, error) {
	var b Build
	var srv, sha, ref, digest *string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, org_id, resource_id, server_id, dedup_key, git_sha, image_ref, image_digest, status, created_at, updated_at
		  FROM builds WHERE resource_id = $1 AND dedup_key = $2`, resourceID, dedupKey).Scan(
		&b.ID, &b.OrgID, &b.ResourceID, &srv, &b.DedupKey, &sha, &ref, &digest, &b.Status, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Build{}, ErrNotFound
	}
	if err != nil {
		return Build{}, err
	}
	b.ServerID, b.GitSHA, b.ImageRef, b.ImageDigest = deref(srv), deref(sha), deref(ref), deref(digest)
	return b, nil
}

// RecordBuildResult upserts a build's outcome (dedup-keyed). Used by the agent's
// build status report to persist the image digest a future deploy reuses.
func (s *Store) RecordBuildResult(ctx context.Context, orgID, resourceID, serverID, dedupKey, gitSHA, imageRef, imageDigest, status string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO builds (id, org_id, resource_id, server_id, dedup_key, git_sha, image_ref, image_digest, status)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9)
		ON CONFLICT (resource_id, dedup_key) DO UPDATE SET
			image_ref = COALESCE(NULLIF(EXCLUDED.image_ref,''), builds.image_ref),
			image_digest = COALESCE(NULLIF(EXCLUDED.image_digest,''), builds.image_digest),
			status = EXCLUDED.status,
			updated_at = now()`,
		newID("bld"), orgID, resourceID, serverID, dedupKey, gitSHA, imageRef, imageDigest, status)
	return err
}

// ServerRef identifies a server to reconcile after a mutation.
type ServerRef struct{ ServerID, OrgID string }

// DrainDeployRequests turns queued git deploy_requests (P1-7) into queued
// deployment rows: each request resolves to the app resources in its target
// environment, and one deployment is created per resource. Returns the distinct
// servers to re-render. Idempotent: a drained request is marked so a second pass
// skips it. Runs as a short background worker.
func (s *Store) DrainDeployRequests(ctx context.Context) ([]ServerRef, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The branch map carries the optional dedicated build server, so a
	// deployment records where its build should run at the moment it is created
	// (rather than re-reading a mapping that may change mid-flight).
	rows, err := tx.Query(ctx, `
		SELECT dr.id, dr.org_id, dr.connection_id, dr.environment_id, dr.ref, dr.sha, c.project_id,
		       COALESCE((SELECT bm.build_server_id FROM git_branch_map bm
		                  WHERE bm.connection_id = dr.connection_id
		                    AND bm.environment_id = dr.environment_id
		                  LIMIT 1), '')
		  FROM deploy_requests dr
		  JOIN git_connections c ON c.id = dr.connection_id
		 WHERE dr.kind = 'deploy' AND dr.status = 'queued'
		 ORDER BY dr.created_at
		 FOR UPDATE OF dr SKIP LOCKED`)
	if err != nil {
		return nil, err
	}
	type req struct {
		id, orgID, connID, envID, ref, sha, projectID string
		buildServerID                                 string
	}
	var reqs []req
	for rows.Next() {
		var r req
		var env *string
		if err := rows.Scan(&r.id, &r.orgID, &r.connID, &env, &r.ref, &r.sha, &r.projectID, &r.buildServerID); err != nil {
			rows.Close()
			return nil, err
		}
		r.envID = deref(env)
		reqs = append(reqs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seen := map[string]ServerRef{}
	for _, r := range reqs {
		// App resources in the request's target environment (of the connection's
		// project). Each gets a queued deployment.
		res, err := tx.Query(ctx, `
			SELECT id, COALESCE(server_id,''), spec FROM resources
			 WHERE org_id = $1 AND project_id = $2 AND environment_id = $3 AND kind = 'app'`,
			r.orgID, r.projectID, r.envID)
		if err != nil {
			return nil, err
		}
		type appRes struct {
			id, serverID string
			spec         []byte
		}
		var apps []appRes
		for res.Next() {
			var a appRes
			if err := res.Scan(&a.id, &a.serverID, &a.spec); err != nil {
				res.Close()
				return nil, err
			}
			apps = append(apps, a)
		}
		res.Close()
		if err := res.Err(); err != nil {
			return nil, err
		}

		for _, a := range apps {
			cfgHash := configHash(a.spec)
			svcCount := composeServiceCount(a.spec)
			// A newer push supersedes any still-in-flight deploy for this resource,
			// so only one deployment per (server,resource) is ever in flight.
			if err := supersedeInFlightTx(ctx, tx, r.orgID, a.serverID, a.id); err != nil {
				return nil, err
			}
			depID := newID("dep")
			if _, err := tx.Exec(ctx, `
				INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id,
				                         trigger, git_ref, git_sha, image_pin, config_hash, service_count, status, created_by,
				                         build_server_id)
				VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,'git',$7,$8,$9,$10,$11,'queued','github-webhook',NULLIF($12,''))`,
				depID, r.orgID, a.id, r.envID, a.serverID, r.connID, r.ref, r.sha, deployPin(depID), cfgHash, svcCount,
				r.buildServerID); err != nil {
				return nil, err
			}
			if a.serverID != "" {
				seen[a.serverID] = ServerRef{ServerID: a.serverID, OrgID: r.orgID}
			}
			// The build server needs a re-render too: the build op lands in ITS
			// document, not the deploy target's.
			if r.buildServerID != "" && r.buildServerID != a.serverID {
				seen[r.buildServerID] = ServerRef{ServerID: r.buildServerID, OrgID: r.orgID}
			}
		}
		// Record what the push actually did. Marking every request 'drained'
		// regardless made a push that resolved to NOTHING look exactly like one
		// that deployed: the webhook was accepted, the request was drained, and
		// no deploy ran. That is the normal state right after connecting a repo —
		// before any resource exists — and the product had no answer to "I
		// pushed, why is nothing happening?".
		status, detail := "drained", ""
		if len(apps) == 0 {
			status = "no_targets"
			detail = "no app resources in this environment yet — create one and push again, or use Redeploy"
			if err := auditTx(ctx, tx, r.orgID, "webhook",
				"Push matched no deployable resources", r.ref); err != nil {
				return nil, err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE deploy_requests
			   SET status = $2, deployments_created = $3, detail = $4
			 WHERE id = $1`, r.id, status, len(apps), detail); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	out := make([]ServerRef, 0, len(seen))
	for _, sr := range seen {
		out = append(out, sr)
	}
	return out, nil
}

// configHash is a stable fingerprint of a resource's spec, part of the build
// dedup key (config_hash + git_sha).
func configHash(spec []byte) string {
	sum := sha256.Sum256(spec)
	return hex.EncodeToString(sum[:8])
}

// DeployTarget is the release a git-deployed app resource should be running: the
// clone source + the deployment driving it. The reconciler renders the
// clone→build→rollout chain (or, for a rollback with a retained image, just the
// rollout) from it.
type DeployTarget struct {
	DeploymentID string
	ResourceID   string
	ProjectID    string
	ConnectionID string
	Provider     string
	RepoFullName string
	Ref          string
	SHA          string
	ConfigHash   string
	ImageDigest  string // set for a rollback (reuse a retained image; skip clone/build)
	// ImagePin is the release's build pin (dsd.DeployPin): builds tag under it,
	// rollback/config rows carry their SOURCE release's pin so the render
	// re-ships that release's exact images (SIGMA-173/168). Empty on legacy rows.
	ImagePin string
	Trigger  string
	Status   string // queued|building|deploying|success (the filtered set below)
	// CreatedAt orders a resource's deployments. The reconciler derives the
	// blue-green Traefik router priority from it (SIGMA-164), so it must come
	// from stored data rather than render-time wall-clock: a resync has to
	// re-render byte-identical labels.
	CreatedAt time.Time
	// ServiceStatus is the per-service state of a Compose deployment
	// (service → deploying|success|failed). It is what makes a cross-server
	// dependency enforceable: a service placed on another host can only be
	// rendered once the services it depends on report success, and those live
	// in a different server's document where op-level DependsOn cannot reach.
	ServiceStatus map[string]string
	// ServerID is the server the deployment was created against — the "home"
	// server for services that declare no explicit placement.
	ServerID string
	// BuildServerID is the dedicated server that compiles the image, when the
	// branch mapping named one. Empty means "build on the deploy target".
	BuildServerID string
}

// DeployTargetsForServer returns the current deploy target per app resource on a
// server — the latest deployment that is not failed/superseded/rolled_back. The
// reconciler renders the deploy pipeline from these instead of a bare
// container.apply.
func (s *Store) DeployTargetsForServer(ctx context.Context, serverID string) (map[string]DeployTarget, error) {
	// A Compose app may place services on servers OTHER than the one its
	// deployment was created against, so this server is a target when it owns
	// the deployment OR hosts at least one of the app's services. Without the
	// second clause a placed service would never appear in any document and
	// would silently never deploy.
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT ON (d.resource_id)
		       d.id, d.resource_id, r.project_id, d.connection_id, c.provider, c.repo_full_name,
		       d.git_ref, d.git_sha, d.config_hash, d.image_digest, COALESCE(d.image_pin,''), d.trigger, d.status,
		       d.created_at, d.service_status, COALESCE(d.server_id, ''), COALESCE(d.build_server_id, '')
		  FROM deployments d
		  JOIN resources r ON r.id = d.resource_id
		  JOIN git_connections c ON c.id = d.connection_id
		 WHERE d.status IN ('queued','building','deploying','success')
		   AND (d.server_id = $1
		        OR d.build_server_id = $1
		        OR (jsonb_typeof(r.spec->'compose'->'services') = 'array'
		            AND EXISTS (
		              SELECT 1 FROM jsonb_array_elements(r.spec->'compose'->'services') svc
		               WHERE svc->>'serverId' = $1)))
		 ORDER BY d.resource_id, d.created_at DESC`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DeployTarget{}
	for rows.Next() {
		var t DeployTarget
		var ref, sha, cfg, digest *string
		var svcStatus []byte
		if err := rows.Scan(&t.DeploymentID, &t.ResourceID, &t.ProjectID, &t.ConnectionID, &t.Provider,
			&t.RepoFullName, &ref, &sha, &cfg, &digest, &t.ImagePin, &t.Trigger, &t.Status, &t.CreatedAt,
			&svcStatus, &t.ServerID, &t.BuildServerID); err != nil {
			return nil, err
		}
		if len(svcStatus) > 0 {
			// A malformed/absent map just means "nothing has reported yet"; a
			// decode error must not drop the whole deploy target.
			_ = json.Unmarshal(svcStatus, &t.ServiceStatus)
		}
		t.Ref, t.SHA, t.ConfigHash, t.ImageDigest = deref(ref), deref(sha), deref(cfg), deref(digest)
		out[t.ResourceID] = t
	}
	return out, rows.Err()
}

// DeploymentCloneCredential returns the short-lived clone credential + repo for a
// deployment: a GitHub App installation token when the connection carries an
// installation (SIGMA-55), else the connection's KMS-wrapped PAT (P1-6
// envelope). Scoped to the REQUESTING server: an agent token can only fetch the
// credential for a deployment its own host owns a part of (BOLA). The plaintext
// token is returned to the agent for in-memory use and never persisted.
//
// "Owns a part of" is deploymentReporterClause, not `server_id` (SIGMA-228). The
// server that asks for a clone credential is by definition the server the
// git.clone op was RENDERED into, and that is not the deploy target whenever a
// dedicated build server exists — the clone+build ops live in the build server's
// document — nor for a cluster workload, whose deployment has no server_id at
// all. Matching the deploy target alone 404s exactly the agent that has to
// clone, so every private-repo deploy on those two shapes fails at clone with a
// Git auth error that reads like a bad token. Release must use the same
// predicate as render and report, or the three disagree about who owns a deploy.
func (s *Store) DeploymentCloneCredential(ctx context.Context, serverID, deploymentID string) (token, repo, provider string, err error) {
	var orgID, connID string
	err = s.Pool.QueryRow(ctx, `
		SELECT d.org_id, d.connection_id, c.repo_full_name, c.provider
		  FROM deployments d
		  JOIN git_connections c ON c.id = d.connection_id
		  JOIN resources r ON r.id = d.resource_id
		 WHERE d.id = $2 AND`+deploymentReporterClause, serverID, deploymentID).Scan(&orgID, &connID, &repo, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", err
	}
	token, err = s.gitCloneToken(ctx, orgID, connID)
	if err != nil {
		return "", "", "", err
	}
	return token, repo, provider, nil
}

// deploymentReporterClause decides whether the reporting server is entitled to
// advance a deployment. It MUST stay the mirror image of DeployTargetsForServer's
// WHERE: a server that RENDERS a deployment's ops has to be allowed to REPORT on
// them, and every asymmetry between the two is a deployment that renders, runs
// and then hangs forever because its report was silently dropped.
//
// Four ways a server legitimately owns part of a deployment:
//   - it is the deploy target;
//   - it is the dedicated build server, so the clone+build ops are in ITS document;
//   - the resource deploys into a cluster and this server is one of its nodes;
//   - it hosts one of the app's Compose services under per-service placement.
//
// $1 is the reporting server, and the query it is spliced into must expose the
// deployment as `d` and its resource as `r`.
const deploymentReporterClause = `
		   (d.server_id = $1
		    OR d.build_server_id = $1
		    OR (r.cluster_id IS NOT NULL AND EXISTS (
		          SELECT 1 FROM cluster_nodes n
		           WHERE n.cluster_id = r.cluster_id AND n.server_id = $1))
		    OR (jsonb_typeof(r.spec->'compose'->'services') = 'array'
		        AND EXISTS (
		          SELECT 1 FROM jsonb_array_elements(r.spec->'compose'->'services') svc
		           WHERE svc->>'serverId' = $1)))`

// DeployPeersForResource returns the OTHER servers whose documents are gated on
// this resource's deployment status, so they can be re-rendered the moment it
// moves.
//
// A single-server deploy needs none of this: every op is in one document and
// op-level ordering does the work. The moment a deploy spans machines it stops
// being true — the build lives in the build server's document, a placed Compose
// service in its host's, a cluster workload in the control plane's — and the
// control plane holds each of those back until the deployment says the step
// before it finished. Without a nudge, "held back" means "until the next
// 60-second resync", which a three-stage pipeline pays three times over for no
// reason. The reporting server is excluded: it has just been rendered.
func (s *Store) DeployPeersForResource(ctx context.Context, resourceID, excludeServerID string) ([]ServerRef, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH dep AS (
			SELECT d.id, d.org_id, d.server_id, d.build_server_id, r.cluster_id, r.spec
			  FROM deployments d
			  JOIN resources r ON r.id = d.resource_id
			 WHERE d.resource_id = $1 AND d.status IN ('queued','building','deploying','success')
			 ORDER BY d.created_at DESC LIMIT 1)
		SELECT DISTINCT sv, org_id FROM (
			SELECT server_id AS sv, org_id FROM dep
			UNION ALL
			SELECT build_server_id, org_id FROM dep
			UNION ALL
			SELECT n.server_id, dep.org_id FROM dep
			  JOIN cluster_nodes n ON n.cluster_id = dep.cluster_id
			UNION ALL
			SELECT svc->>'serverId', dep.org_id FROM dep,
			  LATERAL jsonb_array_elements(
			    CASE WHEN jsonb_typeof(dep.spec->'compose'->'services') = 'array'
			         THEN dep.spec->'compose'->'services' ELSE '[]'::jsonb END) svc
		) peers
		 WHERE sv IS NOT NULL AND sv <> '' AND sv <> $2`, resourceID, excludeServerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerRef
	for rows.Next() {
		var ref ServerRef
		if err := rows.Scan(&ref.ServerID, &ref.OrgID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// AdvanceDeploymentForResource transitions the in-flight deployment for a
// (server, resource) as its pipeline ops report in. phase is "clone" | "build" |
// "rollout"; a failure fails the deployment. No-op (ErrNotFound) when there is no
// in-flight deployment — so it is safe to call for every res:<id> op status,
// including non-git container.apply resources.
func (s *Store) AdvanceDeploymentForResource(ctx context.Context, serverID, resourceID, phase string, ok bool, detail string, reportVersion int64) error {
	var depID, curStatus, gitSHA, pin string
	var depVersion int64
	err := s.Pool.QueryRow(ctx, `
		SELECT d.id, d.status, COALESCE(d.git_sha,''), COALESCE(d.image_pin,''), d.dsd_version
		  FROM deployments d
		  JOIN resources r ON r.id = d.resource_id
		 WHERE d.resource_id = $2 AND d.status IN ('queued','building','deploying')
		   AND`+deploymentReporterClause+`
		 ORDER BY d.created_at DESC LIMIT 1`, serverID, resourceID).Scan(&depID, &curStatus, &gitSHA, &pin, &depVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no in-flight deployment (supersede keeps this to at most one)
	}
	if err != nil {
		return err
	}
	// Drop a stale report: it was produced from an older DSD version than the one
	// that rendered this in-flight deployment, so its clone/build/rollout ops
	// belong to a now-superseded deployment — applying them here would fabricate
	// this deployment's status (SIGMA-134). A zero on either side means "unknown"
	// (a legacy/unstamped row, or an unversioned caller); fall through unchanged.
	if reportVersion > 0 && depVersion > 0 && reportVersion < depVersion {
		return nil
	}
	up := DeploymentStatusUpdate{}
	if !ok {
		up.Status, up.Detail, up.MarkFinished = "failed", detail, true
	} else {
		var target string
		switch phase {
		case "clone":
			target = "building"
		case "build":
			target = "deploying"
		case "rollout":
			target = "success"
		default:
			return nil
		}
		// Monotonic: a single status POST reports clone/build/rollout in arbitrary
		// map order — only a forward move applies, never a regression.
		if deployStatusRank(target) <= deployStatusRank(curStatus) {
			return nil
		}
		up.Status = target
		// Stamp started_at on the first move off 'queued', whichever phase reports
		// first (SetDeploymentStatus sets it only when currently NULL).
		up.MarkStarted = true
		if target == "success" {
			up.MarkFinished = true
			// Record the deployable image reference so the release becomes a
			// rebuild-free rollback target. With a pin this is the immutable
			// per-deployment tag the build actually produced (SIGMA-173); rollback
			// and config rows carry their SOURCE's pin, so the recorded reference
			// stays the exact image that shipped.
			if gitSHA != "" {
				up.ImageDigest = pinnedImageTag(resourceID, gitSHA, pin)
			}
		}
	}
	err = s.SetDeploymentStatus(ctx, depID, up)
	if errors.Is(err, ErrConflict) {
		return nil // already terminal — a concurrent report won
	}
	return err
}

// AdvanceDeploymentService tracks one Compose service's status on the in-flight
// deployment and rolls the OVERALL status up from the per-service map: the
// deployment is 'failed' the moment any service fails, 'success' once every
// service (service_count of them) succeeds, otherwise 'deploying'. Isolated from
// the single-container AdvanceDeploymentForResource path. A no-op when there is no
// in-flight deployment.
func (s *Store) AdvanceDeploymentService(ctx context.Context, serverID, resourceID, service, phase string, ok bool, detail string, reportVersion int64) error {
	svcStatus := ""
	if !ok {
		svcStatus = "failed"
	} else {
		switch phase {
		case "build":
			svcStatus = "deploying"
		case "rollout":
			svcStatus = "success"
		default:
			return nil // clone/other phases don't carry a per-service status
		}
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var depID, gitSHA string
	var serviceCount int
	var depVersion int64
	err = tx.QueryRow(ctx, `
		SELECT d.id, COALESCE(d.git_sha,''), d.service_count, d.dsd_version
		  FROM deployments d
		  JOIN resources r ON r.id = d.resource_id
		 WHERE d.resource_id = $2 AND d.status IN ('queued','building','deploying')
		   AND`+deploymentReporterClause+`
		 ORDER BY d.created_at DESC LIMIT 1`, serverID, resourceID).Scan(&depID, &gitSHA, &serviceCount, &depVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// Drop a stale per-service report from a superseded deployment (older DSD
	// version than the in-flight one) so it can't fabricate the new deployment's
	// status (SIGMA-134). Zero on either side = unknown → fall through.
	if reportVersion > 0 && depVersion > 0 && reportVersion < depVersion {
		return nil
	}

	// Record this service's status, but never regress a service already 'success'
	// (a re-report on a later DSD version must not undo a completed service).
	var raw []byte
	err = tx.QueryRow(ctx, `
		UPDATE deployments
		   SET service_status = jsonb_set(COALESCE(service_status,'{}'::jsonb), ARRAY[$2::text], to_jsonb($3::text), true)
		 WHERE id = $1 AND COALESCE(service_status->>$2, '') <> 'success'
		RETURNING service_status`, depID, service, svcStatus).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		// The service was already 'success' (no update) — recompute from the current
		// map so a straggler still completes the deployment.
		if err := tx.QueryRow(ctx, `SELECT service_status FROM deployments WHERE id = $1`, depID).Scan(&raw); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	statuses := map[string]string{}
	_ = json.Unmarshal(raw, &statuses)
	failed := false
	success := 0
	for _, st := range statuses {
		switch st {
		case "failed":
			failed = true
		case "success":
			success++
		}
	}

	up := DeploymentStatusUpdate{}
	switch {
	case failed:
		up.Status, up.Detail, up.MarkFinished = "failed", detail, true
	case serviceCount > 0 && success >= serviceCount:
		up.Status, up.MarkStarted, up.MarkFinished = "success", true, true
		// Deliberately NO image_digest stamp: the single-container tag was never
		// built for a Compose resource (SIGMA-168 — the fabricated marker put
		// un-reshippable releases into RollbackTargets). Compose eligibility now
		// rides image_pin, stamped at creation, from which every service's
		// pinned tag is re-derivable.
	default:
		up.Status, up.MarkStarted = "deploying", true
	}

	// Apply the overall transition within the same tx (terminal-freeze respected).
	if err := setDeploymentStatusTx(ctx, tx, depID, up); errors.Is(err, ErrConflict) {
		return tx.Commit(ctx) // already terminal — keep the per-service update
	} else if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// composeServiceCount returns how many RUNNABLE Compose services a resource spec
// declares (0 for a single-container app), so a deployment knows how many service
// rollouts must succeed before it is done. The runnable rule (a name plus a build
// context or a prebuilt image) MUST match the reconciler's validComposeServices
// filter, or the success denominator would diverge from the rendered ops.
func composeServiceCount(spec []byte) int {
	var s struct {
		Compose *struct {
			Services []struct {
				Name  string `json:"name"`
				Build string `json:"build"`
				Image string `json:"image"`
			} `json:"services"`
		} `json:"compose"`
	}
	if json.Unmarshal(spec, &s) != nil || s.Compose == nil {
		return 0
	}
	n := 0
	for _, svc := range s.Compose.Services {
		if svc.Name != "" && (svc.Build != "" || svc.Image != "") {
			n++
		}
	}
	return n
}

// resourceServiceCountTx computes the CURRENT compose service count for a
// resource — used by rollback/redeploy so the per-service denominator reflects
// the spec that will actually be rendered, not a stale copy from a prior row.
// resolveBuildServerTx answers "where does this deployment build?" for the
// creators that mint a deployment from an EARLIER one instead of from a push:
// manual redeploy, rollback and config deploy (SIGMA-231).
//
// Only the push path (DrainDeployRequests) and CreateHeadDeployment ever wrote
// build_server_id, so those three minted rows with the column NULL. That is not
// cosmetic: ClusterBuildSpecsForServer keys on it, and it is the ONLY thing that
// puts a cluster workload's clone+build ops into any document at all — a cluster
// app whose redeploy lost the column can be built by nobody, so it sits 'queued'
// with an empty log until TimeoutStaleDeployments fails it 45 minutes later with
// a message blaming the agent. clusterImageReady keys on it too. And because
// each of these creators copies from the resource's MOST RECENT deployment, one
// dropped column poisons every deploy after it.
//
// Prefer the source row (a deploy's history should explain itself), and fall
// back to the branch map the way DrainDeployRequests resolves it — that covers a
// release created before the operator picked a build server.
func resolveBuildServerTx(ctx context.Context, tx pgx.Tx, srcBuildServer, connID, envID string) (string, error) {
	if srcBuildServer != "" {
		return srcBuildServer, nil
	}
	if connID == "" || envID == "" {
		return "", nil
	}
	var out string
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(build_server_id,'') FROM git_branch_map
		 WHERE connection_id = $1 AND environment_id = $2 LIMIT 1`, connID, envID).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return out, err
}

func resourceServiceCountTx(ctx context.Context, tx pgx.Tx, orgID, resourceID string) (int, error) {
	var spec []byte
	err := tx.QueryRow(ctx, `SELECT spec FROM resources WHERE org_id = $1 AND id = $2`, orgID, resourceID).Scan(&spec)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return composeServiceCount(spec), nil
}

// AppendDeployLog appends one build/orchestration log line, scoped to the
// reporting server (BOLA guard — an agent can't forge or read into another
// host's deploy logs). A line for a deployment this server owns no part of is
// silently dropped.
//
// "Owns a part of" is deploymentReporterClause, not `server_id`: the servers
// that produce these lines are exactly the servers that render the ops. A
// dedicated build server holds the clone+build ops, so every build log comes
// from a host that is not the deploy target; a cluster workload's rollout runs
// on a node; a placed Compose service's startup logs come from its host. Match
// on the deploy target alone and each of those streams is written by a server
// the predicate rejects — the deploy view then shows an empty log for the one
// deployment whose failure you most need to read.
func (s *Store) AppendDeployLog(ctx context.Context, serverID, deploymentID, stream, line string) error {
	if stream == "" {
		stream = "build"
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO deploy_logs (deployment_id, stream, line)
		SELECT $2, $3, $4
		 WHERE EXISTS (
		   SELECT 1 FROM deployments d
		     JOIN resources r ON r.id = d.resource_id
		    WHERE d.id = $2 AND`+deploymentReporterClause+`)`,
		serverID, deploymentID, stream, line)
	return err
}

// DeployLog is one streamed build/orchestration log line.
type DeployLog struct {
	ID     int64     `json:"id"`
	Stream string    `json:"stream"`
	Line   string    `json:"line"`
	At     time.Time `json:"at"`
}

// DeployLogsSince returns log lines with id > afterID (SSE cursor), oldest first.
func (s *Store) DeployLogsSince(ctx context.Context, deploymentID string, afterID int64, limit int) ([]DeployLog, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, stream, line, at FROM deploy_logs
		 WHERE deployment_id = $1 AND id > $2 ORDER BY id LIMIT $3`, deploymentID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeployLog{}
	for rows.Next() {
		var l DeployLog
		if err := rows.Scan(&l.ID, &l.Stream, &l.Line, &l.At); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
