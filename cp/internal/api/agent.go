package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

type ctxKey int

const serverCtxKey ctxKey = 0

// requireAgent authenticates `Authorization: Bearer sat_…` and puts the
// resolved server on the request context.
func (s *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent token required"})
			return
		}
		srv, err := s.store.ServerByAgentToken(r.Context(), tok)
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "agent token invalid"})
			return
		}
		if err != nil {
			s.log.Error("agent auth", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), serverCtxKey, srv)))
	}
}

func serverFrom(r *http.Request) store.Server {
	srv, _ := r.Context().Value(serverCtxKey).(store.Server)
	return srv
}

type hardeningReport struct {
	Score         int  `json:"score"`
	DiskEncrypted bool `json:"diskEncrypted"`
	SSHLocked     bool `json:"sshLocked"`
}

type heartbeatRequest struct {
	AgentVersion  string              `json:"agentVersion"`
	Facts         json.RawMessage     `json:"facts"`
	Pubkey        string              `json:"pubkey"`
	Endpoint      string              `json:"endpoint"`
	Metrics       *store.MetricSample `json:"metrics"`
	Hardening     *hardeningReport    `json:"hardening"`
	MeshApplied   bool                `json:"meshApplied"`
	MeshPeerCount int                 `json:"meshPeerCount"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	srv := serverFrom(r)
	var hardening *store.HardeningReport
	if req.Hardening != nil {
		hardening = &store.HardeningReport{
			Score:         req.Hardening.Score,
			DiskEncrypted: req.Hardening.DiskEncrypted,
			SSHLocked:     req.Hardening.SSHLocked,
		}
	}
	if err := s.store.RecordHeartbeat(r.Context(), srv.ID, store.HeartbeatInput{
		AgentVersion:  req.AgentVersion,
		Facts:         req.Facts,
		Pubkey:        req.Pubkey,
		Endpoint:      req.Endpoint,
		Metrics:       req.Metrics,
		Hardening:     hardening,
		MeshApplied:   req.MeshApplied,
		MeshPeerCount: req.MeshPeerCount,
	}); err != nil {
		s.log.Error("heartbeat", "err", err, "server", srv.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleMeshPeers returns the requesting agent's own mesh identity plus its
// same-org peers. Org isolation falls out of the query: peers are looked up
// by the authenticated server's org, never by caller-supplied input.
func (s *Server) handleMeshPeers(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	peers, err := s.store.MeshPeers(r.Context(), srv.OrgID, srv.ID)
	if err != nil {
		s.log.Error("mesh peers", "err", err, "server", srv.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"self": map[string]any{
			"serverId": srv.ID,
			"meshIp":   srv.MeshIP,
			"pubkey":   srv.Pubkey,
		},
		"peers": peers,
	})
}

// handleAgentGitCredential mints the short-lived clone credential for a
// deployment (P1-9). Scoped to the requesting server: an agent can only fetch the
// credential for a deployment targeting its own host. The plaintext is returned
// for in-memory use by git.clone and never lands on disk.
func (s *Server) handleAgentGitCredential(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	depID := r.URL.Query().Get("deploymentId")
	if depID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "deploymentId is required"})
		return
	}
	token, repo, provider, err := s.store.DeploymentCloneCredential(r.Context(), srv.ID, depID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no deployment for this server"})
		return
	}
	if err != nil {
		s.log.Error("git credential", "err", err, "server", srv.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": token, "repoFullName": repo, "provider": provider})
}

// handleAgentBuildLog ingests build/orchestration log lines for the deploy SSE
// stream. Bounded per request.
func (s *Server) handleAgentBuildLog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeploymentID string   `json:"deploymentId"`
		Stream       string   `json:"stream"`
		Lines        []string `json:"lines"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.DeploymentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "deploymentId and lines required"})
		return
	}
	if len(req.Lines) > 500 {
		req.Lines = req.Lines[:500]
	}
	for _, line := range req.Lines {
		if err := s.store.AppendDeployLog(r.Context(), req.DeploymentID, req.Stream, line); err != nil {
			s.log.Error("append deploy log", "err", err)
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// validCertStatus bounds the cert-status values an agent may report, so a buggy
// or compromised agent can't persist an arbitrary status string.
var validCertStatus = map[string]bool{"pending": true, "issuing": true, "issued": true, "failed": true}

// handleAgentDomainStatus ingests the ACME certificate state the agent reads
// from Traefik's acme store. Each entry is scoped to the reporting server, so an
// agent can only update cert state for domains routed to a resource it hosts.
func (s *Server) handleAgentDomainStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Domains []struct {
			Domain    string     `json:"domain"`
			Status    string     `json:"status"`
			Serial    string     `json:"serial"`
			ExpiresAt *time.Time `json:"expiresAt"`
			Error     string     `json:"error"`
		} `json:"domains"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	srv := serverFrom(r)
	applied := 0
	for _, d := range req.Domains {
		if strings.TrimSpace(d.Domain) == "" || !validCertStatus[d.Status] {
			continue // ignore blank domains and unknown status values
		}
		err := s.store.SetDomainCertStatus(r.Context(), srv.ID, d.Domain, d.Status, d.Serial, d.ExpiresAt, d.Error)
		if errors.Is(err, store.ErrNotFound) {
			continue // domain not on this server (or detached) — ignore, don't fail the batch
		}
		if err != nil {
			s.log.Error("domain status", "err", err, "server", srv.ID, "domain", d.Domain)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		applied++
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "applied": applied})
}
