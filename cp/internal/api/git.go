package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/gitdetect"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// GitAPI is the git-integration slice of the store the P1-7 endpoints need.
type GitAPI interface {
	HandleGitWebhook(ctx context.Context, ev store.GitWebhookEvent) (store.WebhookOutcome, error)
	CreateGitConnection(ctx context.Context, orgID string, in store.CreateGitConnectionInput, actor string) (store.GitConnection, error)
	ListGitConnections(ctx context.Context, orgID, projectID string) ([]store.GitConnection, error)
	GetGitConnection(ctx context.Context, orgID, connID string) (store.GitConnection, error)
	DeleteGitConnection(ctx context.Context, orgID, connID, actor string) error
	SetBranchMap(ctx context.Context, orgID, connID, branch, envID, policy, actor string) (store.BranchMap, error)
	ListBranchMaps(ctx context.Context, orgID, connID string) ([]store.BranchMap, error)
	DeleteBranchMap(ctx context.Context, orgID, mapID, actor string) error
	PromoteBranch(ctx context.Context, orgID, mapID, actor string) (store.DeployRequest, error)
	ListDeployRequests(ctx context.Context, orgID string, limit int) ([]store.DeployRequest, error)
	// Previews (P1-12): the per-connection toggle + PR environment records.
	SetConnectionPreviews(ctx context.Context, orgID, connID string, enabled bool, serverID, actor string) error
	ListPreviewEnvironments(ctx context.Context, orgID, connID string) ([]store.PreviewEnvironment, error)
	// GitHub App (SIGMA-55): link an installation to an existing connection.
	SetConnectionInstallation(ctx context.Context, orgID, connID, installationID, actor string) error
	// GitHub App (SIGMA-87): bind an installation id to the acting org
	// (first-writer-wins); errors if it belongs to another org.
	ClaimInstallation(ctx context.Context, orgID, installationID string) error
}

// claimInstallation binds a client-supplied installation id to the acting org
// (first-writer-wins) before it is used to mint a token or persisted, so it can
// never reference an installation another org owns (SIGMA-87). Returns true when
// the caller may proceed; on a cross-org / invalid id it writes the response and
// returns false. A no-op (proceed) when no installation id is supplied or the
// git store isn't wired.
func (s *Server) claimInstallation(w http.ResponseWriter, r *http.Request, orgID, installationID string) bool {
	if strings.TrimSpace(installationID) == "" || s.git == nil {
		return true
	}
	if err := s.git.ClaimInstallation(r.Context(), orgID, installationID); err != nil {
		s.writeStoreErr(w, err, "claim installation")
		return false
	}
	return true
}

// InstallationTokenSource mints GitHub App installation access tokens.
type InstallationTokenSource interface {
	InstallationToken(ctx context.Context, installationID string) (string, error)
}

// handleSetPreviews flips a connection's preview flag and designates the
// preview server. Project Admin+.
func (s *Server) handleSetPreviews(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  bool   `json:"enabled"`
		ServerID string `json:"serverId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	err := s.git.SetConnectionPreviews(r.Context(), r.PathValue("orgId"), r.PathValue("connId"),
		req.Enabled, req.ServerID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "set previews")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "enabled": req.Enabled})
}

// handleListPreviews lists a connection's preview environments (open first).
func (s *Server) handleListPreviews(w http.ResponseWriter, r *http.Request) {
	previews, err := s.git.ListPreviewEnvironments(r.Context(), r.PathValue("orgId"), r.PathValue("connId"))
	if err != nil {
		s.writeStoreErr(w, err, "list previews")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"previews": previews})
}

// RepoInspector derives the deploy config from a connected repo's files.
type RepoInspector interface {
	Inspect(ctx context.Context, repoFullName, token string) (gitdetect.Detected, error)
}

// effectiveGitToken picks the credential detect/connect read the repo with:
// an explicitly pasted token wins; otherwise an App installation token is
// minted when the request references an installation and the App is
// configured. Minting failures degrade to unauthenticated (public-repo) reads
// — the resulting 404s surface as "could not read repository", the same
// behaviour as a wrong PAT.
func (s *Server) effectiveGitToken(ctx context.Context, token, installationID string) string {
	if token != "" || strings.TrimSpace(installationID) == "" || s.installTokens == nil {
		return token
	}
	minted, err := s.installTokens.InstallationToken(ctx, strings.TrimSpace(installationID))
	if err != nil {
		s.log.Warn("github app installation token", "installation", installationID, "err", err)
		return ""
	}
	return minted
}

// handleGitDetect previews the deploy config sigmahub detects for a repo, so the
// connect wizard can pre-fill it. Read-only; persists nothing.
func (s *Server) handleGitDetect(w http.ResponseWriter, r *http.Request) {
	if s.inspector == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "repo detection is not configured"})
		return
	}
	var req struct {
		RepoFullName   string `json:"repoFullName"`
		InstallationID string `json:"installationId"`
		Token          string `json:"token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(req.RepoFullName) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repoFullName is required"})
		return
	}
	// SIGMA-87: bind the installation to this org before minting a token with it.
	if !s.claimInstallation(w, r, r.PathValue("orgId"), req.InstallationID) {
		return
	}
	token := s.effectiveGitToken(r.Context(), req.Token, req.InstallationID)
	detected, err := s.inspector.Inspect(r.Context(), req.RepoFullName, token)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not read repository: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, detected)
}

