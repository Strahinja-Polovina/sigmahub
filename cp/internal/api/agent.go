package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	AgentVersion string `json:"agentVersion"`
	// Facts is re-sent on EVERY heartbeat, not just at register, so a host that
	// gains a GPU, has a driver installed or has its disk grown is not stuck
	// with what it looked like on day one (SIGMA-201). Same raw-JSON contract as
	// registerRequest.Facts: unknown keys are stored, and the keys the product
	// acts on are decoded once by store.ParseHostFacts.
	//
	// An OLD agent omits the SIGMA-201 keys entirely. That must not blank them:
	// RecordHeartbeat merges rather than assigns, so absent means "unchanged".
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

// meshPeersETag builds the validator for one agent's view of the mesh: the
// org's peer-set fingerprint plus this server's own identity, which the
// response also carries and which changes when the agent re-keys or is given a
// mesh address.
func meshPeersETag(digest string, srv store.Server) string {
	self := ""
	if srv.MeshIP != nil {
		self = *srv.MeshIP
	}
	if srv.Pubkey != nil {
		self += "|" + *srv.Pubkey
	}
	sum := sha256.Sum256([]byte(digest + "|" + srv.ID + "|" + self))
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// etagMatches implements the If-None-Match comparison for our own validators:
// a comma-separated list, `*`, and the weak `W/` prefix, which we ignore
// because a 304 here is about the peer set being byte-identical either way.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

// handleMeshPeers returns the requesting agent's own mesh identity plus its
// same-org peers. Org isolation falls out of the query: peers are looked up
// by the authenticated server's org, never by caller-supplied input.
//
// The answer is conditional (SIGMA-323). Every agent polls this after every
// heartbeat — every 30 seconds — and the peer set only moves when a server is
// enrolled, re-keys, changes endpoint or is deleted, so the unconditional
// version made the steady-state cost quadratic in the size of the org: N agents
// x N rows scanned, allocated and serialised every 30s, over a million rows a
// minute at 500 servers, all of it restating an unchanged answer. Worse, none
// of it was distinguishable from real work, so an operator saw pool contention
// and rising p99 on tenant endpoints with nothing to attribute it to.
//
// With a validator the steady state is one small aggregate and an empty 304,
// and the full payload is paid only when membership actually changes.
func (s *Server) handleMeshPeers(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	// The fingerprint comes first because a match means the peer list never has
	// to be read at all — that skipped read IS the fix. A digest that cannot be
	// computed is not a reason to fail a request the agent needs: fall through
	// to the unconditional answer, which is exactly the old behaviour.
	if digest, derr := s.store.MeshPeersDigest(r.Context(), srv.OrgID, srv.ID); derr == nil {
		etag := meshPeersETag(digest, srv)
		w.Header().Set("ETag", etag)
		// Peer sets are per-agent and security-relevant; no shared cache in
		// front of the control plane may hand one agent another's answer.
		w.Header().Set("Cache-Control", "private, no-cache")
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	} else {
		s.log.Warn("mesh peers digest", "err", derr, "server", srv.ID)
	}
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

// validDeployLogStream is the closed set of deploy-log streams: 'build' is the
// builder's output, 'startup' is a failed generation's own logs captured before
// the container is removed (SIGMA-181).
var validDeployLogStream = map[string]bool{"build": true, "startup": true}

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
	// The stream is a UI column, rendered verbatim next to every line. Bound it
	// to the streams the agent actually produces so a buggy or compromised agent
	// can't paint arbitrary labels into an operator's deploy view.
	if req.Stream != "" && !validDeployLogStream[req.Stream] {
		req.Stream = "build"
	}
	srv := serverFrom(r)
	// Scoped to the servers that RUN this deployment — the deploy target, its
	// build server, a cluster node, a Compose placement host. A server with no
	// part in it can't forge into another deployment's log.
	//
	// One call for the whole batch: this used to loop, and with the agent posting
	// a line at a time that made a 2,000-line build 2,000 requests AND 2,000
	// statements (SIGMA-252).
	if err := s.store.AppendDeployLogs(r.Context(), srv.ID, req.DeploymentID, req.Stream, req.Lines); err != nil {
		s.log.Error("append deploy log", "err", err)
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
