package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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

type heartbeatRequest struct {
	AgentVersion string              `json:"agentVersion"`
	Facts        json.RawMessage     `json:"facts"`
	Pubkey       string              `json:"pubkey"`
	Endpoint     string              `json:"endpoint"`
	Metrics      *store.MetricSample `json:"metrics"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	srv := serverFrom(r)
	if err := s.store.RecordHeartbeat(r.Context(), srv.ID, store.HeartbeatInput{
		AgentVersion: req.AgentVersion,
		Facts:        req.Facts,
		Pubkey:       req.Pubkey,
		Endpoint:     req.Endpoint,
		Metrics:      req.Metrics,
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