// handleGitConnect connects a repo to a project. When detection is available it
// gates the connection on the repo being deployable (a Dockerfile or Compose
// file present) — an undeployable repo gets a 422 with an actionable reason
// rather than a dangling connection.
func (s *Server) handleGitConnect(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	orgID := r.PathValue("orgId")
	var req struct {
		ProjectID      string `json:"projectId"`
		Provider       string `json:"provider"`
		InstallationID string `json:"installationId"`
		RepoFullName   string `json:"repoFullName"`
		Token          string `json:"token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(req.ProjectID) == "" || strings.TrimSpace(req.RepoFullName) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "projectId and repoFullName are required"})
		return
	}

	// SIGMA-87: bind the installation to this org before minting a token with it.
	if !s.claimInstallation(w, r, orgID, req.InstallationID) {
		return
	}
	// Deployability gate: only refuse when detection succeeds AND says the repo
	// ships neither a Dockerfile nor a Compose file. A detection error (private
	// repo, transient) is not a hard block — the connection is still allowed and
	// detection can be retried from the UI.
	if s.inspector != nil {
		token := s.effectiveGitToken(r.Context(), req.Token, req.InstallationID)
		if detected, derr := s.inspector.Inspect(r.Context(), req.RepoFullName, token); derr == nil && !detected.Deployable {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":    detected.Reason,
				"detected": detected,
			})
			return
		}
	}

	conn, err := s.git.CreateGitConnection(r.Context(), orgID, store.CreateGitConnectionInput{
		ProjectID:      strings.TrimSpace(req.ProjectID),
		Provider:       strings.TrimSpace(req.Provider),
		InstallationID: strings.TrimSpace(req.InstallationID),
		RepoFullName:   strings.TrimSpace(req.RepoFullName),
		Token:          req.Token,
	}, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "connect repo")
		return
	}
	writeJSON(w, http.StatusCreated, conn)
}

func (s *Server) handleListGitConnections(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	conns, err := s.git.ListGitConnections(r.Context(), r.PathValue("orgId"), r.URL.Query().Get("projectId"))
	if err != nil {
		s.writeStoreErr(w, err, "list connections")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": conns})
}

// handleGetGitConnection returns a connection together with its branch routes,
// so the web detail view is a single fetch.
func (s *Server) handleGetGitConnection(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	orgID, connID := r.PathValue("orgId"), r.PathValue("connId")
	conn, err := s.git.GetGitConnection(r.Context(), orgID, connID)
	if err != nil {
		s.writeStoreErr(w, err, "get connection")
		return
	}
	maps, err := s.git.ListBranchMaps(r.Context(), orgID, connID)
	if err != nil {
		s.writeStoreErr(w, err, "list branch maps")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connection": conn, "branchMaps": maps})
}

func (s *Server) handleDeleteGitConnection(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	if err := s.git.DeleteGitConnection(r.Context(), r.PathValue("orgId"), r.PathValue("connId"), principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "disconnect repo")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetBranchMap(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	var req struct {
		Branch        string `json:"branch"`
		EnvironmentID string `json:"environmentId"`
		Policy        string `json:"policy"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	m, err := s.git.SetBranchMap(r.Context(), r.PathValue("orgId"), r.PathValue("connId"),
		req.Branch, req.EnvironmentID, req.Policy, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "map branch")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleDeleteBranchMap(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	if err := s.git.DeleteBranchMap(r.Context(), r.PathValue("orgId"), r.PathValue("mapId"), principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "unmap branch")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePromoteBranch enqueues a deploy of a manual branch's last-seen commit.
func (s *Server) handlePromoteBranch(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	dr, err := s.git.PromoteBranch(r.Context(), r.PathValue("orgId"), r.PathValue("mapId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "promote branch")
		return
	}
	writeJSON(w, http.StatusAccepted, dr)
}

// handleGitAppInfo tells the dashboard whether a GitHub App is registered:
// the slug builds the install link, enabled says the CP can mint installation
// tokens (i.e. the App private key + id are configured).
func (s *Server) handleGitAppInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.installTokens != nil,
		"slug":    s.githubAppSlug,
	})
}

// handleSetInstallation links a GitHub App installation to a connection — the
// dashboard's post-install callback lands here. Project Admin+.
func (s *Server) handleSetInstallation(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	var req struct {
		InstallationID string `json:"installationId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	// SIGMA-87: bind the installation to this org before linking it.
	if !s.claimInstallation(w, r, r.PathValue("orgId"), req.InstallationID) {
		return
	}
	err := s.git.SetConnectionInstallation(r.Context(), r.PathValue("orgId"), r.PathValue("connId"),
		req.InstallationID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "link installation")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "linked"})
}

func (s *Server) handleListDeployRequests(w http.ResponseWriter, r *http.Request) {
	if s.git == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	reqs, err := s.git.ListDeployRequests(r.Context(), r.PathValue("orgId"), 0)
	if err != nil {
		s.writeStoreErr(w, err, "list deploy requests")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployRequests": reqs})
}
