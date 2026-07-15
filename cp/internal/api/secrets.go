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
	writeJSON(w, http.StatusCreated, sec)
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
	writeJSON(w, http.StatusOK, map[string]any{"secrets": secrets})
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
	if err := s.domain.DeleteSecret(r.Context(), orgID, secretID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "delete secret")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
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
