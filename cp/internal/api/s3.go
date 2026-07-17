package api

// S3 storage endpoints (P2-1), mirroring the database pair: metadata is
// member-visible, the credential reveal is Project Admin and audited.

import (
	"errors"
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func (s *Server) writeS3Err(w http.ResponseWriter, err error, action string) {
	if errors.Is(err, store.ErrNotS3) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "resource is not an s3 storage"})
		return
	}
	s.writeStoreErr(w, err, action)
}

func (s *Server) handleGetS3(w http.ResponseWriter, r *http.Request) {
	info, err := s.domain.GetS3Info(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"))
	if err != nil {
		s.writeS3Err(w, err, "get s3 info")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleRevealS3Connection(w http.ResponseWriter, r *http.Request) {
	conn, err := s.domain.RevealS3Connection(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"), principalFrom(r).Name)
	if err != nil {
		s.writeS3Err(w, err, "reveal s3 connection")
		return
	}
	writeJSON(w, http.StatusOK, conn)
}
