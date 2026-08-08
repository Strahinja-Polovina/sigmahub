package api

// GitHub as an ORG-LEVEL integration: connect the App once, then select repos.
//
// The per-connection endpoints in git.go still exist (a token-based connection
// to a repo the App can't see remains valid), but the normal path is now
// install → list repos → pick one, with the git_connection derived behind the
// scenes so nobody has to assemble one by hand per resource.

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/githubapp"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// GitIntegrationAPI is the org-level-integration slice of the store.
type GitIntegrationAPI interface {
	ClaimInstallationWithMeta(ctx context.Context, orgID, installationID, login, accountType, actor string) (store.GitHubInstallation, error)
	ListOrgInstallations(ctx context.Context, orgID string) ([]store.GitHubInstallation, error)
	DeleteOrgInstallation(ctx context.Context, orgID, installationID, actor string, force bool) error
	EnsureGitConnection(ctx context.Context, orgID string, in store.EnsureGitConnectionInput, actor string) (store.GitConnection, error)
}

// RepoLister lists the repositories an installation token can reach.
type RepoLister interface {
	ListInstallationRepos(ctx context.Context, token string) ([]githubapp.Repo, bool, error)
}

// InstallationAccountSource reads the account an installation belongs to.
type InstallationAccountSource interface {
	Account(ctx context.Context, installationID string) (githubapp.InstallationAccount, error)
}

// handleGetGitIntegration reports the org's GitHub integration state: whether
// the App is configured on this control plane at all, and which installations
// the org has connected. This is what the dashboard renders instead of asking
// for a repo URL and a token.
func (s *Server) handleGetGitIntegration(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"enabled":       s.installTokens != nil,
		"slug":          s.githubAppSlug,
		"installations": []store.GitHubInstallation{},
	}
	if s.gitIntegration != nil {
		insts, err := s.gitIntegration.ListOrgInstallations(r.Context(), r.PathValue("orgId"))
		if err != nil {
			s.writeStoreErr(w, err, "list installations")
			return
		}
		resp["installations"] = insts
		resp["connected"] = len(insts) > 0
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleConnectGitIntegration claims a GitHub App installation for the org —
// the post-install callback lands here. Project Admin+.
func (s *Server) handleConnectGitIntegration(w http.ResponseWriter, r *http.Request) {
	if s.gitIntegration == nil {
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
	orgID := r.PathValue("orgId")

	// Name the integration when we can. A lookup failure is not fatal: the
	// binding is what matters, and an unnamed integration still works.
	var login, accountType string
	if s.installAccounts != nil {
		if acct, err := s.installAccounts.Account(r.Context(), strings.TrimSpace(req.InstallationID)); err == nil {
			login, accountType = acct.Login, acct.Type
		} else {
			s.log.Warn("read installation account", "installation", req.InstallationID, "err", err)
		}
	}
	inst, err := s.gitIntegration.ClaimInstallationWithMeta(r.Context(), orgID,
		req.InstallationID, login, accountType, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "connect github integration")
		return
	}
	writeJSON(w, http.StatusCreated, inst)
}

// handleDisconnectGitIntegration removes an installation binding. Refuses while
// repos still deploy through it unless ?force=true, so one click can't sever
// push-to-deploy for the whole org without saying so first.
func (s *Server) handleDisconnectGitIntegration(w http.ResponseWriter, r *http.Request) {
	if s.gitIntegration == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	force := r.URL.Query().Get("force") == "true"
	err := s.gitIntegration.DeleteOrgInstallation(r.Context(), r.PathValue("orgId"),
		r.PathValue("installationId"), principalFrom(r).Name, force)
	var inUse store.ErrIntegrationInUse
	if errors.As(err, &inUse) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":       "this integration still backs connected repositories",
			"connections": inUse.Connections,
			"hint":        "retry with ?force=true to disconnect anyway; those repos stop deploying on push",
		})
		return
	}
	if err != nil {
		s.writeStoreErr(w, err, "disconnect github integration")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

// handleListGitRepos lists every repo the org's installations can reach —
// the picker's data source. Repos are merged across installations, deduped by
// full name and sorted, so the caller sees one flat selectable list.
func (s *Server) handleListGitRepos(w http.ResponseWriter, r *http.Request) {
	if s.gitIntegration == nil || s.repoLister == nil || s.installTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the GitHub App is not configured on this control plane",
		})
		return
	}
	orgID := r.PathValue("orgId")
	insts, err := s.gitIntegration.ListOrgInstallations(r.Context(), orgID)
	if err != nil {
		s.writeStoreErr(w, err, "list installations")
		return
	}
	if len(insts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"repos": []githubapp.Repo{}, "connected": false})
		return
	}

	seen := map[string]githubapp.Repo{}
	truncated := false
	// Partial failure is reported, not hidden: one broken installation must not
	// blank the picker for the others.
	var failures []string
	for _, inst := range insts {
		token, terr := s.installTokens.InstallationToken(r.Context(), inst.InstallationID)
		if terr != nil {
			s.log.Warn("installation token", "installation", inst.InstallationID, "err", terr)
			failures = append(failures, inst.AccountLogin)
			continue
		}
		repos, cut, lerr := s.repoLister.ListInstallationRepos(r.Context(), token)
		if lerr != nil {
			s.log.Warn("list installation repos", "installation", inst.InstallationID, "err", lerr)
			failures = append(failures, inst.AccountLogin)
			continue
		}
		truncated = truncated || cut
		for _, repo := range repos {
			if _, dup := seen[repo.FullName]; dup {
				continue
			}
			repo.InstallationID = inst.InstallationID
			seen[repo.FullName] = repo
		}
	}

	out := make([]githubapp.Repo, 0, len(seen))
	for _, repo := range seen {
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })

	resp := map[string]any{"repos": out, "connected": true, "truncated": truncated}
	if len(failures) > 0 {
		resp["unavailable"] = failures
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSelectGitRepo binds a repo to a project, deriving the git connection if
// the project doesn't have one yet. Idempotent — selecting the same repo again
// returns the same connection rather than erroring on the uniqueness rule.
func (s *Server) handleSelectGitRepo(w http.ResponseWriter, r *http.Request) {
	if s.gitIntegration == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "git integration is not configured"})
		return
	}
	var req struct {
		ProjectID      string `json:"projectId"`
		RepoFullName   string `json:"repoFullName"`
		InstallationID string `json:"installationId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	orgID := r.PathValue("orgId")
	// The installation must belong to this org before it is written anywhere.
	if !s.claimInstallation(w, r, orgID, req.InstallationID) {
		return
	}
	conn, err := s.gitIntegration.EnsureGitConnection(r.Context(), orgID, store.EnsureGitConnectionInput{
		ProjectID:      req.ProjectID,
		RepoFullName:   req.RepoFullName,
		InstallationID: req.InstallationID,
	}, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "select repo")
		return
	}

	// Push-to-deploy needs the webhook; same best-effort contract as connect.
	if s.inspector != nil && s.publicURL != "" && s.githubWebhookSecret != "" {
		if token := s.effectiveGitToken(r.Context(), "", conn.InstallationID); token != "" {
			hookURL := s.publicURL + "/v1/webhooks/github"
			if err := s.inspector.RegisterPushWebhook(r.Context(), conn.RepoFullName, hookURL, s.githubWebhookSecret, token); err != nil {
				s.log.Warn("register push webhook", "repo", conn.RepoFullName, "err", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, conn)
}
