package api

import (
	"errors"
	"net/http"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// writeDisconnectErr answers a disconnect refusal. It exists to make the 409
// MACHINE-READABLE: the store carries the blocking resource names on the error,
// and the dialog has to list them ("re-home or delete web, api, then try
// again") rather than print a Go error string at the operator (SIGMA-205).
// Everything else falls through to the ordinary mapping.
func (s *Server) writeDisconnectErr(w http.ResponseWriter, err error, op string) {
	var bound store.ErrBoundResources
	if errors.As(err, &bound) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          err.Error(),
			"boundResources": bound.Names,
		})
		return
	}
	s.writeStoreErr(w, err, op)
}

// handleDecommissionServer starts a GRACEFUL decommission (SIGMA-204): the
// server moves to 'decommissioning' and its next DSD is a single
// agent.uninstall op, which removes the workloads and then the agent itself.
// The tombstone is written later — when the agent acks, or when the sweeper
// times the teardown out. 409 (with the names) while resources are bound.
// Project Admin+.
func (s *Server) handleDecommissionServer(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	var req struct {
		// PurgeVolumes destroys named volumes too. Absent means false: the
		// customer's data is not collateral of a machine being returned.
		PurgeVolumes bool `json:"purgeVolumes"`
	}
	// An empty body is a valid request (the conservative default), so a decode
	// failure is only fatal when there WAS a body and it was malformed. `!= 0`
	// rather than `> 0`: a chunked request reports -1, and skipping the decode
	// for one would silently drop a purgeVolumes the operator did tick.
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}
	state, err := s.domain.BeginDecommission(r.Context(), orgID, serverID, req.PurgeVolumes, principalFrom(r).Name)
	if err != nil {
		s.writeDisconnectErr(w, err, "decommission server")
		return
	}
	// Render the uninstall op now rather than at the next 60s resync: the whole
	// point of the graceful path is that the operator watches it happen.
	//
	// Skipped when the server is already gone: there is no document to render
	// for a tombstoned row, and asking the reconciler for one would be work
	// whose only possible output is nothing.
	if s.reconcile != nil && !state.Removed {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":       state.Status,
		"purgeVolumes": state.PurgeVolumes,
		"startedAt":    state.StartedAt,
		"removed":      state.Removed,
	})
}

// handleDeleteServer is the FORCE disconnect: tombstone + agent-token revoke,
// with nothing removed from the host. Offered when the server is unreachable or
// the graceful teardown timed out; the dialog pairs it with the manual cleanup
// script. 409 with the bound-resource list while resources remain. Project
// Admin+.
func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	if err := s.domain.DeleteServer(r.Context(), orgID, serverID, principalFrom(r).Name); err != nil {
		s.writeDisconnectErr(w, err, "delete server")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// handleAgentUninstallAck is the agent's final word on a decommission, and the
// one message on the agent channel that removes a server.
//
// It is a dedicated endpoint rather than the ordinary DSD op-status report for
// a sequencing reason: op status is posted by the DSD loop AFTER the handler
// returns, and by then the handler has already torn down the WireGuard
// interface and deleted the data dir that holds the agent's credential. The ack
// has to be sent from INSIDE the handler, while the channel it travels on still
// exists. See agent/internal/uninstall for the ordering.
func (s *Server) handleAgentUninstallAck(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	var req struct {
		// OK is false when a teardown step failed. The decommission completes
		// either way — the agent has removed itself by the time it can say
		// anything, so holding the row open would leave us waiting on a machine
		// that is already gone — but the detail lands in the audit log so the
		// operator knows to run the manual cleanup script.
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	err := s.store.CompleteDecommission(r.Context(), srv.ID, req.OK, req.Detail)
	if errors.Is(err, store.ErrNotFound) {
		// The sweeper's timeout or a force disconnect got there first. The agent
		// did nothing wrong and has nothing to retry — its host is already torn
		// down — so accept and discard rather than making it back off.
		writeJSON(w, http.StatusOK, map[string]string{"status": "already settled"})
		return
	}
	if err != nil {
		s.writeStoreErr(w, err, "complete decommission")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "decommissioned"})
}

// handleRevokeAgentToken revokes a server's agent token so its next heartbeat
// 401s and the agent exits. Project Admin+.
func (s *Server) handleRevokeAgentToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID := r.PathValue("serverId")
	if err := s.domain.RevokeAgentToken(r.Context(), orgID, serverID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "revoke agent token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleListServiceTokens lists an org's service tokens (metadata only). Org
// Admin.
func (s *Server) handleListServiceTokens(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	tokens, err := s.domain.ListServiceTokens(r.Context(), orgID)
	if err != nil {
		s.writeStoreErr(w, err, "list service tokens")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleRevokeServiceToken revokes a service token. Org Admin.
func (s *Server) handleRevokeServiceToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	tokenID := r.PathValue("tokenId")
	if err := s.domain.RevokeServiceToken(r.Context(), orgID, tokenID, principalFrom(r).Name); err != nil {
		s.writeStoreErr(w, err, "revoke service token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// handleRotateServiceToken revokes a service token and mints a replacement with
// the same name and role, returning the new plaintext once. Org Admin.
func (s *Server) handleRotateServiceToken(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	tokenID := r.PathValue("tokenId")
	token, p, err := s.domain.RotateServiceToken(r.Context(), orgID, tokenID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "rotate service token")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "id": p.ID, "name": p.Name, "role": p.Role})
}
