package api

import (
	"errors"
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// writeDBErr maps database-endpoint store errors: a non-database resource (or
// one in another org) is a 404 like any unknown id.
func (s *Server) writeDBErr(w http.ResponseWriter, err error, op string) {
	if errors.Is(err, store.ErrNotDatabase) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	s.writeStoreErr(w, err, op)
}

// handleGetDatabase returns a database resource's non-secret connection
// metadata (engine, mesh host/port, database, username) plus its backup
// policy. Developer-visible; no reveal, no audit.
func (s *Server) handleGetDatabase(w http.ResponseWriter, r *http.Request) {
	info, err := s.domain.GetDatabaseInfo(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"))
	if err != nil {
		s.writeDBErr(w, err, "get database")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleRevealDatabaseConnection decrypts and returns the generated
// credentials and canonical connection URL. Routed at Project Admin+ (a
// Developer token 403s in the middleware); every successful reveal writes an
// audit row in the store.
func (s *Server) handleRevealDatabaseConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := s.domain.RevealDatabaseConnection(r.Context(),
		r.PathValue("orgId"), r.PathValue("resourceId"), principalFrom(r).Name)
	if err != nil {
		s.writeDBErr(w, err, "reveal database connection")
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

// handleExposeDatabase is the deliberately-preserved public-exposure hook:
// databases are mesh-only in v1, so the API surface exists but returns the
// typed not-enabled error. The eventual design rides P1-5's declarative
// nftables ops plus per-engine TLS (recorded de-scope decision on SIGMA-49).
func (s *Server) handleExposeDatabase(w http.ResponseWriter, r *http.Request) {
	// 404 for non-database resources so the typed error never leaks resource
	// existence semantics beyond what the metadata endpoint already does.
	if _, err := s.domain.GetDatabaseInfo(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId")); err != nil {
		s.writeDBErr(w, err, "expose database")
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error":   "public_exposure_not_enabled",
		"message": "databases are mesh-only in v1; public exposure (per-engine TLS + IP allowlist) ships in a fast-follow",
	})
}
