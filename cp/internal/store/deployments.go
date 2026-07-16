package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// supersedeInFlightTx freezes any still-in-flight deployment for a (server,
// resource) as 'superseded' — called in the same tx that creates a newer
// deployment, so there is at most ONE in-flight deployment per (server,resource).
// That single-in-flight invariant is what lets the op-status path advance "the
// in-flight deployment" unambiguously, and it makes the promised "superseded when
// a newer deploy wins the race" state actually hold (no lingering orphan rows).
func supersedeInFlightTx(ctx context.Context, tx pgx.Tx, orgID, serverID, resourceID string) error {
	if serverID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE deployments SET
			status = 'superseded',
			finished_at = now(),
			duration_seconds = GREATEST(0, EXTRACT(EPOCH FROM (now() - COALESCE(started_at, created_at)))::int)
		 WHERE org_id = $1 AND server_id = $2 AND resource_id = $3
		   AND status IN ('queued','building','deploying')`,
		orgID, serverID, resourceID)
	return err
}

// deployImageTag mirrors dsd.DeployImageTag — the deterministic image reference a
// git-deployed resource's build produces and its rollout runs. Kept inline (not
// an import) to avoid a store→dsd dependency; the two MUST stay in sync.
func deployImageTag(resourceID, sha string) string {
	return "sigmahub/" + resourceID + ":" + sha
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

	// The resource must belong to the org; capture its server if not supplied.
	var serverID string
	err = tx.QueryRow(ctx,
		`SELECT server_id FROM resources WHERE org_id = $1 AND id = $2`, orgID, in.ResourceID).Scan(&serverID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	if in.ServerID != "" {
		serverID = in.ServerID
	}

	d := Deployment{
		ID: newID("dep"), OrgID: orgID, ResourceID: in.ResourceID, EnvironmentID: in.EnvironmentID,
		ServerID: serverID, ConnectionID: in.ConnectionID, Trigger: trigger, GitRef: in.GitRef,
		GitSHA: in.GitSHA, ConfigHash: in.ConfigHash, ImageDigest: in.ImageDigest,
		RollbackOf: in.RollbackOf, Status: "queued", CreatedBy: actor,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id, trigger,
		                         git_ref, git_sha, image_digest, config_hash, rollback_of, status, created_by)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,$8,$9,NULLIF($10,''),$11,NULLIF($12,''),'queued',$13)
		RETURNING created_at`,
		d.ID, orgID, in.ResourceID, in.EnvironmentID, serverID, in.ConnectionID, trigger,
		in.GitRef, in.GitSHA, in.ImageDigest, in.ConfigHash, in.RollbackOf, actor).Scan(&d.CreatedAt)
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

	var cur string
	var startedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT status, started_at FROM deployments WHERE id = $1 FOR UPDATE`, deploymentID).Scan(&cur, &startedAt)
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
	return tx.Commit(ctx)
}

// ListDeployments returns a resource's release history newest-first (org-scoped).
func (s *Store) ListDeployments(ctx context.Context, orgID, resourceID string, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, resource_id, environment_id, server_id, connection_id, trigger, git_ref, git_sha,
		       image_digest, config_hash, status, detail, rollback_of, build_seconds, duration_seconds,
		       created_by, created_at, started_at, finished_at
		  FROM deployments WHERE org_id = $1 AND resource_id = $2
		 ORDER BY created_at DESC LIMIT $3`, orgID, resourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDeployments(rows)
}

