package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/gitdetect"
)

// ErrConflict marks uniqueness violations (duplicate names, key reuse).
var ErrConflict = errors.New("conflict")

// ErrInvalid marks domain-rule violations (bad kind, availability matrix,
// unattached server); the message is safe to return to the caller.
type ErrInvalid struct{ Msg string }

func (e ErrInvalid) Error() string { return e.Msg }

type Project struct {
	ID          string    `json:"id"`
	OrgID       string    `json:"orgId"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Environment struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"orgId"`
	ProjectID  string    `json:"projectId"`
	Name       string    `json:"name"`
	Production bool      `json:"production"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Resource struct {
	ID            string          `json:"id"`
	OrgID         string          `json:"orgId"`
	ProjectID     string          `json:"projectId"`
	EnvironmentID string          `json:"environmentId"`
	ServerID      string          `json:"serverId"`
	Name          string          `json:"name"`
	Kind          string          `json:"kind"`
	Spec          json.RawMessage `json:"spec"`
	Status        json.RawMessage `json:"status"`
	// Ephemeral marks a PR-preview resource (ensurePreviewTx): torn down with
	// its PR, not a first-class service. Surfaced so the dashboard can badge it
	// and guard its Delete instead of presenting it as ordinary (SIGMA-194).
	Ephemeral bool      `json:"ephemeral"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// The server-type list, the availability matrix and the per-type enrollment
// requirements all live in server_catalog.go — one definition the dashboard is
// generated from. IsServerType / ServerTypes / AllowedServerTypes are there.

func isUniqueViolation(err error) bool {
	// 23505 = unique_violation; matching by SQLSTATE text keeps us off
	// pgconn-internal types.
	return err != nil && strings.Contains(err.Error(), "23505")
}

func isForeignKeyViolation(err error) bool {
	// 23503 = foreign_key_violation.
	return err != nil && strings.Contains(err.Error(), "23503")
}

// ── Projects ────────────────────────────────────────────────────────────────

func (s *Store) CreateProject(ctx context.Context, orgID, name, description, actor string) (Project, error) {
	p := Project{ID: newID("prj")}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Project{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO projects (id, org_id, name, description, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, org_id, name, description, created_by, created_at`,
		p.ID, orgID, name, description, actor,
	).Scan(&p.ID, &p.OrgID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt)
	if isUniqueViolation(err) {
		return Project{}, fmt.Errorf("%w: a project named %q already exists", ErrConflict, name)
	}
	if err != nil {
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Project created", name); err != nil {
		return Project{}, err
	}
	return p, tx.Commit(ctx)
}

func (s *Store) ListProjects(ctx context.Context, orgID string) ([]Project, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, name, description, created_by, created_at
		  FROM projects WHERE org_id = $1 ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetProject(ctx context.Context, orgID, projectID string) (Project, error) {
	var p Project
	err := s.Pool.QueryRow(ctx, `
		SELECT id, org_id, name, description, created_by, created_at
		  FROM projects WHERE org_id = $1 AND id = $2`, orgID, projectID,
	).Scan(&p.ID, &p.OrgID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// UpdateProject renames a project / updates its description.
func (s *Store) UpdateProject(ctx context.Context, orgID, projectID, name, description, actor string) (Project, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Project{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var p Project
	err = tx.QueryRow(ctx, `
		UPDATE projects SET name = $3, description = $4
		 WHERE org_id = $1 AND id = $2
		 RETURNING id, org_id, name, description, created_by, created_at`,
		orgID, projectID, name, description,
	).Scan(&p.ID, &p.OrgID, &p.Name, &p.Description, &p.CreatedBy, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if isUniqueViolation(err) {
		return Project{}, fmt.Errorf("%w: a project named %q already exists", ErrConflict, name)
	}
	if err != nil {
		return Project{}, fmt.Errorf("update project: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Project updated", name); err != nil {
		return Project{}, err
	}
	return p, tx.Commit(ctx)
}

// DeleteProject removes a project (cascading its environments and resources)
// and returns the distinct servers those resources ran on, so the caller can
// re-render their DSDs — without the nudge a "deleted" database kept serving
// connections for up to a minute until the fleet resync (SIGMA-193).
func (s *Store) DeleteProject(ctx context.Context, orgID, projectID, actor string) ([]string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Retain the restic repo keys of every database in this project BEFORE the
	// cascade removes their backup policies — otherwise the customer's offsite
	// snapshots survive in their bucket as undecryptable ciphertext (SIGMA-170).
	if err := archiveRepoKeysTx(ctx, tx, orgID, "project", projectID); err != nil {
		return nil, fmt.Errorf("archive repo keys: %w", err)
	}
	servers, err := cascadeResourceCleanupTx(ctx, tx, orgID, "project_id", projectID)
	if err != nil {
		return nil, err
	}

	var name string
	err = tx.QueryRow(ctx,
		`DELETE FROM projects WHERE org_id = $1 AND id = $2 RETURNING name`,
		orgID, projectID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("delete project: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Project deleted", name); err != nil {
		return nil, err
	}
	return servers, tx.Commit(ctx)
}

// ── Environments ────────────────────────────────────────────────────────────

func (s *Store) CreateEnvironment(ctx context.Context, orgID, projectID, name string, production bool, actor string) (Environment, error) {
	// Resolve the project inside the org so a cross-tenant projectID 404s.
	proj, err := s.GetProject(ctx, orgID, projectID)
	if err != nil {
		return Environment{}, err
	}

	e := Environment{ID: newID("env")}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Environment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = tx.QueryRow(ctx, `
		INSERT INTO environments (id, org_id, project_id, name, production)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, org_id, project_id, name, production, created_at`,
		e.ID, orgID, projectID, name, production,
	).Scan(&e.ID, &e.OrgID, &e.ProjectID, &e.Name, &e.Production, &e.CreatedAt)
	if isUniqueViolation(err) {
		return Environment{}, fmt.Errorf("%w: environment %q already exists in this project", ErrConflict, name)
	}
	if err != nil {
		return Environment{}, fmt.Errorf("insert environment: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Environment created", proj.Name+"/"+name); err != nil {
		return Environment{}, err
	}
	return e, tx.Commit(ctx)
}

func (s *Store) ListEnvironments(ctx context.Context, orgID, projectID string) ([]Environment, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, project_id, name, production, created_at
		  FROM environments WHERE org_id = $1 AND project_id = $2 ORDER BY created_at`,
		orgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Environment{}
	for rows.Next() {
		var e Environment
		if err := rows.Scan(&e.ID, &e.OrgID, &e.ProjectID, &e.Name, &e.Production, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEnvironment removes an environment (cascading its resources) and
// returns the distinct servers to re-render — same rationale as DeleteProject
// (SIGMA-193).
func (s *Store) DeleteEnvironment(ctx context.Context, orgID, envID, actor string) ([]string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Same key retention as DeleteProject (SIGMA-170) — an environment cascade
	// takes its resources' backup policies with it.
	if err := archiveRepoKeysTx(ctx, tx, orgID, "environment", envID); err != nil {
		return nil, fmt.Errorf("archive repo keys: %w", err)
	}
	servers, err := cascadeResourceCleanupTx(ctx, tx, orgID, "environment_id", envID)
	if err != nil {
		return nil, err
	}

	var name string
	err = tx.QueryRow(ctx,
		`DELETE FROM environments WHERE org_id = $1 AND id = $2 RETURNING name`,
		orgID, envID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("delete environment: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Environment deleted", name); err != nil {
		return nil, err
	}
	return servers, tx.Commit(ctx)
}

// UpdateEnvironmentProduction flips an environment's production flag — the
// seed for new databases' backup retention. It was write-once at creation and
// the web derived it from a magic name match ("production"/"prod"), so a prod
// environment named "live" or "prd" silently got the non-production backup
// defaults forever, with no way to correct it (SIGMA-190). Audited.
func (s *Store) UpdateEnvironmentProduction(ctx context.Context, orgID, envID string, production bool, actor string) (Environment, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Environment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var e Environment
	err = tx.QueryRow(ctx, `
		UPDATE environments SET production = $3
		 WHERE org_id = $1 AND id = $2
		 RETURNING id, org_id, project_id, name, production, created_at`,
		orgID, envID, production).Scan(&e.ID, &e.OrgID, &e.ProjectID, &e.Name, &e.Production, &e.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, ErrNotFound
	}
	if err != nil {
		return Environment{}, err
	}
	action := "Environment unmarked production"
	if production {
		action = "Environment marked production"
	}
	if err := auditTx(ctx, tx, orgID, actor, action, e.Name); err != nil {
		return Environment{}, err
	}
	return e, tx.Commit(ctx)
}

// cascadeResourceCleanupTx performs DeleteResource's post-delete duties for
// every resource a project/environment cascade is about to remove (SIGMA-193):
// it queues the pre-authorised volume teardown for EPHEMERAL resources (the
// same carve-out DeleteResource applies — non-ephemeral volumes deliberately
// stay on disk), queues the Kubernetes manifest teardown for CLUSTER-deployed
// resources (SIGMA-312 — deleting the project is as final for those workloads as
// deleting them one by one, and nothing else will ever ask the node to stop
// running them), and returns the distinct servers whose DSD must re-render.
// MUST run BEFORE the DELETE that triggers the cascade — afterwards there is
// nothing left to read. scopeCol is an internal constant, never user input.
func cascadeResourceCleanupTx(ctx context.Context, tx pgx.Tx, orgID, scopeCol, scopeID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, COALESCE(server_id,''), COALESCE(cluster_id,''), spec, ephemeral
		   FROM resources WHERE org_id = $1 AND `+scopeCol+` = $2`,
		orgID, scopeID)
	if err != nil {
		return nil, err
	}
	type res struct {
		id, serverID, clusterID string
		spec                    json.RawMessage
		ephemeral               bool
	}
	var all []res
	for rows.Next() {
		var r res
		if err := rows.Scan(&r.id, &r.serverID, &r.clusterID, &r.spec, &r.ephemeral); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var servers []string
	for _, r := range all {
		if r.serverID != "" && !seen[r.serverID] {
			seen[r.serverID] = true
			servers = append(servers, r.serverID)
		}
		// A cluster workload is bound to no server, so its re-render target is the
		// control-plane node that will apply the teardown.
		if r.clusterID != "" {
			cpServer, err := insertK8sTeardownTx(ctx, tx, orgID, r.clusterID, r.id, r.spec)
			if err != nil {
				return nil, err
			}
			if cpServer != "" && !seen[cpServer] {
				seen[cpServer] = true
				servers = append(servers, cpServer)
			}
		}
		if !r.ephemeral {
			continue
		}
		for _, vol := range resourceVolumeNames(r.id, r.spec) {
			if _, err := insertPendingDestructiveOpTx(ctx, tx, orgID, r.serverID, dsd.KindVolumeRemove, vol, "system"); err != nil {
				return nil, err
			}
			if err := auditTx(ctx, tx, orgID, "system", "Destructive-op confirm requested (ephemeral)", dsd.KindVolumeRemove+" "+vol); err != nil {
				return nil, err
			}
			if err := auditTx(ctx, tx, orgID, "system", "Destructive-op confirmed (ephemeral)", dsd.KindVolumeRemove+" "+vol); err != nil {
				return nil, err
			}
		}
	}
	return servers, nil
}

// ── Env ↔ server attachment ─────────────────────────────────────────────────

func (s *Store) AttachServer(ctx context.Context, orgID, envID, serverID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Both sides must exist in this org; anything else is a 404, never a
	// cross-tenant write.
	var envName, srvName string
	if err := tx.QueryRow(ctx,
		`SELECT name FROM environments WHERE org_id = $1 AND id = $2`, orgID, envID,
	).Scan(&envName); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err := tx.QueryRow(ctx,
		`SELECT name FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`, orgID, serverID,
	).Scan(&srvName); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO env_servers (environment_id, server_id, org_id)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, envID, serverID, orgID); err != nil {
		return fmt.Errorf("attach server: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Server attached", srvName+" -> "+envName); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) DetachServer(ctx context.Context, orgID, envID, serverID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		DELETE FROM env_servers WHERE org_id = $1 AND environment_id = $2 AND server_id = $3`,
		orgID, envID, serverID)
	if err != nil {
		return fmt.Errorf("detach server: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := auditTx(ctx, tx, orgID, actor, "Server detached", serverID+" x "+envID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnvServerIDs returns the servers attached to an environment.
func (s *Store) EnvServerIDs(ctx context.Context, orgID, envID string) ([]string, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT server_id FROM env_servers WHERE org_id = $1 AND environment_id = $2 ORDER BY created_at`,
		orgID, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ── Resources ───────────────────────────────────────────────────────────────

type CreateResourceInput struct {
	EnvironmentID string
	ServerID      string
	// ClusterID deploys INTO a Kubernetes cluster instead of onto one server.
	// Mutually exclusive with ServerID.
	ClusterID string
	Name      string
	Kind      string
	Spec      json.RawMessage
}

// buildMethodFromSpec reads spec.build.method — how the wizard decided this app
// gets built. Empty (every resource created before the wizard could express it,
// and every non-app kind) means the historical default: a Dockerfile at the
// clone root.
func buildMethodFromSpec(spec json.RawMessage) string {
	if len(bytes.TrimSpace(spec)) == 0 {
		return ""
	}
	var s struct {
		Build *struct {
			Method string `json:"method"`
		} `json:"build"`
	}
	if err := json.Unmarshal(spec, &s); err != nil || s.Build == nil {
		return ""
	}
	return s.Build.Method
}

// specHasComposeServices reports whether the spec carries a non-empty compose
// service graph.
//
// Non-empty is the test, not merely present: `"compose": {"services": []}` is
// the same nothing as an absent block — it renders no containers — and it is
// what a detection that found a compose file it could not parse would leave
// behind.
func specHasComposeServices(spec json.RawMessage) bool {
	if len(bytes.TrimSpace(spec)) == 0 {
		return false
	}
	var s struct {
		Compose *struct {
			Services []json.RawMessage `json:"services"`
		} `json:"compose"`
	}
	if err := json.Unmarshal(spec, &s); err != nil || s.Compose == nil {
		return false
	}
	return len(s.Compose.Services) > 0
}

// CreateResource enforces the domain rules the UI can't be trusted with:
// known kind, availability matrix against the server's type, and the server
// actually being attached to the target environment.
func (s *Store) CreateResource(ctx context.Context, orgID string, in CreateResourceInput, actor string) (Resource, error) {
	allowed := AllowedServerTypes(in.Kind)
	if allowed == nil {
		return Resource{}, ErrInvalid{Msg: fmt.Sprintf("unknown resource kind %q", in.Kind)}
	}
	// P1-10 engine gate: the Postgres-only fallback build disables engines by
	// configuration, so creation must fail loudly rather than provision nothing.
	if IsDBKind(in.Kind) && !s.dbEngineEnabled(in.Kind) {
		return Resource{}, ErrInvalid{Msg: fmt.Sprintf("database engine %q is not enabled on this control plane", in.Kind)}
	}
	// P2-2 S3 engine selection + gate: the engine rides the resource spec
	// (default MinIO). An unknown or disabled engine fails create loudly rather
	// than provisioning the wrong (or no) engine.
	var s3Engine string
	if IsS3Kind(in.Kind) {
		s3Engine = s3EngineFromSpec(in.Spec)
		if !IsS3Engine(s3Engine) {
			return Resource{}, ErrInvalid{Msg: fmt.Sprintf("unknown s3 engine %q", s3Engine)}
		}
		if !s.s3EngineEnabled(s3Engine) {
			return Resource{}, ErrInvalid{Msg: fmt.Sprintf("s3 engine %q is not enabled on this control plane", s3Engine)}
		}
	}
	// The wizard's build decision rides the spec (SIGMA-209). The agent refuses
	// a builder it does not know, which is the right last line of defence but a
	// terrible first one: by then the resource exists, the operator has closed
	// the wizard, and the failure surfaces as a red deployment. Refuse it here,
	// symmetric with the engine gates above.
	if method := buildMethodFromSpec(in.Spec); method != "" {
		if !gitdetect.ValidBuildMethod(method) {
			return Resource{}, ErrInvalid{Msg: fmt.Sprintf("unknown build method %q", method)}
		}
		// A Compose app IS its service graph, and spec.compose is the only thing
		// that tells the reconciler to treat it as one. Without it the render
		// takes the single-container path: one image built from a Dockerfile the
		// repository may not have, and every other service never built, never
		// started and never mentioned anywhere.
		//
		// That combination used to be accepted. What the operator saw was an app
		// that simply never came up, next to a service-graph panel answering 404
		// — which the dashboard reports as "the control plane didn't answer",
		// because a resource with no graph and a control plane that cannot be
		// reached are the same response to a page that only asked one question.
		// Nothing anywhere said the resource was missing the one field its build
		// method depends on.
		//
		// Refused here for the reason stated above about builders: the agent is
		// the right LAST line of defence and a terrible first one, since by then
		// the resource exists and the wizard has closed.
		if method == gitdetect.BuildCompose && !specHasComposeServices(in.Spec) {
			return Resource{}, ErrInvalid{Msg: "this app is set to build from Docker Compose but carries no service graph — " +
				"re-run detection on the repository so the compose file's services are read, or pick a different build method"}
		}
	}
	// SIGMA-214: look the requested model up BEFORE the transaction opens. The
	// lookup is a call to huggingface.co, and a transaction held open across a
	// third party's latency is how a slow dependency becomes a lock queue on our
	// own database. What comes back decides two things below — whether any
	// runtime we render can serve this repository at all, and whether it fits the
	// card on the target host — and an absent answer (the Hub is down, the model
	// is an Ollama tag, nobody wired a sizer) is the fail-open state that skips
	// both, so a huggingface.co incident never stops a deploy. See llm_fit.go.
	var modelSize ModelSize
	if IsLLMKind(in.Kind) {
		modelSize = s.sizeModelForFit(ctx, in.Spec)
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Resource{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SIGMA-295: same cap as server creation — an org past its billing grace
	// period does not get to provision NEW resources. Existing resources, their
	// deploys, certificates and backups are untouched.
	if err := assertBillingNotCappedTx(ctx, tx, orgID, time.Now()); err != nil {
		return Resource{}, err
	}

	var projectID, envName string
	var envProduction bool
	if err := tx.QueryRow(ctx,
		`SELECT project_id, name, production FROM environments WHERE org_id = $1 AND id = $2`,
		orgID, in.EnvironmentID).Scan(&projectID, &envName, &envProduction); errors.Is(err, pgx.ErrNoRows) {
		return Resource{}, ErrNotFound
	} else if err != nil {
		return Resource{}, err
	}

	// Cluster deploy: the workload is scheduled by Kubernetes, so it has no
	// server of its own and the server-type matrix does not apply. What DOES
	// apply is clusterExcludedKinds — the kinds this control plane will not run
	// under a scheduler, each for the reason documented beside it.
	if in.ClusterID != "" {
		if in.ServerID != "" {
			return Resource{}, ErrInvalid{Msg: "a resource targets either a server or a cluster, not both"}
		}
		if !ClusterKindAllowed(in.Kind) {
			return Resource{}, ErrKindNotClusterable{Kind: in.Kind}
		}
		var clusterEnv string
		if err := tx.QueryRow(ctx,
			`SELECT environment_id FROM clusters WHERE org_id = $1 AND id = $2 FOR SHARE`,
			orgID, in.ClusterID).Scan(&clusterEnv); errors.Is(err, pgx.ErrNoRows) {
			return Resource{}, ErrNotFound
		} else if err != nil {
			return Resource{}, err
		}
		if clusterEnv != in.EnvironmentID {
			return Resource{}, ErrInvalid{Msg: "that cluster belongs to a different environment"}
		}
		// There is deliberately no GPU fit check here. `llm` is the only kind it
		// could apply to and clusterExcludedKinds refuses that kind above, so a
		// second copy of the arithmetic on this branch would be a check no create
		// can reach — and unreachable code that tests still cover reads as a
		// supported path, which is how the cluster-llm hole gets re-opened.
	} else if in.ServerID == "" {
		return Resource{}, ErrInvalid{Msg: "a target server or cluster is required"}
	}

	var serverType, serverStatus, serverName string
	var serverFacts json.RawMessage
	// Server placement checks. A cluster deploy has no server of its own — the
	// scheduler picks the node — so the type matrix and env attachment are the
	// cluster's concern, already validated above.
	if in.ClusterID == "" {
		// FOR SHARE locks the server row so a concurrent DeleteServer (FOR UPDATE)
		// cannot tombstone it between this liveness check and the resource insert —
		// the two serialize, so the resource either blocks the delete or is rejected
		// against an already-tombstoned server (SIGMA-132).
		// facts and name ride along for the SIGMA-214 fit check below: the row is
		// already locked and read here, so asking for them costs nothing and
		// saves a second query that could observe a different row.
		if err := tx.QueryRow(ctx,
			`SELECT type, status, name, facts FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL FOR SHARE`,
			orgID, in.ServerID).Scan(&serverType, &serverStatus, &serverName, &serverFacts); errors.Is(err, pgx.ErrNoRows) {
			return Resource{}, ErrNotFound
		} else if err != nil {
			return Resource{}, err
		}

		// A host the enrollment gate refused is compatible with the matrix on
		// paper and not in fact — that is the whole finding. Scheduling onto it
		// anyway reproduces the exact failure SIGMA-203 exists to move earlier:
		// the deploy is accepted, the container starts on hardware that cannot
		// run it, and the operator debugs a rollout instead of reading a
		// sentence about their server (SIGMA-203).
		if serverStatus == ServerStatusIncompatible {
			return Resource{}, ErrInvalid{Msg: fmt.Sprintf(
				"that server is marked incompatible with its %q type — change its type or disconnect it before scheduling work onto it",
				serverType)}
		}
		// Nor onto a machine that is being torn down. Its document is a single
		// uninstall op, so a resource created here would never be rendered
		// anywhere — and worse, it would re-arm the bound-resources 409 and
		// block the completion of a decommission already in flight (SIGMA-204).
		if serverStatus == ServerStatusDecommissioning {
			return Resource{}, ErrInvalid{Msg: "that server is being decommissioned — pick another host"}
		}

		ok := false
		for _, t := range allowed {
			if t == serverType {
				ok = true
				break
			}
		}
		if !ok {
			return Resource{}, ErrInvalid{Msg: fmt.Sprintf(
				"resource kind %q cannot run on a %q server; allowed server types: %s",
				in.Kind, serverType, strings.Join(allowed, ", "))}
		}

		var attached bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM env_servers WHERE org_id = $1 AND environment_id = $2 AND server_id = $3)`,
			orgID, in.EnvironmentID, in.ServerID).Scan(&attached); err != nil {
			return Resource{}, err
		}
		if !attached {
			return Resource{}, ErrInvalid{Msg: "server is not attached to the target environment"}
		}

		// The model checks come last of all the placement checks, and in this
		// order. checkModelServable is certain — the Hub said this repository is a
		// format or a task no runtime we render can serve — while checkModelFits
		// is an ESTIMATE, so it speaks last of everything. An operator whose
		// server is also incompatible and also unattached should be told those,
		// not handed a VRAM arithmetic lesson; and one who picked a GGUF repo
		// should be told THAT rather than an estimate about a file vLLM will never
		// open.
		if IsLLMKind(in.Kind) {
			model := parseLLMSpec(in.Spec).Model
			if err := checkModelServable(model, modelSize); err != nil {
				return Resource{}, err
			}
			gpu := ParseHostFacts(serverFacts).GPU
			var perGPU uint64
			if gpu != nil {
				perGPU = gpu.VRAMBytesPerGPU
			}
			if err := checkModelFits(model, modelSize, perGPU, serverName); err != nil {
				return Resource{}, err
			}
		}
	}

	r := Resource{ID: newID("res")}
	err = tx.QueryRow(ctx, `
		INSERT INTO resources (id, org_id, project_id, environment_id, server_id, name, kind, spec, cluster_id)
		VALUES ($1, $2, $3, $4, NULLIF($5,''), $6, $7, $8, NULLIF($9,''))
		RETURNING id, org_id, project_id, environment_id, COALESCE(server_id,''), name, kind, spec, status, created_at, updated_at`,
		r.ID, orgID, projectID, in.EnvironmentID, in.ServerID, in.Name, in.Kind, normalizeFacts(in.Spec), in.ClusterID,
	).Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.EnvironmentID, &r.ServerID, &r.Name, &r.Kind,
		&r.Spec, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if isUniqueViolation(err) {
		return Resource{}, fmt.Errorf("%w: a resource named %q already exists in %s", ErrConflict, in.Name, envName)
	}
	if err != nil {
		return Resource{}, fmt.Errorf("insert resource: %w", err)
	}
	// P1-10: a database resource is provisioned in the same transaction —
	// generated credentials (envelope-encrypted), mesh port allocation and the
	// default backup-policy row — so it can never exist half-provisioned.
	if IsDBKind(in.Kind) {
		if err := s.provisionDatabaseTx(ctx, tx, orgID, r, envProduction); err != nil {
			return Resource{}, err
		}
	}
	// P2-1: S3 storage provisions the same way (root credentials + mesh port),
	// minus the backup-policy row — object-store DR is out of the P1-11 path.
	// P2-2: the selected engine (gated above) is recorded on the credentials row.
	if IsLLMKind(in.Kind) {
		if err := s.provisionLLMTx(ctx, tx, orgID, r, modelSize); err != nil {
			return Resource{}, err
		}
	}
	if IsS3Kind(in.Kind) {
		if err := s.provisionS3Tx(ctx, tx, orgID, r, s3Engine); err != nil {
			return Resource{}, err
		}
	}
	if err := auditTx(ctx, tx, orgID, actor, "Resource created", in.Name+" ("+in.Kind+")"); err != nil {
		return Resource{}, err
	}
	return r, tx.Commit(ctx)
}

// ListResources returns org resources, optionally filtered by environment.
func (s *Store) ListResources(ctx context.Context, orgID, envID string) ([]Resource, error) {
	q := `SELECT id, org_id, project_id, environment_id, COALESCE(server_id,''), name, kind, spec, status, ephemeral, created_at, updated_at
	        FROM resources WHERE org_id = $1`
	args := []any{orgID}
	if envID != "" {
		q += ` AND environment_id = $2`
		args = append(args, envID)
	}
	q += ` ORDER BY created_at`
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Resource{}
	for rows.Next() {
		var r Resource
		if err := rows.Scan(&r.ID, &r.OrgID, &r.ProjectID, &r.EnvironmentID, &r.ServerID, &r.Name, &r.Kind,
			&r.Spec, &r.Status, &r.Ephemeral, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteResource removes a resource and returns the server whose DSD must be
// re-rendered: the server it was bound to, or — for a cluster-deployed resource,
// which is bound to no server at all — the cluster's control-plane node, which
// is where its Kubernetes teardown is applied (SIGMA-312).
func (s *Store) DeleteResource(ctx context.Context, orgID, resourceID, actor string) (serverID string, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Retain this database's restic repo key before the cascade drops its backup
	// policy (SIGMA-170). Deleting a resource deliberately leaves its Docker
	// volumes in place — destroying live bytes needs the two-phase confirm flow —
	// so destroying the key to its offsite copies with the same unguarded DELETE
	// was the odd one out.
	if err := archiveRepoKeysTx(ctx, tx, orgID, "resource", resourceID); err != nil {
		return "", fmt.Errorf("archive repo keys: %w", err)
	}

	var (
		name      string
		clusterID string
		spec      json.RawMessage
		ephemeral bool
	)
	err = tx.QueryRow(ctx,
		`DELETE FROM resources WHERE org_id = $1 AND id = $2
		 RETURNING name, COALESCE(server_id,''), COALESCE(cluster_id,''), spec, ephemeral`,
		orgID, resourceID).Scan(&name, &serverID, &clusterID, &spec, &ephemeral)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("delete resource: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Resource deleted", name); err != nil {
		return "", err
	}
	// Ephemeral carve-out: a preview resource's volumes are torn down without
	// interactive approval — the preview opt-in is the pre-authorisation. The
	// removal is still recorded as a pending destructive op and audited as the
	// system actor (both request and confirm phases), so the audit trail is
	// uniform with the interactive path.
	if ephemeral {
		for _, vol := range resourceVolumeNames(resourceID, spec) {
			if _, err := insertPendingDestructiveOpTx(ctx, tx, orgID, serverID, dsd.KindVolumeRemove, vol, "system"); err != nil {
				return "", err
			}
			if err := auditTx(ctx, tx, orgID, "system", "Destructive-op confirm requested (ephemeral)", dsd.KindVolumeRemove+" "+vol); err != nil {
				return "", err
			}
			if err := auditTx(ctx, tx, orgID, "system", "Destructive-op confirmed (ephemeral)", dsd.KindVolumeRemove+" "+vol); err != nil {
				return "", err
			}
		}
	}
	// A cluster workload has no server, so deleting the row used to be the whole
	// story: the reconciler simply stopped rendering it, k3s kept applying the
	// manifest it already had, and the Deployment, Service and Ingress went on
	// serving the attached domain forever (SIGMA-312). Queue the manifest
	// teardown on the control-plane node and re-render THAT server. Last, so the
	// ephemeral volume ops above are still addressed to the resource's own
	// (empty, for a cluster workload) server rather than to the control plane.
	if clusterID != "" {
		cpServer, terr := insertK8sTeardownTx(ctx, tx, orgID, clusterID, resourceID, spec)
		if terr != nil {
			return "", terr
		}
		if cpServer != "" {
			serverID = cpServer
		}
	}
	return serverID, tx.Commit(ctx)
}

// ── Server attributes ───────────────────────────────────────────────────────

// SetProxyRole flips a server's proxy role, audited.
func (s *Store) SetProxyRole(ctx context.Context, orgID, serverID string, proxy bool, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var name string
	err = tx.QueryRow(ctx, `
		UPDATE servers SET proxy_role = $3
		 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
		 RETURNING name`, orgID, serverID, proxy).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("set proxy role: %w", err)
	}
	action := "Proxy role cleared"
	if proxy {
		action = "Proxy role set"
	}
	if err := auditTx(ctx, tx, orgID, actor, action, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetServerType re-files a server under a different type and re-runs the
// compatibility gate against the facts already on record (SIGMA-203).
//
// This is one of the two exits from `incompatible`, and the one that keeps the
// machine: an operator who connected their ordinary box as a GPU server has a
// perfectly good general server, and the product's answer to that must not be
// "disconnect it and start again". Because the verdict is recomputed here from
// the stored facts, the change takes effect immediately — the operator does not
// wait 30 seconds for a heartbeat to find out whether the new type sticks.
//
// It is not restricted to incompatible servers: re-filing a running host is the
// same operation, and the gate is what makes it safe. What DOES block it is
// hosted resources the new type cannot run — a Postgres on a host being re-filed
// as `storage` would become unschedulable where it sits, which is the failure
// this whole area exists to prevent, so it is refused with the names.
func (s *Store) SetServerType(ctx context.Context, orgID, serverID, newType, actor string) error {
	if !IsServerType(newType) {
		return ErrInvalid{Msg: fmt.Sprintf("unknown server type %q; expected one of %s",
			newType, strings.Join(ServerTypes(), ", "))}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var name, oldType, status string
	var facts json.RawMessage
	var lastSeenAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT name, type, status, facts, last_seen_at FROM servers
		 WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL
		 FOR UPDATE`, orgID, serverID).Scan(&name, &oldType, &status, &facts, &lastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load server: %w", err)
	}

	// A machine being torn down is not re-filed. The type decides what the
	// reconciler renders and what the host is billed as, and both questions are
	// already settled for a server whose only remaining document is the
	// uninstall op (SIGMA-204). Cancelling a decommission is not a thing the
	// product offers, so silently letting a type edit sit on the row would leave
	// the operator with a machine that reads as one type and dies as another.
	if status == ServerStatusDecommissioning {
		return fmt.Errorf("%w: %s is being decommissioned — its type can no longer be changed", ErrConflict, name)
	}

	// Membership is the cluster's to end, not a type edit's — see
	// clusterMembershipTx for what waving this through cost.
	if cluster, err := clusterMembershipTx(ctx, tx, orgID, serverID); err != nil {
		return err
	} else if cluster != "" {
		return fmt.Errorf("%w: this server is a node of the %s cluster — remove it from the cluster before changing its type",
			ErrConflict, cluster)
	}

	rows, err := tx.Query(ctx,
		`SELECT name, kind FROM resources WHERE org_id = $1 AND server_id = $2 ORDER BY name`, orgID, serverID)
	if err != nil {
		return fmt.Errorf("list hosted resources: %w", err)
	}
	var stranded []string
	for rows.Next() {
		var rn, kind string
		if err := rows.Scan(&rn, &kind); err != nil {
			rows.Close()
			return err
		}
		if !CanHost(newType, kind) {
			stranded = append(stranded, fmt.Sprintf("%s (%s)", rn, ResourceKindLabel(kind)))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(stranded) > 0 {
		return fmt.Errorf("%w: a %s server cannot host %s — move or delete %s first",
			ErrConflict, newType, joinOr(stranded), pluralize(len(stranded), "it", "them"))
	}

	if _, err := tx.Exec(ctx, `UPDATE servers SET type = $2 WHERE id = $1`, serverID, newType); err != nil {
		return fmt.Errorf("set server type: %w", err)
	}
	fails := CheckServerCompatibility(newType, ParseHostFacts(facts))
	if err := writeCompatibilityTx(ctx, tx, serverID,
		statusAfterTypeChange(status, fails, lastSeenAt, DefaultStaleAfter), fails); err != nil {
		return err
	}
	if oldType != newType {
		if err := auditTx(ctx, tx, orgID, actor,
			fmt.Sprintf("Server type changed from %s to %s", oldType, newType), name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// RenameServer gives a server an operator-chosen name, permanently taking it
// out of the machine's hands: the connect form no longer asks for one and
// registration fills it from the reported hostname, so name_auto must be
// cleared here or the next registration of the same record would overwrite the
// choice (SIGMA-202).
func (s *Store) RenameServer(ctx context.Context, orgID, serverID, name, actor string) error {
	name = sanitizeServerName(name)
	if name == "" {
		return ErrInvalid{Msg: "a server name is required"}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var oldName string
	err = tx.QueryRow(ctx, `
		UPDATE servers SET name = $3, name_auto = FALSE
		  FROM servers old
		 WHERE servers.id = old.id AND servers.org_id = $1 AND servers.id = $2 AND servers.deleted_at IS NULL
		 RETURNING old.name`, orgID, serverID, name).Scan(&oldName)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("rename server: %w", err)
	}
	if oldName == name {
		return tx.Commit(ctx)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Server renamed to "+name, oldName); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ── Audit read + shared helper ──────────────────────────────────────────────

type AuditEntry struct {
	ID        int64     `json:"id"`
	OrgID     string    `json:"orgId"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	CreatedAt time.Time `json:"createdAt"`
}

// ListAudit returns the org's newest audit rows, newest first.
func (s *Store) ListAudit(ctx context.Context, orgID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, actor, action, target, created_at
		  FROM cp_audit_log WHERE org_id = $1
		 ORDER BY id DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.OrgID, &a.Actor, &a.Action, &a.Target, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func auditTx(ctx context.Context, tx pgx.Tx, orgID, actor, action, target string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		VALUES ($1, $2, $3, $4)`, orgID, actor, action, target); err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	return nil
}
