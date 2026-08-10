package api

import (
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

type createSecretRequest struct {
	Name          string `json:"name"`
	Value         string `json:"value"`
	EnvironmentID string `json:"environmentId"` // "" = project-scoped
	EnvVar        bool   `json:"envVar"`
}

// handleCreateSecret creates a project- or environment-scoped secret. Project
// Admin+. The value is encrypted under the org DEK; only metadata is returned.
func (s *Server) handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	projectID := r.PathValue("projectId")
	var req createSecretRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "name is required"})
		return
	}
	sec, err := s.domain.CreateSecret(r.Context(), orgID, principalFrom(r).Name, store.CreateSecretInput{
		ProjectID:     projectID,
		EnvironmentID: req.EnvironmentID,
		Name:          req.Name,
		Value:         req.Value,
		EnvVar:        req.EnvVar,
	})
	if err != nil {
		s.writeStoreErr(w, err, "create secret")
		return
	}
	// A new (or re-valued) secret must actually reach the running containers:
	// mint config deployments for the apps in scope and re-render their servers
	// (SIGMA-166 — this handler previously had no reconcile hook at all, so the
	// change waited for the fleet resync and then wedged the standing rollout
	// generation).
	s.mintConfigDeploysForSecretScope(r, orgID, sec, "secret changed")
	writeJSON(w, http.StatusCreated, sec)
}

type updateSecretRequest struct {
	Value string `json:"value"`
}

// handleUpdateSecretValue re-seals an existing secret's value in place. Project
// Admin+, same bar as create/delete.
//
// SIGMA-264: rotating a credential used to mean delete-then-create, and because
// BOTH halves mint config deployments the delete alone re-rolled every dependent
// app WITHOUT the variable — a live service restarts missing its key, then rolls
// a second time when the replacement lands. One update is one config deployment,
// and the secret keeps its id so every ref that names it still resolves.
func (s *Server) handleUpdateSecretValue(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	secretID := r.PathValue("secretId")
	var req updateSecretRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	sec, err := s.domain.UpdateSecretValue(r.Context(), orgID, secretID, req.Value, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "update secret")
		return
	}
	// Same reconcile rule as create/delete (SIGMA-166): the rendered container
	// spec changed, so the apps in scope need their own rollout generation.
	s.mintConfigDeploysForSecretScope(r, orgID, sec, "secret changed")
	writeJSON(w, http.StatusOK, sec)
}

// handleListSecrets lists secret METADATA (never values) for a project,
// optionally filtered to one environment. Developer+ (no raw values exposed).
func (s *Server) handleListSecrets(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	projectID := r.PathValue("projectId")
	envID := r.URL.Query().Get("environmentId")
	secrets, err := s.domain.ListSecrets(r.Context(), orgID, projectID, envID)
	if err != nil {
		s.writeStoreErr(w, err, "list secrets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": jsonList(secrets)})
}

// handleRevealSecret returns a secret's decrypted value, audited. Gated at
// Project Admin+ at the route, so a Developer 403s before reaching here.
func (s *Server) handleRevealSecret(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	secretID := r.PathValue("secretId")
	value, err := s.domain.RevealSecret(r.Context(), orgID, secretID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "reveal secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"value": value})
}

func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	secretID := r.PathValue("secretId")
	sec, err := s.domain.DeleteSecret(r.Context(), orgID, secretID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete secret")
		return
	}
	// A removed ref changes the rendered container spec — same rule as create
	// (SIGMA-166).
	s.mintConfigDeploysForSecretScope(r, orgID, sec, "secret removed")
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// mintConfigDeploysForSecretScope resolves the app resources a secret's scope
// reaches and mints config deployments for them. Best-effort: the secret write
// itself already committed, so a failure here logs and leaves convergence to
// the fleet resync rather than failing the request.
func (s *Server) mintConfigDeploysForSecretScope(r *http.Request, orgID string, sec store.Secret, reason string) {
	envID := ""
	if sec.EnvironmentID != nil {
		envID = *sec.EnvironmentID
	}
	ids, err := s.domain.AppResourcesForSecretScope(r.Context(), orgID, sec.ProjectID, envID)
	if err != nil {
		s.log.Error("config deploys: resolve secret scope", "err", err)
		return
	}
	s.mintConfigDeploys(r, orgID, ids, reason)
}

// mintConfigDeploys queues config deployments for the given resources and
// nudges the affected servers (SIGMA-166). Best-effort, shared by the secret
// and domain handlers.
func (s *Server) mintConfigDeploys(r *http.Request, orgID string, resourceIDs []string, reason string) {
	refs, err := s.domain.CreateConfigDeployments(r.Context(), orgID, resourceIDs, principalFrom(r).Name, reason)
	if err != nil {
		s.log.Error("config deploys: mint", "err", err, "reason", reason)
		return
	}
	if s.reconcile == nil {
		return
	}
	for _, ref := range refs {
		s.reconcile.ReconcileAsync(ref.OrgID, ref.ServerID)
	}
}

// handleRotateKEK re-wraps the org's DEKs under the current custody key (no data
// re-encryption). Org Admin.
func (s *Server) handleRotateKEK(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	n, err := s.domain.RotateKEK(r.Context(), orgID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "rotate kek")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rewrapped": n})
}

// handleRotateDEK starts a DEK rotation and immediately drives the lazy
// re-encrypt so old DEKs retire once drained. Org Admin.
func (s *Server) handleRotateDEK(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	if _, err := s.domain.RotateDEK(r.Context(), orgID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "rotate dek")
		return
	}
	n, err := s.domain.ReencryptSecrets(r.Context(), orgID)
	if err != nil {
		s.writeStoreErr(w, err, "reencrypt secrets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reencrypted": n})
}

// handleAgentSecrets returns the decrypted secrets a resource should receive,
// over the already-authenticated agent channel. Org/resource scope come from
// the agent's own server identity, never caller input. Every fetch is audited.
func (s *Server) handleAgentSecrets(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	resourceID := r.URL.Query().Get("resourceId")
	if resourceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "resourceId is required"})
		return
	}
	secrets, err := s.store.ResolveSecretsForResource(r.Context(), srv.OrgID, srv.ID, resourceID, "sigmad("+srv.ID+")")
	if err != nil {
		s.writeStoreErr(w, err, "resolve secrets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": secrets})
}
