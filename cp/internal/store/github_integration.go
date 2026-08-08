package store

// GitHub as an ORG-LEVEL integration. The App is installed once per org (or
// once per GitHub account the org owns); after that a repo is SELECTED, never
// re-connected by hand, and the git_connection that push-to-deploy needs is
// derived on demand from the installation that can already read the repo.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// GitHubInstallation is one GitHub App installation an org has connected.
type GitHubInstallation struct {
	InstallationID string    `json:"installationId"`
	OrgID          string    `json:"orgId"`
	AccountLogin   string    `json:"accountLogin"`
	AccountType    string    `json:"accountType"` // User|Organization
	CreatedBy      string    `json:"createdBy"`
	CreatedAt      time.Time `json:"createdAt"`
}

// ClaimInstallationWithMeta claims an installation for an org (same
// first-writer-wins rule as ClaimInstallation) and records the account it is
// installed on, so the dashboard can name the integration instead of showing a
// bare numeric id. Idempotent: re-claiming refreshes the metadata.
func (s *Store) ClaimInstallationWithMeta(ctx context.Context, orgID, installationID, login, accountType, actor string) (GitHubInstallation, error) {
	installationID = strings.TrimSpace(installationID)
	if installationID == "" || !isDigits(installationID) {
		return GitHubInstallation{}, ErrInvalid{Msg: "installationId must be a numeric GitHub installation id"}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return GitHubInstallation{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var inst GitHubInstallation
	// The DO UPDATE only fires for the owning org; a different owner keeps its
	// row and we detect the mismatch from the returned org_id (SIGMA-87).
	err = tx.QueryRow(ctx, `
		INSERT INTO github_installations (installation_id, org_id, account_login, account_type, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (installation_id) DO UPDATE SET
			account_login = CASE WHEN github_installations.org_id = EXCLUDED.org_id
			                     THEN EXCLUDED.account_login ELSE github_installations.account_login END,
			account_type  = CASE WHEN github_installations.org_id = EXCLUDED.org_id
			                     THEN EXCLUDED.account_type ELSE github_installations.account_type END,
			updated_at    = now()
		RETURNING installation_id, org_id, account_login, account_type, created_by, created_at`,
		installationID, orgID, strings.TrimSpace(login), strings.TrimSpace(accountType), actor).
		Scan(&inst.InstallationID, &inst.OrgID, &inst.AccountLogin, &inst.AccountType, &inst.CreatedBy, &inst.CreatedAt)
	if err != nil {
		return GitHubInstallation{}, err
	}
	if inst.OrgID != orgID {
		// Owned by another org — opaque 404, never confirms it exists.
		return GitHubInstallation{}, ErrNotFound
	}
	if err := auditTx(ctx, tx, orgID, actor, "GitHub integration connected", inst.AccountLogin); err != nil {
		return GitHubInstallation{}, err
	}
	return inst, tx.Commit(ctx)
}

// ListOrgInstallations returns the org's connected GitHub App installations.
func (s *Store) ListOrgInstallations(ctx context.Context, orgID string) ([]GitHubInstallation, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT installation_id, org_id, account_login, account_type, created_by, created_at
		  FROM github_installations WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GitHubInstallation{}
	for rows.Next() {
		var i GitHubInstallation
		if err := rows.Scan(&i.InstallationID, &i.OrgID, &i.AccountLogin, &i.AccountType, &i.CreatedBy, &i.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ErrIntegrationInUse reports a disconnect refused because repos still deploy
// through the integration. Removing it silently would break push-to-deploy on
// every one of them with no warning (the SIGMA-159 lesson).
type ErrIntegrationInUse struct {
	Connections int
}

func (e ErrIntegrationInUse) Error() string {
	return "the GitHub integration still backs connected repositories"
}

// DeleteOrgInstallation disconnects an integration. It refuses while git
// connections still reference it unless force is set, so a stray click can't
// silently sever push-to-deploy for every repo in the org.
func (s *Store) DeleteOrgInstallation(ctx context.Context, orgID, installationID, actor string, force bool) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owner string
	err = tx.QueryRow(ctx, `SELECT org_id FROM github_installations WHERE installation_id = $1 FOR UPDATE`,
		installationID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && owner != orgID) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	var inUse int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM git_connections
		 WHERE org_id = $1 AND installation_id = $2`, orgID, installationID).Scan(&inUse); err != nil {
		return err
	}
	if inUse > 0 && !force {
		return ErrIntegrationInUse{Connections: inUse}
	}
	// Connections keep their rows (and their repos keep deploying from a stored
	// token if they have one); they simply lose the installation binding.
	if _, err := tx.Exec(ctx, `
		UPDATE git_connections SET installation_id = NULL
		 WHERE org_id = $1 AND installation_id = $2`, orgID, installationID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM github_installations WHERE installation_id = $1`, installationID); err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "GitHub integration disconnected", installationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EnsureGitConnectionInput selects a repo for a project.
type EnsureGitConnectionInput struct {
	ProjectID      string
	RepoFullName   string
	InstallationID string
	Provider       string
}

// EnsureGitConnection is the repo PICKER's counterpart to CreateGitConnection:
// it returns the project's existing connection for a repo, or derives one from
// the org's integration when there isn't one yet.
//
// This is what makes selecting a repo per resource work: the connection (and so
// the webhook routing, branch maps and clone credentials) is an implementation
// detail the user never has to assemble by hand. Idempotent by (project, repo),
// so selecting the same repo for a second resource reuses one connection rather
// than racing to create a duplicate.
func (s *Store) EnsureGitConnection(ctx context.Context, orgID string, in EnsureGitConnectionInput, actor string) (GitConnection, error) {
	repo := strings.TrimSpace(in.RepoFullName)
	projectID := strings.TrimSpace(in.ProjectID)
	if repo == "" || projectID == "" {
		return GitConnection{}, ErrInvalid{Msg: "projectId and repoFullName are required"}
	}

	var conn GitConnection
	err := s.Pool.QueryRow(ctx, `
		SELECT id, org_id, project_id, provider, COALESCE(installation_id, ''), repo_full_name,
		       created_by, created_at, previews_enabled, COALESCE(preview_server_id, '')
		  FROM git_connections
		 WHERE org_id = $1 AND project_id = $2 AND repo_full_name = $3`,
		orgID, projectID, repo).
		Scan(&conn.ID, &conn.OrgID, &conn.ProjectID, &conn.Provider, &conn.InstallationID,
			&conn.RepoFullName, &conn.CreatedBy, &conn.CreatedAt, &conn.PreviewsEnabled, &conn.PreviewServerID)
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return GitConnection{}, err
	}

	installationID := strings.TrimSpace(in.InstallationID)
	if installationID == "" {
		// Fall back to the org's integration — with exactly one installation the
		// choice is unambiguous, which is the whole point of connecting once.
		insts, lerr := s.ListOrgInstallations(ctx, orgID)
		if lerr != nil {
			return GitConnection{}, lerr
		}
		if len(insts) == 1 {
			installationID = insts[0].InstallationID
		}
	}
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = "github"
	}
	created, err := s.CreateGitConnection(ctx, orgID, CreateGitConnectionInput{
		ProjectID:      projectID,
		Provider:       provider,
		InstallationID: installationID,
		RepoFullName:   repo,
		AutoConnected:  true,
	}, actor)
	if err == nil {
		return created, nil
	}
	// A concurrent picker won the race — its connection is just as good.
	if isUniqueViolation(err) {
		if qerr := s.Pool.QueryRow(ctx, `
			SELECT id, org_id, project_id, provider, COALESCE(installation_id, ''), repo_full_name,
			       created_by, created_at, previews_enabled, COALESCE(preview_server_id, '')
			  FROM git_connections
			 WHERE org_id = $1 AND project_id = $2 AND repo_full_name = $3`,
			orgID, projectID, repo).
			Scan(&conn.ID, &conn.OrgID, &conn.ProjectID, &conn.Provider, &conn.InstallationID,
				&conn.RepoFullName, &conn.CreatedBy, &conn.CreatedAt, &conn.PreviewsEnabled, &conn.PreviewServerID); qerr == nil {
			return conn, nil
		}
	}
	return GitConnection{}, err
}
