package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// WebhookActor is the synthetic audit actor for provider-driven webhook events.
// Webhooks are unauthenticated (verified only by HMAC signature), so there is no
// service principal to attribute the row to.
const WebhookActor = "github-webhook"

// GitConnection links a provider repository to a project. The provider token
// (a GitHub App installation token, or a PAT) is held only in KMS-wrapped form.
type GitConnection struct {
	ID             string    `json:"id"`
	OrgID          string    `json:"orgId"`
	ProjectID      string    `json:"projectId"`
	Provider       string    `json:"provider"`
	InstallationID string    `json:"installationId"`
	RepoFullName   string    `json:"repoFullName"`
	CreatedBy      string    `json:"createdBy"`
	CreatedAt      time.Time `json:"createdAt"`
	// Previews (P1-12): PR events spawn ephemeral environments on the
	// designated preview server when enabled.
	PreviewsEnabled bool   `json:"previewsEnabled"`
	PreviewServerID string `json:"previewServerId,omitempty"`
}

// BranchMap routes pushes on one branch to one environment under a deploy policy.
type BranchMap struct {
	ID            string     `json:"id"`
	ConnectionID  string     `json:"connectionId"`
	Branch        string     `json:"branch"`
	EnvironmentID string     `json:"environmentId"`
	Policy        string     `json:"policy"` // "auto" | "manual"
	LastRef       string     `json:"lastRef,omitempty"`
	LastSHA       string     `json:"lastSha,omitempty"`
	LastPushedAt  *time.Time `json:"lastPushedAt,omitempty"`
	// BuildServerID names a dedicated server to build on. Empty means "build on
	// the deploy target", which is the existing behaviour. Building saturates
	// CPU and disk for minutes; on a busy host that lands on the machine serving
	// traffic, so this lets the build move off it.
	BuildServerID string    `json:"buildServerId,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// DeployRequest is an enqueued deploy (drained by P1-9) or a recorded PR routing
// hook (kind='pr_hook', which carries no deploy semantics — that is P1-12).
type DeployRequest struct {
	ID            string    `json:"id"`
	OrgID         string    `json:"orgId"`
	ConnectionID  string    `json:"connectionId"`
	EnvironmentID string    `json:"environmentId,omitempty"`
	Kind          string    `json:"kind"` // "deploy" | "pr_hook"
	Ref           string    `json:"ref"`
	SHA           string    `json:"sha"`
	Branch        string    `json:"branch,omitempty"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
}

// CreateGitConnectionInput is the payload for connecting a repo to a project.
type CreateGitConnectionInput struct {
	ProjectID      string
	Provider       string // defaults to "github"
	InstallationID string
	RepoFullName   string // owner/name
	Token          string // provider token, stored KMS-wrapped (may be empty)
	// AutoConnected marks a connection derived from the org's GitHub integration
	// (the repo picker) rather than assembled by hand in the Git panel.
	AutoConnected bool
}

// GitWebhookEvent is the provider-agnostic, already-signature-verified shape the
// HTTP layer hands to the store after parsing a delivery.
type GitWebhookEvent struct {
	DeliveryID   string
	Provider     string
	EventType    string // "push" | "pull_request" | ...
	RepoFullName string
	Ref          string // refs/heads/<branch> for push
	SHA          string
	Branch       string // extracted branch (push head, or PR head ref)
	Action       string // pull_request action (opened|synchronize|closed|...)
	PRNumber     int    // pull_request number (0 for non-PR events)
	Deleted      bool   // push that deleted the branch — never deploys
	// InstallationID is the GitHub App installation the delivery was sent for.
	// With repo uniqueness org-scoped (SIGMA-174) the repo name alone no longer
	// identifies a tenant, so this is the primary routing key.
	InstallationID string
	// PushedAt is the head commit's timestamp for a push (nil if unavailable).
	// Used to reject out-of-order webhook deliveries (SIGMA-136).
	PushedAt *time.Time
}

// WebhookOutcome reports what a delivery did, so the HTTP layer can shape its
// (always 2xx) acknowledgement and tests can assert routing.
type WebhookOutcome struct {
	Duplicate bool // redelivered id — no-op
	// Ambiguous: several orgs have this repo connected and the delivery carried
	// no installation binding to pick one, so it was dropped unrouted rather
	// than guessed into a foreign tenant (SIGMA-174).
	Ambiguous  bool
	Connection *GitConnection // nil when the repo is not connected
	Enqueued   *DeployRequest // set when an auto-deploy was enqueued
	PRHook     *DeployRequest // set when a pull_request routing hook was recorded
	// Previews (P1-12): PreviewDeploy is the enqueued preview deploy request;
	// PreviewTeardown is the server whose DSD must re-render after a close.
	PreviewDeploy   *DeployRequest
	PreviewTeardown *ServerRef
}

