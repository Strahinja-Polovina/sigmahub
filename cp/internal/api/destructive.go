package api

import (
	"net/http"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
)

// confirmTokenTTL bounds how long a minted destructive-op confirm token is
// valid. Short: the operator requests it and immediately approves in the same
// dialog.
const confirmTokenTTL = 5 * time.Minute

// allowedDestructiveOps is the set of op kinds the two-phase flow guards. Only
// volume removal is destructive of data today; new destructive kinds must be
// added here explicitly (deny-by-default).
var allowedDestructiveOps = map[string]bool{
	dsd.KindVolumeRemove: true,
}

type confirmTokenRequest struct {
	OpKind string `json:"opKind"`
	Target string `json:"target"`
}

// handleIssueConfirmToken is phase 1 of the two-phase destructive-op flow: it
// mints a short-lived, single-use token authorising one destructive op on one
// server. Gated >= Project Admin; the request is audited by the store.
func (s *Server) handleIssueConfirmToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	var req confirmTokenRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if !allowedDestructiveOps[req.OpKind] || req.Target == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "unsupported destructive op or empty target"})
		return
	}
	token, expiresAt, err := s.domain.IssueConfirmToken(r.Context(), orgID, serverID, req.OpKind, req.Target, principalFrom(r).Name, confirmTokenTTL)
	if err != nil {
		s.writeStoreErr(w, err, "issue confirm token")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "expiresAt": expiresAt})
}

type destructiveOpRequest struct {
	Token  string `json:"token"`
	OpKind string `json:"opKind"`
	Target string `json:"target"`
}

// handleConfirmDestructive is phase 2: it atomically claims the confirm token
// and records the destructive op as pending, so the reconciler renders it into
// the server's DSD. Gated >= Project Admin; the confirmation is audited by the
// store. A missing/expired/used token is 404; a token that does not authorise
// the requested op is 422.
func (s *Server) handleConfirmDestructive(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	var req destructiveOpRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if req.Token == "" || !allowedDestructiveOps[req.OpKind] || req.Target == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "missing token or unsupported op"})
		return
	}
	if _, err := s.domain.ConfirmDestructiveOp(r.Context(), orgID, req.Token, serverID, req.OpKind, req.Target, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "confirm destructive op")
		return
	}
	// Re-render the server's DSD so the agent receives the volume.remove op.
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true})
}
