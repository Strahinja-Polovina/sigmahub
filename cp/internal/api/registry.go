package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

const defaultBootstrapTTL = time.Hour

type issueTokenRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Provider string `json:"provider"`
	Region   string `json:"region"`
}

type registerRequest struct {
	BootstrapToken string          `json:"bootstrapToken"`
	Name           string          `json:"name"`
	AgentVersion   string          `json:"agentVersion"`
	Facts          json.RawMessage `json:"facts"`
	Pubkey         string          `json:"pubkey"`
}

var validServerTypes = map[string]bool{"general": true, "database": true, "storage": true, "gpu": true}

func (s *Server) handleIssueBootstrapToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	var req issueTokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	typ := req.Type
	if typ == "" {
		typ = "general"
	}
	if !validServerTypes[typ] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid server type"})
		return
	}
	token, expiresAt, err := s.store.IssueBootstrapToken(
		r.Context(), orgID, req.Name, typ, req.Provider, req.Region, principalFrom(r).Name, defaultBootstrapTTL)
	if err != nil {
		s.log.Error("issue bootstrap token", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":     token,
		"expiresAt": expiresAt,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.BootstrapToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bootstrapToken is required"})
		return
	}
	res, err := s.store.RegisterServer(
		r.Context(), req.BootstrapToken, req.Name, req.AgentVersion, req.Facts, req.Pubkey)
	if errors.Is(err, store.ErrTokenInvalid) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bootstrap token invalid"})
		return
	}
	if err != nil {
		s.log.Error("register server", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"serverId":   res.Server.ID,
		"agentToken": res.AgentToken,
		"server":     res.Server,
	})
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	servers, err := s.store.ListServers(r.Context(), r.PathValue("orgId"))
	if err != nil {
		s.log.Error("list servers", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if servers == nil {
		servers = []store.Server{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), r.PathValue("orgId"), r.PathValue("serverId"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "server not found"})
		return
	}
	if err != nil {
		s.log.Error("get server", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, srv)
}
