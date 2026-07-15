// Package api is the control-plane HTTP surface.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// Pinger is the slice of the store the API needs for readiness.
type Pinger interface {
	Ping(ctx context.Context) error
}

// StoreAPI is everything the HTTP handlers need from the store; faked in
// tests.
type StoreAPI interface {
	IssueBootstrapToken(ctx context.Context, orgID, serverName, serverType, provider, region, createdBy string, ttl time.Duration) (string, time.Time, error)
	RegisterServer(ctx context.Context, bootstrapToken, name, agentVersion string, facts json.RawMessage, pubkey string) (store.RegisterResult, error)
	ServerByAgentToken(ctx context.Context, token string) (store.Server, error)
	AuthenticateServiceToken(ctx context.Context, token string) (store.ServicePrincipal, error)
	RecordHeartbeat(ctx context.Context, serverID string, in store.HeartbeatInput) error
	MeshPeers(ctx context.Context, orgID, selfServerID string) ([]store.MeshPeer, error)
	MetricsSince(ctx context.Context, orgID, serverID string, since time.Time) ([]store.MetricPoint, error)
	ListServers(ctx context.Context, orgID string) ([]store.Server, error)
	GetServer(ctx context.Context, orgID, serverID string) (store.Server, error)
}

type Server struct {
	log             *slog.Logger
	db              Pinger
	store           StoreAPI
	domain          DomainAPI
	devServiceToken string
	provisionToken  string
	mux             *http.ServeMux
}

// Options carries the API's authn material. DevServiceToken is the dev-mode
// static bypass (empty in prod); ProvisionToken gates POST /v1/orgs.
type Options struct {
	DevServiceToken string
	ProvisionToken  string
}

// New builds the HTTP surface.
func New(log *slog.Logger, db Pinger, st StoreAPI, dom DomainAPI, opts Options) *Server {
	s := &Server{
		log: log, db: db, store: st, domain: dom,
		devServiceToken: opts.DevServiceToken,
		provisionToken:  opts.ProvisionToken,
		mux:             http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Org provisioning (dedicated token; mints the org's web credential).
	s.mux.HandleFunc("POST /v1/orgs", s.requireProvision(s.handleProvisionOrg))

	// Dashboard-facing (org-scoped service tokens): reads need any role,
	// mutations need at least Project Admin — mirroring the v1 web RBAC.
	// Mutating POSTs support Idempotency-Key replay — EXCEPT token minting:
	// replaying a mint must issue a fresh token, never store/return the
	// one-time plaintext for later replay.
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/bootstrap-tokens", s.requireService(store.RoleProjectAdmin, s.handleIssueBootstrapToken))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers", s.requireService(store.RoleDeveloper, s.handleListServers))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers/{serverId}", s.requireService(store.RoleDeveloper, s.handleGetServer))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers/{serverId}/metrics", s.requireService(store.RoleDeveloper, s.handleGetMetrics))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/servers/{serverId}/proxy-role", s.requireService(store.RoleProjectAdmin, s.handleProxyRole))

	// Domain model (P1-1).
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/projects", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleCreateProject)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/projects", s.requireService(store.RoleDeveloper, s.handleListProjects))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/projects/{projectId}", s.requireService(store.RoleDeveloper, s.handleGetProject))
	s.mux.HandleFunc("PATCH /v1/orgs/{orgId}/projects/{projectId}", s.requireService(store.RoleProjectAdmin, s.handleUpdateProject))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/projects/{projectId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteProject))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/projects/{projectId}/environments", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleCreateEnvironment)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/projects/{projectId}/environments", s.requireService(store.RoleDeveloper, s.handleListEnvironments))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/environments/{envId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteEnvironment))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/environments/{envId}/servers", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleAttachServer)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/environments/{envId}/servers", s.requireService(store.RoleDeveloper, s.handleEnvServers))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/environments/{envId}/servers/{serverId}", s.requireService(store.RoleProjectAdmin, s.handleDetachServer))
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/resources", s.requireService(store.RoleProjectAdmin, s.idempotent(s.handleCreateResource)))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/resources", s.requireService(store.RoleDeveloper, s.handleListResources))
	s.mux.HandleFunc("DELETE /v1/orgs/{orgId}/resources/{resourceId}", s.requireService(store.RoleProjectAdmin, s.handleDeleteResource))
	// Audit is member-visible (matches the web settings tab shown to all
	// members); mutations above stay Project Admin+.
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/audit", s.requireService(store.RoleDeveloper, s.handleListAudit))

	// Agent-facing.
	s.mux.HandleFunc("POST /v1/agent/register", s.handleRegister)
	s.mux.HandleFunc("POST /v1/agent/heartbeat", s.requireAgent(s.handleHeartbeat))
	s.mux.HandleFunc("GET /v1/agent/mesh/peers", s.requireAgent(s.handleMeshPeers))
}

func (s *Server) Handler() http.Handler {
	return s.withLogging(s.mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable", "reason": "database unreachable",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxBodyBytes caps request bodies. The register endpoint is unauthenticated,
// so an unbounded decode is a pre-auth memory-exhaustion vector.
const maxBodyBytes = 1 << 20 // 1 MiB

// decodeJSON reads a size-capped JSON body. On an oversized or malformed body
// it returns an error; callers respond 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	return json.NewDecoder(r.Body).Decode(dst)
}
