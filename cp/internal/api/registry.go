package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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
	// Name is OPTIONAL. The dashboard's connect form stopped asking for one
	// (SIGMA-202): the machine reports its hostname at registration, and that
	// becomes the name unless the caller supplied one here.
	Name      string `json:"name"`
	Type      string `json:"type"`
	Provider  string `json:"provider"`
	Region    string `json:"region"`
	ProxyRole bool   `json:"proxyRole"`
	// Distro was what the operator PICKED in the connect wizard, before the
	// machine had ever been asked. The dashboard no longer offers that choice —
	// the agent reads /etc/os-release at register and on every heartbeat, and
	// that reading replaces this (SIGMA-201/202). The field survives for API
	// callers that still have a reason to declare one; it is only ever a
	// starting value, and it is validated server-side.
	Distro string `json:"distro"`
	// HostIP: the public address from the connect form, stored as the server's
	// initial endpoint (SIGMA-187) and used as the row's placeholder name until
	// the agent reports a hostname.
	HostIP string `json:"hostIp"`
}

type registerRequest struct {
	BootstrapToken string `json:"bootstrapToken"`
	Name           string `json:"name"`
	AgentVersion   string `json:"agentVersion"`
	// Facts is the agent's host description, carried as raw JSON on purpose:
	// a control plane older than the agent talking to it must store the fields
	// it does not yet understand rather than silently dropping them, which is
	// what a fully typed struct here would do.
	//
	// The keys the product acts on are decoded by store.ParseHostFacts, whose
	// HostFacts covers exactly the Requirement.Fact names in the server catalog
	// — arch, distro, diskTotalBytes, gpu (SIGMA-201). Declaring them a second
	// time here would create a second opinion about what a missing value means,
	// which is the class of bug serverTypeError below exists to document.
	Facts  json.RawMessage `json:"facts"`
	Pubkey string          `json:"pubkey"`
}

// serverTypeError validates a requested server type against the CANONICAL
// catalog and returns the message to reject it with, or "" when it is fine.
//
// This used to be a fifth hand-written list (`validServerTypes`) holding only
// general/database/storage/gpu. It ran BEFORE the store's own check, so the
// store's list was unreachable on these two routes and the boundary refused
// three types the domain model accepted: the connect dialog's own VPS and Build
// buttons POSTed here and got a bare 400 "invalid server type" (SIGMA-198).
// Delegating means the boundary can never again accept less than the store.
func serverTypeError(typ string) string {
	if store.IsServerType(typ) {
		return ""
	}
	return fmt.Sprintf("invalid server type %q; expected one of %s",
		typ, strings.Join(store.ServerTypes(), ", "))
}

// unsupportedDistroMessage names the onboardable distros from the catalog. The
// sentence used to be hand-typed at both rejection points, so adding a distro
// meant editing prose in two places that no test read.
func unsupportedDistroMessage() string {
	return "unsupported distro: only " + store.SupportedDistroSentence() + " can be onboarded"
}

// withInstallerRelease adds the release this control plane installs to a
// bootstrap-token response.
//
// The version travels WITH the token on purpose. The dashboard's only job is to
// render one line containing both, and it used to source them from two places —
// the token from here, the version from its own SIGMAHUB_AGENT_VERSION — so the
// command could name a release this control plane does not serve. Handing both
// back in one response removes the second source rather than synchronising it:
// there is no value the dashboard could render that did not come from the
// control plane that will have to serve it.
//
// agentVersionError is present exactly when agentVersion is empty, and carries
// the control plane's own sentence about which setting is missing, so the dialog
// shows the operator what to fix instead of a symptom.
func (s *Server) withInstallerRelease(body map[string]any) map[string]any {
	version, refusal := s.installerRelease()
	body["agentVersion"] = version
	if refusal != "" {
		body["agentVersionError"] = refusal
	}
	return body
}

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
	if msg := serverTypeError(typ); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	token, serverID, expiresAt, err := s.store.IssueBootstrapToken(
		r.Context(), orgID, req.Name, typ, req.Provider, req.Region, principalFrom(r).Name, defaultBootstrapTTL)
	if err != nil {
		s.log.Error("issue bootstrap token", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, s.withInstallerRelease(map[string]any{
		"token":     token,
		"serverId":  serverID,
		"expiresAt": expiresAt,
	}))
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
	if msg := serverTypeError(typ); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if req.Distro != "" && !store.DistroSupported(req.Distro) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": unsupportedDistroMessage()})
		return
	}
	res, err := s.store.ProvisionServer(r.Context(), orgID, store.ProvisionInput{
		Name: req.Name, Type: typ, Provider: req.Provider, Region: req.Region,
		ProxyRole: req.ProxyRole, Distro: req.Distro, HostIP: req.HostIP,
	}, principalFrom(r).Name, defaultBootstrapTTL)
	if errors.Is(err, store.ErrUnsupportedDistro) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": unsupportedDistroMessage()})
		return
	}
	if err != nil {
		s.log.Error("provision server", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, s.withInstallerRelease(map[string]any{
		"serverId":        res.ServerID,
		"token":           res.Token,
		"expiresAt":       res.ExpiresAt,
		"bootstrapPubkey": res.BootstrapPubkey,
	}))
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
	writeJSON(w, http.StatusCreated, s.withInstallerRelease(map[string]any{
		"serverId":        res.ServerID,
		"token":           res.Token,
		"expiresAt":       res.ExpiresAt,
		"bootstrapPubkey": res.BootstrapPubkey,
	}))
}

