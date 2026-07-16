package api

import (
	"net/http"
)

// ── Database resources (P1-10) ────────────────────────────────────────────────

// handleRevealDBConnection returns a database resource's connection string
// (mesh-internal host + generated credentials). Routed behind Project Admin+
// (a Developer gets 403 from the role gate); every successful reveal writes an
// audit row in the store.
func (s *Server) handleRevealDBConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := s.domain.RevealDBConnection(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "reveal db connection")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"connectionString": conn, "scope": "mesh-internal"})
}

// handleGetBackupPolicy returns the resource's backup policy (the P1-11 hook
// row written at creation). Member-visible; contains no secrets.
func (s *Server) handleGetBackupPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.domain.BackupPolicyForResource(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"))
	if err != nil {
		s.writeStoreErr(w, err, "backup policy")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleExposeDB is the public-exposure hook: v1 databases are mesh-internal
// ONLY (the recorded de-scope), so this returns a TYPED not-enabled error. The
// eventual design rides P1-5's declarative nftables ops plus per-engine TLS —
// deferred, hook preserved.
func (s *Server) handleExposeDB(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "public database exposure is not enabled: v1 databases are mesh-internal only",
		"code":  "db_public_exposure_not_enabled",
	})
}
