package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
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

// resourceServerTypes is the availability matrix: which server types may host
// which resource kind. Enforced in the store, not just the UI.
//
// The rules behind it, so a future edit is a decision rather than a guess:
//   - "vps" is a general-purpose host that happens to be virtualized. It hosts
//     whatever a general server hosts; the difference is disclosure (shared
//     tenancy, burst CPU, no nested virt), not capability.
//   - "k8s" nodes host nothing directly. Their workloads arrive through the
//     cluster's control plane, so aiming a resource at a node individually is
//     always a mistake and is refused rather than quietly scheduled.
//   - "build" servers compile images and ship them to a registry; they run no
//     long-lived workloads of their own.
//   - "llm" needs a GPU. Serving a model on CPU is technically possible and
//     practically useless, so it is not offered.
var resourceServerTypes = map[string][]string{
	"app":      {"general", "vps", "gpu"},
	"postgres": {"database", "general", "vps"},
	"mysql":    {"database", "general", "vps"},
	"redis":    {"database", "general", "vps"},
	"mongodb":  {"database", "general", "vps"},
	"s3":       {"storage"},
	"llm":      {"gpu"},
}

// serverTypes is every type a server may declare. A type outside this set is a
// typo, and typos must fail at enrollment rather than producing a host nothing
// can be scheduled onto.
var serverTypes = map[string]bool{
	"general":  true,
	"vps":      true,
	"database": true,
	"storage":  true,
	"gpu":      true,
	"k8s":      true,
	"build":    true,
}

// IsServerType reports whether a server type is known.
func IsServerType(t string) bool { return serverTypes[t] }

// ServerTypes lists the known server types, for the API to publish.
func ServerTypes() []string {
	out := make([]string, 0, len(serverTypes))
	for t := range serverTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// AllowedServerTypes returns the server types a resource kind may run on,
// or nil for an unknown kind.
func AllowedServerTypes(kind string) []string { return resourceServerTypes[kind] }

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
// stay on disk) and returns the distinct servers whose DSD must re-render.
// MUST run BEFORE the DELETE that triggers the cascade — afterwards there is
// nothing left to read. scopeCol is an internal constant, never user input.
func cascadeResourceCleanupTx(ctx context.Context, tx pgx.Tx, orgID, scopeCol, scopeID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, server_id, spec, ephemeral FROM resources WHERE org_id = $1 AND `+scopeCol+` = $2`,
		orgID, scopeID)
	if err != nil {
		return nil, err
	}
	type res struct {
		id, serverID string
		spec         json.RawMessage
		ephemeral    bool
	}
	var all []res
	for rows.Next() {
		var r res
		if err := rows.Scan(&r.id, &r.serverID, &r.spec, &r.ephemeral); err != nil {
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

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Resource{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

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
	// apply is the stateful-kind exclusion — a database rescheduled onto a node
	// without its data is data loss, so it must live on its own server.
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
	} else if in.ServerID == "" {
		return Resource{}, ErrInvalid{Msg: "a target server or cluster is required"}
	}

	var serverType string
	// Server placement checks. A cluster deploy has no server of its own — the
	// scheduler picks the node — so the type matrix and env attachment are the
	// cluster's concern, already validated above.
	if in.ClusterID == "" {
		// FOR SHARE locks the server row so a concurrent DeleteServer (FOR UPDATE)
		// cannot tombstone it between this liveness check and the resource insert —
		// the two serialize, so the resource either blocks the delete or is rejected
		// against an already-tombstoned server (SIGMA-132).
		if err := tx.QueryRow(ctx,
			`SELECT type FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL FOR SHARE`,
			orgID, in.ServerID).Scan(&serverType); errors.Is(err, pgx.ErrNoRows) {
			return Resource{}, ErrNotFound
		} else if err != nil {
			return Resource{}, err
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
	}

	r := Resource{ID: newID("res")}
	err = tx.QueryRow(ctx, `
		INSERT INTO resources (id, org_id, project_id, environment_id, server_id, name, kind, spec, cluster_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,''))
		RETURNING id, org_id, project_id, environment_id, server_id, name, kind, spec, status, created_at, updated_at`,
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
		if err := s.provisionLLMTx(ctx, tx, orgID, r); err != nil {
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
	q := `SELECT id, org_id, project_id, environment_id, server_id, name, kind, spec, status, ephemeral, created_at, updated_at
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

// DeleteResource removes a resource and returns the server it was bound to, so
// the caller can re-render that server's DSD.
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
		spec      json.RawMessage
		ephemeral bool
	)
	err = tx.QueryRow(ctx,
		`DELETE FROM resources WHERE org_id = $1 AND id = $2 RETURNING name, server_id, spec, ephemeral`,
		orgID, resourceID).Scan(&name, &serverID, &spec, &ephemeral)
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