// RollbackTargets returns a resource's last N SUCCESSFUL releases (each with a
// built image digest), newest-first — the candidates a &lt;30s rebuild-free
// rollback can pick from.
func (s *Store) RollbackTargets(ctx context.Context, orgID, resourceID string, limit int) ([]Deployment, error) {
	if limit <= 0 || limit > 10 {
		limit = 10
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, resource_id, environment_id, server_id, connection_id, trigger, git_ref, git_sha,
		       image_digest, config_hash, status, detail, rollback_of, build_seconds, duration_seconds,
		       created_by, created_at, started_at, finished_at
		  FROM deployments
		 WHERE org_id = $1 AND resource_id = $2 AND status = 'success' AND image_digest IS NOT NULL
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
		if err := rows.Scan(&d.ID, &d.OrgID, &d.ResourceID, &env, &srv, &conn, &d.Trigger, &ref, &sha,
			&digest, &cfg, &d.Status, &d.Detail, &rollback, &d.BuildSeconds, &d.DurationSeconds,
			&d.CreatedBy, &d.CreatedAt, &d.StartedAt, &d.FinishedAt); err != nil {
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
		       created_by, created_at, started_at, finished_at
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
	err = tx.QueryRow(ctx, `
		SELECT environment_id, server_id, connection_id, git_ref, git_sha, image_digest, config_hash, status
		  FROM deployments WHERE org_id = $1 AND resource_id = $2 AND id = $3`,
		orgID, resourceID, targetDeploymentID).Scan(&env, &srv, &conn, &ref, &sha, &digest, &cfg, &t.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, "", ErrNotFound
	}
	if err != nil {
		return Deployment{}, "", err
	}
	if t.Status != "success" || digest == nil || *digest == "" {
		return Deployment{}, "", ErrInvalid{Msg: "rollback target must be a successful release with a built image"}
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
	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id, trigger,
		                         git_ref, git_sha, image_digest, config_hash, rollback_of, status, created_by)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),'rollback',NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),NULLIF($10,''),$11,'queued',$12)
		RETURNING created_at`,
		d.ID, orgID, resourceID, d.EnvironmentID, d.ServerID, d.ConnectionID,
		d.GitRef, d.GitSHA, d.ImageDigest, d.ConfigHash, targetDeploymentID, actor).Scan(&d.CreatedAt)
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
	err = tx.QueryRow(ctx, `
		SELECT environment_id, server_id, connection_id, git_ref, git_sha, config_hash
		  FROM deployments WHERE org_id = $1 AND resource_id = $2
		 ORDER BY created_at DESC LIMIT 1`, orgID, resourceID).Scan(&env, &srv, &conn, &ref, &sha, &cfg)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, "", ErrInvalid{Msg: "nothing to redeploy — connect a repo and push first"}
	}
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
	err = tx.QueryRow(ctx, `
		INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id, trigger,
		                         git_ref, git_sha, config_hash, status, created_by)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),'manual',NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),'queued',$10)
		RETURNING created_at`,
		d.ID, orgID, resourceID, d.EnvironmentID, d.ServerID, d.ConnectionID, d.GitRef, d.GitSHA, d.ConfigHash, actor).Scan(&d.CreatedAt)
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

	rows, err := tx.Query(ctx, `
		SELECT dr.id, dr.org_id, dr.connection_id, dr.environment_id, dr.ref, dr.sha, c.project_id
		  FROM deploy_requests dr
		  JOIN git_connections c ON c.id = dr.connection_id
		 WHERE dr.kind = 'deploy' AND dr.status = 'queued'
		 ORDER BY dr.created_at
		 FOR UPDATE OF dr SKIP LOCKED`)
	if err != nil {
		return nil, err
	}
	type req struct{ id, orgID, connID, envID, ref, sha, projectID string }
	var reqs []req
	for rows.Next() {
		var r req
		var env *string
		if err := rows.Scan(&r.id, &r.orgID, &r.connID, &env, &r.ref, &r.sha, &r.projectID); err != nil {
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
			SELECT id, server_id, spec FROM resources
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
			// A newer push supersedes any still-in-flight deploy for this resource,
			// so only one deployment per (server,resource) is ever in flight.
			if err := supersedeInFlightTx(ctx, tx, r.orgID, a.serverID, a.id); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO deployments (id, org_id, resource_id, environment_id, server_id, connection_id,
				                         trigger, git_ref, git_sha, config_hash, status, created_by)
				VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,'git',$7,$8,$9,'queued','github-webhook')`,
				newID("dep"), r.orgID, a.id, r.envID, a.serverID, r.connID, r.ref, r.sha, cfgHash); err != nil {
				return nil, err
			}
			if a.serverID != "" {
				seen[a.serverID] = ServerRef{ServerID: a.serverID, OrgID: r.orgID}
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE deploy_requests SET status = 'drained' WHERE id = $1`, r.id); err != nil {
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
	Trigger      string
}

// DeployTargetsForServer returns the current deploy target per app resource on a
// server — the latest deployment that is not failed/superseded/rolled_back. The
// reconciler renders the deploy pipeline from these instead of a bare
// container.apply.
func (s *Store) DeployTargetsForServer(ctx context.Context, serverID string) (map[string]DeployTarget, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT DISTINCT ON (d.resource_id)
		       d.id, d.resource_id, r.project_id, d.connection_id, c.provider, c.repo_full_name,
		       d.git_ref, d.git_sha, d.config_hash, d.image_digest, d.trigger
		  FROM deployments d
		  JOIN resources r ON r.id = d.resource_id
		  JOIN git_connections c ON c.id = d.connection_id
		 WHERE d.server_id = $1 AND d.status IN ('queued','building','deploying','success')
		 ORDER BY d.resource_id, d.created_at DESC`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]DeployTarget{}
	for rows.Next() {
		var t DeployTarget
		var ref, sha, cfg, digest *string
		if err := rows.Scan(&t.DeploymentID, &t.ResourceID, &t.ProjectID, &t.ConnectionID, &t.Provider,
			&t.RepoFullName, &ref, &sha, &cfg, &digest, &t.Trigger); err != nil {
			return nil, err
		}
		t.Ref, t.SHA, t.ConfigHash, t.ImageDigest = deref(ref), deref(sha), deref(cfg), deref(digest)
		out[t.ResourceID] = t
	}
	return out, rows.Err()
}