// handleAgentUpdate records the desired agent version for a server so the
// reconciler renders an agent.update op — the dashboard-driven, no-SSH upgrade
// path. Project Admin+.
//
// An ABSENT version means "the release this control plane installs", which is
// what the dashboard's upgrade button asks for and the only answer that can be
// right: the agent downloads that release through this control plane's own /dl
// route, which serves exactly one version. A caller may still name a tag
// explicitly — that is how a fleet is pinned or rolled back — and it is checked
// against the same pattern the installer routes use, not a second spelling of
// "looks like a tag" that would reject a prerelease the proxy would happily
// serve.
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
	version := strings.TrimSpace(req.Version)
	if version == "" {
		refusal := ""
		version, refusal = s.installerRelease()
		if refusal != "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": refusal})
			return
		}
	}
	if !releaseTagPattern.MatchString(version) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "version must be a released tag like v0.1.2"})
		return
	}
	if err := s.store.SetDesiredAgentVersion(r.Context(), orgID, serverID, version, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "set desired agent version")
		return
	}
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "queued", "version": version})
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

// handleSetServerType re-files a server under a different type and re-runs the
// registration compatibility gate against the facts already on record — the
// "change the type" exit from `incompatible` (SIGMA-203). Project Admin+.
//
// Answers with the server as it now stands rather than a bare {"status":"ok"}:
// the whole point of this endpoint is that the caller learns immediately
// whether the new type sticks, and a client that had to re-GET would render a
// success toast next to a still-incompatible row.
func (s *Server) handleSetServerType(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	var req struct {
		Type string `json:"type"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if msg := serverTypeError(req.Type); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if err := s.domain.SetServerType(r.Context(), orgID, serverID, req.Type, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "set server type")
		return
	}
	srv, err := s.store.GetServer(r.Context(), orgID, serverID)
	if err != nil {
		s.writeStoreErr(w, err, "get server")
		return
	}
	writeJSON(w, http.StatusOK, srv)
}

// handleRenameServer gives a server an operator-chosen name. The connect form
// stopped asking for one (the machine reports its hostname), so this is where
// the ability to name a server now lives. Project Admin+.
func (s *Server) handleRenameServer(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if err := s.domain.RenameServer(r.Context(), orgID, serverID, req.Name, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "rename server")
		return
	}
	srv, err := s.store.GetServer(r.Context(), orgID, serverID)
	if err != nil {
		s.writeStoreErr(w, err, "get server")
		return
	}
	writeJSON(w, http.StatusOK, srv)
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	// SIGMA-335: ?count=1 answers with the number and nothing else.
	//
	// The dashboard's org switcher shows one integer per org the user belongs
	// to, and it used to get each one by fetching that org's whole server list
	// and calling .length on it. That makes this route build the full dashboard
	// projection — every column, the facts jsonb blob and a correlated
	// readiness subquery per row — and serialise it, once per org, on every
	// render of the layout, with no caching anywhere. A consultant in six orgs
	// of a hundred hosts moved six hundred fully-projected rows across the wire
	// to render six numbers.
	if r.URL.Query().Get("count") == "1" {
		n, err := s.store.CountServers(r.Context(), r.PathValue("orgId"))
		if err != nil {
			s.log.Error("count servers", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": n})
		return
	}
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
