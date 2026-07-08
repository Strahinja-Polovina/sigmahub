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
	AgentVersion string          `json:"agentVersion"`
	Facts        json.RawMessage `json:"facts"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	srv := serverFrom(r)
	if err := s.store.RecordHeartbeat(r.Context(), srv.ID, req.AgentVersion, req.Facts); err != nil {
		s.log.Error("heartbeat", "err", err, "server", srv.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