// DeploymentCloneCredential returns the short-lived clone credential + repo for a
// deployment, resolved from the connection's KMS-wrapped provider token (P1-6
// envelope). Scoped to the REQUESTING server: an agent token can only fetch the
// credential for a deployment targeting its own host (BOLA). The plaintext token
// is returned to the agent for in-memory use and never persisted.
func (s *Store) DeploymentCloneCredential(ctx context.Context, serverID, deploymentID string) (token, repo, provider string, err error) {
	var orgID, connID string
	err = s.Pool.QueryRow(ctx, `
		SELECT d.org_id, d.connection_id, c.repo_full_name, c.provider
		  FROM deployments d JOIN git_connections c ON c.id = d.connection_id
		 WHERE d.id = $1 AND d.server_id = $2`, deploymentID, serverID).Scan(&orgID, &connID, &repo, &provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", ErrNotFound
	}
	if err != nil {
		return "", "", "", err
	}
	token, err = s.gitProviderToken(ctx, orgID, connID)
	if err != nil {
		return "", "", "", err
	}
	return token, repo, provider, nil
}

// AdvanceDeploymentForResource transitions the in-flight deployment for a
// (server, resource) as its pipeline ops report in. phase is "clone" | "build" |
// "rollout"; a failure fails the deployment. No-op (ErrNotFound) when there is no
// in-flight deployment — so it is safe to call for every res:<id> op status,
// including non-git container.apply resources.
func (s *Store) AdvanceDeploymentForResource(ctx context.Context, serverID, resourceID, phase string, ok bool, detail string) error {
	var depID, curStatus, gitSHA string
	err := s.Pool.QueryRow(ctx, `
		SELECT id, status, COALESCE(git_sha,'') FROM deployments
		 WHERE server_id = $1 AND resource_id = $2 AND status IN ('queued','building','deploying')
		 ORDER BY created_at DESC LIMIT 1`, serverID, resourceID).Scan(&depID, &curStatus, &gitSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no in-flight deployment (supersede keeps this to at most one)
	}
	if err != nil {
		return err
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
			// rebuild-free rollback target (RollbackTargets requires image_digest).
			if gitSHA != "" {
				up.ImageDigest = deployImageTag(resourceID, gitSHA)
			}
		}
	}
	err = s.SetDeploymentStatus(ctx, depID, up)
	if errors.Is(err, ErrConflict) {
		return nil // already terminal — a concurrent report won
	}
	return err
}

// AppendDeployLog appends one build/orchestration log line, scoped to the
// reporting server: the row is written only when the deployment targets that
// server (BOLA guard — an agent can't forge or read into another host's deploy
// logs). A line for a deployment not on this server is silently dropped.
func (s *Store) AppendDeployLog(ctx context.Context, serverID, deploymentID, stream, line string) error {
	if stream == "" {
		stream = "build"
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO deploy_logs (deployment_id, stream, line)
		SELECT $1, $2, $3
		 WHERE EXISTS (SELECT 1 FROM deployments WHERE id = $1 AND server_id = $4)`,
		deploymentID, stream, line, serverID)
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
