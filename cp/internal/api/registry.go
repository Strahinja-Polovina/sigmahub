package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// defaultBootstrapTTL: SIGMA-A-5 tightens the bootstrap token to 15 minutes,
// single-redemption, bound to a pre-created server. The automated SSH flow
// redeems within seconds; the manual/NAT path still fits comfortably.
const defaultBootstrapTTL = 15 * time.Minute

type issueTokenRequest struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Provider string `json:"provider"`
	Region   string `json:"region"`
}

type provisionRequest struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Provider  string `json:"provider"`
	Region    string `json:"region"`
	ProxyRole bool   `json:"proxyRole"`
	Distro    string `json:"distro"` // detected host OS; validated server-side
	// HostIP: the public address from the connect wizard, stored as the
	// server's initial endpoint (SIGMA-187).
	HostIP string `json:"hostIp"`
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
	token, serverID, expiresAt, err := s.store.IssueBootstrapToken(
		r.Context(), orgID, req.Name, typ, req.Provider, req.Region, principalFrom(r).Name, defaultBootstrapTTL)
	if err != nil {
		s.log.Error("issue bootstrap token", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":     token,
		"serverId":  serverID,
		"expiresAt": expiresAt,
	})
}

// handleProvisionServer is the SSH onboarding entry point: it pre-creates the
// server, mints a per-server ed25519 bootstrap keypair, and returns the bound
// token plus the public key to drop onto the host. Project Admin+.
func (s *Server) handleProvisionServer(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	var req provisionRequest
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
	if req.Distro != "" && !store.DistroSupported(req.Distro) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "unsupported distro: only Ubuntu 22.04/24.04 and Debian 12 can be onboarded"})
		return
	}
	res, err := s.store.ProvisionServer(r.Context(), orgID, store.ProvisionInput{
		Name: req.Name, Type: typ, Provider: req.Provider, Region: req.Region,
		ProxyRole: req.ProxyRole, Distro: req.Distro, HostIP: req.HostIP,
	}, principalFrom(r).Name, defaultBootstrapTTL)
	if errors.Is(err, store.ErrUnsupportedDistro) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "unsupported distro: only Ubuntu 22.04/24.04 and Debian 12 can be onboarded"})
		return
	}
	if err != nil {
		s.log.Error("provision server", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"serverId":        res.ServerID,
		"token":           res.Token,
		"expiresAt":       res.ExpiresAt,
		"bootstrapPubkey": res.BootstrapPubkey,
	})
}

// handleReissueBootstrapToken regenerates the bootstrap keypair + single-use
// token for an existing still-provisioning server, so a lost or expired
// install command doesn't force the operator to create (and then delete) a
// duplicate server record. Project Admin+.
func (s *Server) handleReissueBootstrapToken(w http.ResponseWriter, r *http.Request) {
	res, err := s.store.ReissueBootstrapToken(
		r.Context(), r.PathValue("orgId"), r.PathValue("serverId"), principalFrom(r).Name, defaultBootstrapTTL)
	if err != nil {
		s.writeStoreErr(w, err, "reissue bootstrap token")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"serverId":        res.ServerID,
		"token":           res.Token,
		"expiresAt":       res.ExpiresAt,
		"bootstrapPubkey": res.BootstrapPubkey,
	})
}

var releaseVersionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// handleAgentUpdate records the desired agent version for a server so the
// reconciler renders an agent.update op — the dashboard-driven, no-SSH upgrade
// path. Project Admin+.
func (s *Server) handleAgentUpdate(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	var req struct {
		Version string `json:"version"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if !releaseVersionRe.MatchString(req.Version) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "version must be a released tag like v0.1.2"})
		return
	}
	if err := s.store.SetDesiredAgentVersion(r.Context(), orgID, serverID, req.Version, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "set desired agent version")
		return
	}
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued", "version": req.Version})
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
		// The agent pins this to verify every DSD it later receives.
		"dsdPublicKey": s.dsdPublicKeyB64(),
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