// gitTokenPurpose namespaces the provider-token wrapping key per org.
func gitTokenPurpose(orgID string) string { return "git_token:" + orgID }

func normalizeRepo(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// branchFromRef strips refs/heads/ from a push ref; other ref namespaces (tags)
// return "" so they never match a branch map.
func branchFromRef(ref string) string {
	const p = "refs/heads/"
	if strings.HasPrefix(ref, p) {
		return ref[len(p):]
	}
	return ""
}

// CreateGitConnection connects a repo to a project. The project must belong to
// the org; the repo may drive at most one connection (unique per provider). The
// provider token is KMS-wrapped before storage; the plaintext never persists.
func (s *Store) CreateGitConnection(ctx context.Context, orgID string, in CreateGitConnectionInput, actor string) (GitConnection, error) {
	provider := in.Provider
	if provider == "" {
		provider = "github"
	}
	repo := strings.TrimSpace(in.RepoFullName)
	if repo == "" || !strings.Contains(repo, "/") {
		return GitConnection{}, ErrInvalid{Msg: "repoFullName must be owner/name"}
	}

	var wrapped []byte
	if in.Token != "" {
		w, err := s.custody.Wrap(ctx, gitTokenPurpose(orgID), []byte(in.Token))
		if err != nil {
			return GitConnection{}, fmt.Errorf("wrap provider token: %w", err)
		}
		wrapped = w
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return GitConnection{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The project must belong to the org (tenant isolation).
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM projects WHERE org_id = $1 AND id = $2)`,
		orgID, in.ProjectID).Scan(&exists); err != nil {
		return GitConnection{}, err
	}
	if !exists {
		return GitConnection{}, ErrNotFound
	}

	c := GitConnection{
		ID: newID("gcn"), OrgID: orgID, ProjectID: in.ProjectID, Provider: provider,
		InstallationID: in.InstallationID, RepoFullName: repo, CreatedBy: actor,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO git_connections (id, org_id, project_id, provider, installation_id, repo_full_name, token_wrapped, created_by, auto_connected)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING created_at`,
		c.ID, c.OrgID, c.ProjectID, c.Provider, c.InstallationID, c.RepoFullName, wrapped, c.CreatedBy, in.AutoConnected).Scan(&c.CreatedAt)
	if isUniqueViolation(err) {
		// Uniqueness is org-scoped (SIGMA-174), so this can only be the caller's
		// own org's connection — the message discloses nothing cross-tenant.
		return GitConnection{}, fmt.Errorf("%w: repository %q is already connected in this organization", ErrConflict, repo)
	}
	if err != nil {
		return GitConnection{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Git repo connected", repo); err != nil {
		return GitConnection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return GitConnection{}, err
	}
	return c, nil
}

// SetConnectionInstallation links a GitHub App installation to an existing
// connection (SIGMA-55: the post-install callback lands here). From then on
// clone credentials and repo inspection prefer short-lived installation
// tokens over the stored PAT.
func (s *Store) SetConnectionInstallation(ctx context.Context, orgID, connID, installationID, actor string) error {
	installationID = strings.TrimSpace(installationID)
	if installationID == "" || !isDigits(installationID) {
		return ErrInvalid{Msg: "installationId must be a numeric GitHub installation id"}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE git_connections SET installation_id = $3
		 WHERE org_id = $1 AND id = $2`, orgID, connID, installationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := auditTx(ctx, tx, orgID, actor, "GitHub App installation linked", connID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ClaimInstallation binds a GitHub App installation id to the org that first
// presents it (first-writer-wins). Returns ErrNotFound if the installation is
// already bound to a DIFFERENT org, so a client-supplied installationId can't
// drive the CP to mint a token for an installation another org owns (SIGMA-87).
// Opaque (surfaces as 404, not 403) so it never confirms another org's
// installation exists. Idempotent for the owning org.
func (s *Store) ClaimInstallation(ctx context.Context, orgID, installationID string) error {
	installationID = strings.TrimSpace(installationID)
	if installationID == "" || !isDigits(installationID) {
		return ErrInvalid{Msg: "installationId must be a numeric GitHub installation id"}
	}
	var owner string
	if err := s.Pool.QueryRow(ctx, `
		INSERT INTO github_installations (installation_id, org_id) VALUES ($1, $2)
		ON CONFLICT (installation_id)
		  DO UPDATE SET installation_id = github_installations.installation_id
		RETURNING org_id`, installationID, orgID).Scan(&owner); err != nil {
		return err
	}
	if owner != orgID {
		return ErrNotFound
	}
	return nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ListGitConnections returns the org's connections, optionally filtered to a
// project (pass "" for all).
func (s *Store) ListGitConnections(ctx context.Context, orgID, projectID string) ([]GitConnection, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, project_id, provider, installation_id, repo_full_name, created_by, created_at, previews_enabled, COALESCE(preview_server_id, '')
		  FROM git_connections
		 WHERE org_id = $1 AND ($2 = '' OR project_id = $2)
		 ORDER BY created_at DESC`, orgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitConnection{}
	for rows.Next() {
		var c GitConnection
		if err := rows.Scan(&c.ID, &c.OrgID, &c.ProjectID, &c.Provider, &c.InstallationID, &c.RepoFullName, &c.CreatedBy, &c.CreatedAt, &c.PreviewsEnabled, &c.PreviewServerID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetGitConnection returns one org-scoped connection.
func (s *Store) GetGitConnection(ctx context.Context, orgID, connID string) (GitConnection, error) {
	var c GitConnection
	err := s.Pool.QueryRow(ctx, `
		SELECT id, org_id, project_id, provider, installation_id, repo_full_name, created_by, created_at, previews_enabled, COALESCE(preview_server_id, '')
		  FROM git_connections WHERE org_id = $1 AND id = $2`, orgID, connID).Scan(
		&c.ID, &c.OrgID, &c.ProjectID, &c.Provider, &c.InstallationID, &c.RepoFullName, &c.CreatedBy, &c.CreatedAt, &c.PreviewsEnabled, &c.PreviewServerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitConnection{}, ErrNotFound
	}
	return c, err
}

// DeleteGitConnection removes a connection (cascading its branch maps).
//
// It REFUSES while any resource is still deployed from this connection.
// deployments.connection_id is ON DELETE SET NULL and DeployTargetsForServer
// INNER JOINs git_connections, so dropping the row silently removes those
// resources' deploy targets; the reconciler then renders them as the no-op
// resource.sync stub, and the agent's GC — whose keep-set is built from the
// document's container/rollout ops — force-removes the RUNNING production
// container. Redeploy can't recover it either, because CreateManualRedeploy
// copies the now-NULL connection_id forward (SIGMA-159). Disconnecting is meant
// to stop future deploys, not to tear down running apps, so the caller must
// remove or re-home the resources first.
func (s *Store) DeleteGitConnection(ctx context.Context, orgID, connID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resources whose current (non-superseded) deployment came from this
	// connection are the ones that would be reaped.
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT r.name
		  FROM deployments d
		  JOIN resources r ON r.id = d.resource_id
		 WHERE d.connection_id = $1
		   AND d.status IN ('queued','building','deploying','success')
		 ORDER BY r.name`, connID)
	if err != nil {
		return fmt.Errorf("list deployed resources: %w", err)
	}
	var deployed []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		deployed = append(deployed, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(deployed) > 0 {
		return fmt.Errorf("%w: %d resource(s) are still deployed from this repo: %s — remove them first",
			ErrConflict, len(deployed), strings.Join(deployed, ", "))
	}

	var repo string
	err = tx.QueryRow(ctx,
		`DELETE FROM git_connections WHERE org_id = $1 AND id = $2 RETURNING repo_full_name`,
		orgID, connID).Scan(&repo)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Git repo disconnected", repo); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetBranchMap upserts a branch→environment route with a deploy policy. The
// connection and environment must both belong to the org.
func (s *Store) SetBranchMap(ctx context.Context, orgID, connID, branch, envID, policy, buildServerID, actor string) (BranchMap, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return BranchMap{}, ErrInvalid{Msg: "branch is required"}
	}
	if policy != "auto" && policy != "manual" {
		return BranchMap{}, ErrInvalid{Msg: `policy must be "auto" or "manual"`}
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BranchMap{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var connExists, envExists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM git_connections WHERE org_id = $1 AND id = $2)`,
		orgID, connID).Scan(&connExists); err != nil {
		return BranchMap{}, err
	}
	if !connExists {
		return BranchMap{}, ErrNotFound
	}
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM environments WHERE org_id = $1 AND id = $2)`,
		orgID, envID).Scan(&envExists); err != nil {
		return BranchMap{}, err
	}
	if !envExists {
		return BranchMap{}, ErrInvalid{Msg: "environment does not belong to this org"}
	}
	buildServerID = strings.TrimSpace(buildServerID)
	if buildServerID != "" {
		// The build server must be this org's, or a mapping could send a clone
		// (with its credential) to another tenant's host.
		if err := assertServerInOrg(ctx, tx, orgID, buildServerID); err != nil {
			return BranchMap{}, err
		}
	}

	m, err := scanBranchMap(tx.QueryRow(ctx, `
		INSERT INTO git_branch_map (id, connection_id, branch, environment_id, policy, build_server_id)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))
		ON CONFLICT (connection_id, branch)
		DO UPDATE SET environment_id = EXCLUDED.environment_id, policy = EXCLUDED.policy,
		              build_server_id = EXCLUDED.build_server_id
		RETURNING id, connection_id, branch, environment_id, policy, last_ref, last_sha, last_pushed_at, build_server_id, created_at`,
		newID("gbm"), connID, branch, envID, policy, buildServerID))
	if err != nil {
		return BranchMap{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Git branch mapped ("+branch+" → "+policy+")", envID); err != nil {
		return BranchMap{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BranchMap{}, err
	}
	return m, nil
}

// ListBranchMaps returns a connection's branch routes (org-scoped).
func (s *Store) ListBranchMaps(ctx context.Context, orgID, connID string) ([]BranchMap, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT m.id, m.connection_id, m.branch, m.environment_id, m.policy, m.last_ref, m.last_sha, m.last_pushed_at, m.created_at
		  FROM git_branch_map m
		  JOIN git_connections c ON c.id = m.connection_id
		 WHERE c.org_id = $1 AND m.connection_id = $2
		 ORDER BY m.branch`, orgID, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BranchMap{}
	for rows.Next() {
		m, err := scanBranchMap(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteBranchMap removes a branch route (org-scoped via the connection join).
func (s *Store) DeleteBranchMap(ctx context.Context, orgID, mapID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var branch string
	err = tx.QueryRow(ctx, `
		DELETE FROM git_branch_map m
		 USING git_connections c
		 WHERE m.connection_id = c.id AND c.org_id = $1 AND m.id = $2
		 RETURNING m.branch`, orgID, mapID).Scan(&branch)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Git branch unmapped", branch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PromoteBranch enqueues a deploy of a manual branch's last-seen commit. It is
// the "promote" half of the manual policy: the push recorded the commit but
// enqueued nothing; this turns that remembered sha into a deploy request.
func (s *Store) PromoteBranch(ctx context.Context, orgID, mapID, actor string) (DeployRequest, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return DeployRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var connID, envID, branch string
	var lastRef, lastSHA *string
	err = tx.QueryRow(ctx, `
		SELECT m.connection_id, m.environment_id, m.branch, m.last_ref, m.last_sha
		  FROM git_branch_map m
		  JOIN git_connections c ON c.id = m.connection_id
		 WHERE c.org_id = $1 AND m.id = $2`, orgID, mapID).Scan(&connID, &envID, &branch, &lastRef, &lastSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployRequest{}, ErrNotFound
	}
	if err != nil {
		return DeployRequest{}, err
	}
	if lastSHA == nil || *lastSHA == "" {
		return DeployRequest{}, ErrInvalid{Msg: "no push has been recorded on this branch to promote"}
	}
	ref := ""
	if lastRef != nil {
		ref = *lastRef
	}
	dr, err := enqueueDeployTx(ctx, tx, orgID, connID, envID, ref, *lastSHA, branch)
	if err != nil {
		return DeployRequest{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Git deploy promoted ("+branch+")", envID); err != nil {
		return DeployRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeployRequest{}, err
	}
	return dr, nil
}

// EnqueueBranchDeploy enqueues a deploy of a KNOWN commit on a mapped branch —
// the initial-deploy path: the head sha was just fetched from the provider, so
// the first build doesn't have to wait for a webhook push. Records the sha on
// the map (so the UI and Promote see it) and audits.
func (s *Store) EnqueueBranchDeploy(ctx context.Context, orgID, mapID, ref, sha, actor string) (DeployRequest, error) {
	if strings.TrimSpace(sha) == "" {
		return DeployRequest{}, ErrInvalid{Msg: "commit sha is required"}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return DeployRequest{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var connID, envID, branch string
	err = tx.QueryRow(ctx, `
		SELECT m.connection_id, m.environment_id, m.branch
		  FROM git_branch_map m
		  JOIN git_connections c ON c.id = m.connection_id
		 WHERE c.org_id = $1 AND m.id = $2`, orgID, mapID).Scan(&connID, &envID, &branch)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeployRequest{}, ErrNotFound
	}
	if err != nil {
		return DeployRequest{}, err
	}
	if ref == "" {
		ref = "refs/heads/" + branch
	}
	if _, err := tx.Exec(ctx, `
		UPDATE git_branch_map SET last_ref = $1, last_sha = $2, last_pushed_at = now() WHERE id = $3`,
		ref, sha, mapID); err != nil {
		return DeployRequest{}, err
	}
	dr, err := enqueueDeployTx(ctx, tx, orgID, connID, envID, ref, sha, branch)
	if err != nil {
		return DeployRequest{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Git initial deploy enqueued ("+branch+")", envID); err != nil {
		return DeployRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return DeployRequest{}, err
	}
	return dr, nil
}

// HandleGitWebhook processes one already-signature-verified delivery atomically:
// it dedupes on the provider delivery id (a redelivery is a no-op), resolves the
// repo to a connection, and — for a push on an 'auto' mapped branch — enqueues
// exactly one deploy request. 'manual' branches record the commit but enqueue
// nothing; pull_request events persist a routing hook with no deploy. Every
// delivery that maps to a connection writes an audit row.
func (s *Store) HandleGitWebhook(ctx context.Context, ev GitWebhookEvent) (WebhookOutcome, error) {
	provider := ev.Provider
	if provider == "" {
		provider = "github"
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return WebhookOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Idempotency: the delivery id is the dedup key. A redelivery finds the row
	// already present (ON CONFLICT DO NOTHING → 0 rows) and short-circuits before
	// any routing, so no deploy is enqueued twice.
	tag, err := tx.Exec(ctx, `
		INSERT INTO webhook_deliveries (delivery_id, provider, event_type)
		VALUES ($1,$2,$3) ON CONFLICT (delivery_id) DO NOTHING`,
		ev.DeliveryID, provider, ev.EventType)
	if err != nil {
		return WebhookOutcome{}, err
	}
	if tag.RowsAffected() == 0 {
		return WebhookOutcome{Duplicate: true}, nil
	}

	// Resolve the connected repo, disambiguated by the delivery's installation
	// (SIGMA-174 — several orgs may hold the same repo). An unconnected repo
	// still had its delivery recorded above (keeping redeliveries idempotent);
	// an ambiguous one is dropped rather than routed into a foreign tenant.
	conn, err := gitConnectionForDeliveryTx(ctx, tx, provider, ev.RepoFullName, ev.InstallationID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, errAmbiguousDelivery) {
		if cerr := tx.Commit(ctx); cerr != nil {
			return WebhookOutcome{}, cerr
		}
		return WebhookOutcome{Ambiguous: errors.Is(err, errAmbiguousDelivery)}, nil
	}
	if err != nil {
		return WebhookOutcome{}, err
	}
	out := WebhookOutcome{Connection: &conn}

	switch ev.EventType {
	case "push":
		branch := ev.Branch
		if branch == "" {
			branch = branchFromRef(ev.Ref)
		}
		action := "Git push"
		if ev.Deleted {
			// A branch deletion never deploys; record + audit only.
			action = "Git branch deleted"
			if branch != "" {
				action = "Git branch deleted (" + branch + ")"
			}
		} else if branch != "" {
			m, err := branchMapTx(ctx, tx, conn.ID, branch)
			switch {
			case errors.Is(err, ErrNotFound):
				action = "Git push " + branch + " (branch not mapped)"
			case err != nil:
				return WebhookOutcome{}, err
			default:
				// Remember the commit on the branch map for both policies, so a
				// manual branch can be promoted later to exactly this sha. Guard
				// against out-of-order webhook deliveries: when the push carries a
				// head-commit time, only advance the map (and enqueue a deploy) if
				// this push is at least as new as the recorded one. Otherwise an
				// older commit arriving late would overwrite last_sha and its deploy
				// would supersede the newer commit's (SIGMA-136).
				var advanced bool
				if ev.PushedAt != nil {
					tag, err := tx.Exec(ctx,
						`UPDATE git_branch_map SET last_ref = $1, last_sha = $2, last_pushed_at = $4
						  WHERE id = $3 AND (last_pushed_at IS NULL OR $4 >= last_pushed_at)`,
						ev.Ref, ev.SHA, m.ID, ev.PushedAt)
					if err != nil {
						return WebhookOutcome{}, err
					}
					advanced = tag.RowsAffected() > 0
				} else {
					if _, err := tx.Exec(ctx,
						`UPDATE git_branch_map SET last_ref = $1, last_sha = $2, last_pushed_at = now() WHERE id = $3`,
						ev.Ref, ev.SHA, m.ID); err != nil {
						return WebhookOutcome{}, err
					}
					advanced = true
				}
				switch {
				case !advanced:
					action = "Git push " + branch + " (stale delivery ignored)"
				case m.Policy == "auto":
					dr, err := enqueueDeployTx(ctx, tx, conn.OrgID, conn.ID, m.EnvironmentID, ev.Ref, ev.SHA, branch)
					if err != nil {
						return WebhookOutcome{}, err
					}
					out.Enqueued = &dr
					action = "Git push " + branch + " → deploy enqueued"
				default:
					action = "Git push " + branch + " (manual — awaiting promotion)"
				}
			}
		}
		if err := auditTx(ctx, tx, conn.OrgID, WebhookActor, action, conn.RepoFullName); err != nil {
			return WebhookOutcome{}, err
		}

	case "pull_request":
		dr, err := recordPRHookTx(ctx, tx, conn.OrgID, conn.ID, ev.Ref, ev.SHA, ev.Branch)
		if err != nil {
			return WebhookOutcome{}, err
		}
		out.PRHook = &dr
		if err := auditTx(ctx, tx, conn.OrgID, WebhookActor,
			"Git pull_request "+strings.TrimSpace(ev.Action)+" ("+ev.Branch+")", conn.RepoFullName); err != nil {
			return WebhookOutcome{}, err
		}
		// Previews (P1-12): opened/synchronized PRs spawn (or redeploy) the PR's
		// ephemeral environment; a closed PR tears it down. Same-repo PRs only —
		// a fork's head SHA is not fetchable with the connection's credential.
		if conn.PreviewsEnabled && ev.PRNumber > 0 {
			switch ev.Action {
			case "opened", "reopened", "synchronize", "ready_for_review":
				envID, _, err := ensurePreviewTx(ctx, tx, conn, ev.PRNumber, ev.Branch, ev.SHA)
				if err != nil {
					return WebhookOutcome{}, err
				}
				pd, err := enqueueDeployTx(ctx, tx, conn.OrgID, conn.ID, envID, ev.Ref, ev.SHA, ev.Branch)
				if err != nil {
					return WebhookOutcome{}, err
				}
				out.PreviewDeploy = &pd
			case "closed":
				serverID, torn, err := teardownPreviewTx(ctx, tx, conn, ev.PRNumber)
				if err != nil {
					return WebhookOutcome{}, err
				}
				if torn && serverID != "" {
					out.PreviewTeardown = &ServerRef{ServerID: serverID, OrgID: conn.OrgID}
				}
			}
		}

	default:
		// Acknowledge + audit any other subscribed event; no routing.
		if err := auditTx(ctx, tx, conn.OrgID, WebhookActor, "Git webhook "+ev.EventType, conn.RepoFullName); err != nil {
			return WebhookOutcome{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return WebhookOutcome{}, err
	}
	return out, nil
}

// ListDeployRequests returns an org's deploy requests, newest first.
func (s *Store) ListDeployRequests(ctx context.Context, orgID string, limit int) ([]DeployRequest, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, org_id, connection_id, environment_id, kind, ref, sha, branch, status, created_at
		  FROM deploy_requests WHERE org_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeployRequest{}
	for rows.Next() {
		var d DeployRequest
		var env, branch *string
		if err := rows.Scan(&d.ID, &d.OrgID, &d.ConnectionID, &env, &d.Kind, &d.Ref, &d.SHA, &branch, &d.Status, &d.CreatedAt); err != nil {
			return nil, err
		}
		if env != nil {
			d.EnvironmentID = *env
		}
		if branch != nil {
			d.Branch = *branch
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// gitCloneToken resolves the credential a clone (or repo read) should use for
// a connection: a short-lived App installation token when the connection has
// an installation and a minter is configured, falling back to the stored PAT
// when minting fails (revoked installation, GitHub outage) or no App is set
// up. "" with no error means a public, credential-less repo.
func (s *Store) gitCloneToken(ctx context.Context, orgID, connID string) (string, error) {
	var installationID string
	var wrapped []byte
	err := s.Pool.QueryRow(ctx,
		`SELECT installation_id, token_wrapped FROM git_connections WHERE org_id = $1 AND id = $2`,
		orgID, connID).Scan(&installationID, &wrapped)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}

	var mintErr error
	if installationID != "" && s.installTokens != nil {
		token, err := s.installTokens.InstallationToken(ctx, installationID)
		if err == nil {
			return token, nil
		}
		mintErr = err
	}
	if len(wrapped) == 0 {
		if mintErr != nil {
			return "", fmt.Errorf("mint installation token: %w", mintErr)
		}
		return "", nil
	}
	plain, err := s.custody.Unwrap(ctx, gitTokenPurpose(orgID), wrapped)
	if err != nil {
		return "", fmt.Errorf("unwrap provider token: %w", err)
	}
	return string(plain), nil
}

// GitTokenForRepo resolves the credential to READ an org's connected repo:
// the connection's App installation token or stored PAT (same resolution as a
// clone). "" with nil error = connected public repo; ErrNotFound = nothing
// usable in this org. Lets repo detection see a private repo without the
// wizard carrying a token.
//
// Fallback: when THIS repo isn't connected, the newest connection for the
// same provider owner (e.g. any other SigmaJunction/* repo) is used — a PAT
// or App installation is owner-scoped in practice, so one connected repo
// makes its sibling repos detectable org-wide. Tokens never cross sigmahub
// orgs: every lookup is org_id-scoped.
func (s *Store) GitTokenForRepo(ctx context.Context, orgID, repoFullName string) (string, error) {
	repo := strings.Trim(strings.TrimSpace(repoFullName), "/")
	var connID string
	err := s.Pool.QueryRow(ctx,
		`SELECT id FROM git_connections WHERE org_id = $1 AND repo_full_name = $2`,
		orgID, repo).Scan(&connID)
	if errors.Is(err, pgx.ErrNoRows) {
		owner, _, _ := strings.Cut(repo, "/")
		if owner == "" {
			return "", ErrNotFound
		}
		err = s.Pool.QueryRow(ctx, `
			SELECT id FROM git_connections
			 WHERE org_id = $1 AND repo_full_name LIKE $2 || '/%'
			 ORDER BY created_at DESC LIMIT 1`,
			orgID, owner).Scan(&connID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
	}
	if err != nil {
		return "", err
	}
	return s.gitCloneToken(ctx, orgID, connID)
}

// ── tx helpers ──────────────────────────────────────────────────────────────

// errAmbiguousDelivery marks a delivery whose repo is connected in several orgs
// with no installation binding to pick one. Dropped, never guessed (SIGMA-174).
var errAmbiguousDelivery = errors.New("ambiguous webhook delivery")

const gitConnectionCols = `id, org_id, project_id, provider, installation_id, repo_full_name, created_by, created_at, previews_enabled, COALESCE(preview_server_id, '')`

func scanGitConnection(row pgx.Row) (GitConnection, error) {
	var c GitConnection
	err := row.Scan(&c.ID, &c.OrgID, &c.ProjectID, &c.Provider, &c.InstallationID, &c.RepoFullName, &c.CreatedBy, &c.CreatedAt, &c.PreviewsEnabled, &c.PreviewServerID)
	return c, err
}

// gitConnectionForDeliveryTx resolves an inbound delivery to a connection. Repo
// uniqueness is org-scoped (SIGMA-174), so the repo name alone no longer
// identifies a tenant. Resolution order:
//  1. the org bound to the delivery's installation id (github_installations,
//     the SIGMA-87 first-writer-wins ownership anchor), then that org's
//     connection for the repo;
//  2. a connection that recorded this installation id directly (created before
//     the org claimed the installation);
//  3. the unique global match — the pre-SIGMA-174 behavior, safe only while
//     exactly one org holds the repo. Two or more matches without an
//     installation binding return errAmbiguousDelivery.
func gitConnectionForDeliveryTx(ctx context.Context, tx pgx.Tx, provider, repo, installationID string) (GitConnection, error) {
	norm := normalizeRepo(repo)
	if installationID != "" {
		var orgID string
		err := tx.QueryRow(ctx,
			`SELECT org_id FROM github_installations WHERE installation_id = $1`,
			installationID).Scan(&orgID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return GitConnection{}, err
		}
		if err == nil {
			c, err := scanGitConnection(tx.QueryRow(ctx, `
				SELECT `+gitConnectionCols+` FROM git_connections
				 WHERE org_id = $1 AND provider = $2 AND lower(repo_full_name) = $3`,
				orgID, provider, norm))
			if err == nil {
				return c, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return GitConnection{}, err
			}
		}
		c, err := scanGitConnection(tx.QueryRow(ctx, `
			SELECT `+gitConnectionCols+` FROM git_connections
			 WHERE installation_id = $1 AND provider = $2 AND lower(repo_full_name) = $3`,
			installationID, provider, norm))
		if err == nil {
			return c, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return GitConnection{}, err
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT `+gitConnectionCols+` FROM git_connections
		 WHERE provider = $1 AND lower(repo_full_name) = $2 LIMIT 2`, provider, norm)
	if err != nil {
		return GitConnection{}, err
	}
	defer rows.Close()
	var conns []GitConnection
	for rows.Next() {
		c, err := scanGitConnection(rows)
		if err != nil {
			return GitConnection{}, err
		}
		conns = append(conns, c)
	}
	if err := rows.Err(); err != nil {
		return GitConnection{}, err
	}
	switch len(conns) {
	case 0:
		return GitConnection{}, ErrNotFound
	case 1:
		return conns[0], nil
	default:
		return GitConnection{}, errAmbiguousDelivery
	}
}

func branchMapTx(ctx context.Context, tx pgx.Tx, connID, branch string) (BranchMap, error) {
	m, err := scanBranchMap(tx.QueryRow(ctx, `
		SELECT id, connection_id, branch, environment_id, policy, last_ref, last_sha, last_pushed_at, build_server_id, created_at
		  FROM git_branch_map WHERE connection_id = $1 AND branch = $2`, connID, branch))
	if errors.Is(err, pgx.ErrNoRows) {
		return BranchMap{}, ErrNotFound
	}
	return m, err
}

// scanBranchMap reads a branch-map row, tolerating NULL last_ref/last_sha (a
// branch that has not yet seen a push). Works for both pgx.Row and pgx.Rows.
func scanBranchMap(row pgx.Row) (BranchMap, error) {
	var m BranchMap
	var lastRef, lastSHA, buildServer *string
	err := row.Scan(&m.ID, &m.ConnectionID, &m.Branch, &m.EnvironmentID, &m.Policy, &lastRef, &lastSHA, &m.LastPushedAt, &buildServer, &m.CreatedAt)
	if buildServer != nil {
		m.BuildServerID = *buildServer
	}
	if lastRef != nil {
		m.LastRef = *lastRef
	}
	if lastSHA != nil {
		m.LastSHA = *lastSHA
	}
	return m, err
}

func enqueueDeployTx(ctx context.Context, tx pgx.Tx, orgID, connID, envID, ref, sha, branch string) (DeployRequest, error) {
	d := DeployRequest{
		ID: newID("dpr"), OrgID: orgID, ConnectionID: connID, EnvironmentID: envID,
		Kind: "deploy", Ref: ref, SHA: sha, Branch: branch, Status: "queued",
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO deploy_requests (id, org_id, connection_id, environment_id, kind, ref, sha, branch, status)
		VALUES ($1,$2,$3,$4,'deploy',$5,$6,$7,'queued')
		RETURNING created_at`,
		d.ID, orgID, connID, envID, ref, sha, branch).Scan(&d.CreatedAt)
	return d, err
}

func recordPRHookTx(ctx context.Context, tx pgx.Tx, orgID, connID, ref, sha, branch string) (DeployRequest, error) {
	d := DeployRequest{
		ID: newID("dpr"), OrgID: orgID, ConnectionID: connID,
		Kind: "pr_hook", Ref: ref, SHA: sha, Branch: branch, Status: "recorded",
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO deploy_requests (id, org_id, connection_id, environment_id, kind, ref, sha, branch, status)
		VALUES ($1,$2,$3,NULL,'pr_hook',$4,$5,$6,'recorded')
		RETURNING created_at`,
		d.ID, orgID, connID, ref, sha, branch).Scan(&d.CreatedAt)
	return d, err
}
