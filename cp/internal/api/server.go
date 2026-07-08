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
	RecordHeartbeat(ctx context.Context, serverID, agentVersion string, facts json.RawMessage) error
	ListServers(ctx context.Context, orgID string) ([]store.Server, error)
	GetServer(ctx context.Context, orgID, serverID string) (store.Server, error)
}

type Server struct {
	log          *slog.Logger
	db           Pinger
	store        StoreAPI
	serviceToken string
	mux          *http.ServeMux
}

func New(log *slog.Logger, db Pinger, st StoreAPI, serviceToken string) *Server {
	s := &Server{log: log, db: db, store: st, serviceToken: serviceToken, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)
	// Dashboard-facing (service token).
	s.mux.HandleFunc("POST /v1/orgs/{orgId}/bootstrap-tokens", s.requireService(s.handleIssueBootstrapToken))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers", s.requireService(s.handleListServers))
	s.mux.HandleFunc("GET /v1/orgs/{orgId}/servers/{serverId}", s.requireService(s.handleGetServer))
	// Agent-facing.
	s.mux.HandleFunc("POST /v1/agent/register", s.handleRegister)
	s.mux.HandleFunc("POST /v1/agent/heartbeat", s.requireAgent(s.handleHeartbeat))
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
