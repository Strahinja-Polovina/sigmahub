package api

import (
	"net/http"
)

// handleDeleteServer decommissions a server (soft-delete tombstone + agent-token
// revoke). 409 with the bound-resource list while resources remain. Project
// Admin+.
func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	if err := s.domain.DeleteServer(r.Context(), orgID, serverID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "delete server")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleRevokeAgentToken revokes a server's agent token so its next heartbeat
// 401s and the agent exits. Project Admin+.
func (s *Server) handleRevokeAgentToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	if err := s.domain.RevokeAgentToken(r.Context(), orgID, serverID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "revoke agent token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleListServiceTokens lists an org's service tokens (metadata only). Org
// Admin.
func (s *Server) handleListServiceTokens(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	tokens, err := s.domain.ListServiceTokens(r.Context(), orgID)
	if err != nil {
		s.writeStoreErr(w, err, "list service tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleRevokeServiceToken revokes a service token. Org Admin.
func (s *Server) handleRevokeServiceToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	tokenID := r.PathValue("tokenId")
	if err := s.domain.RevokeServiceToken(r.Context(), orgID, tokenID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "revoke service token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleRotateServiceToken revokes a service token and mints a replacement with
// the same name and role, returning the new plaintext once. Org Admin.
func (s *Server) handleRotateServiceToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	tokenID := r.PathValue("tokenId")
	token, p, err := s.domain.RotateServiceToken(r.Context(), orgID, tokenID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "rotate service token")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "id": p.ID, "name": p.Name, "role": p.Role})
}
