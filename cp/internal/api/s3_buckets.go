package api

// SIGMA-65 S3 bucket/key CRUD + quotas + storage metering HTTP surface. The
// dashboard-facing routes mirror the database split (list is Developer-visible,
// mutations are Project Admin+); the agent-facing routes mirror the backup
// credential/status pair (audited per-op credential release + terminal report).

import (
	"net/http"
	"strings"
	"time"
)

// ── Dashboard-facing ────────────────────────────────────────────────────────

func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.domain.ListBuckets(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"))
	if err != nil {
		s.writeStoreErr(w, err, "list buckets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": jsonList(buckets)})
}

func (s *Server) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	orgID := r.PathValue("orgId")
	bucket, serverID, err := s.domain.CreateBucket(r.Context(), orgID, r.PathValue("resourceId"), req.Name, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "create bucket")
		return
	}
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusCreated, bucket)
}

func (s *Server) handleDeleteBucket(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	serverID, err := s.domain.DeleteBucket(r.Context(), orgID, r.PathValue("resourceId"), r.PathValue("bucket"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete bucket")
		return
	}
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleting"})
}

func (s *Server) handleSetBucketQuota(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QuotaBytes int64 `json:"quotaBytes"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	orgID := r.PathValue("orgId")
	serverID, err := s.domain.SetBucketQuota(r.Context(), orgID, r.PathValue("resourceId"), r.PathValue("bucket"), req.QuotaBytes, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "set bucket quota")
		return
	}
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "quotaBytes": req.QuotaBytes})
}

func (s *Server) handleCreateBucketKey(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	accessKey, serverID, err := s.domain.CreateBucketKey(r.Context(), orgID, r.PathValue("resourceId"), r.PathValue("bucket"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "create bucket key")
		return
	}
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	// The secret is not returned here: the op that programs it into the engine
	// has not run yet, so the only honest answer at this point is the key id.
	// It is readable afterwards through handleRevealBucketKey, which is the
	// audited path a human uses (SIGMA-313) — the executing agent still gets it
	// through its own per-op credential release.
	writeJSON(w, http.StatusCreated, map[string]string{"accessKey": accessKey})
}

// handleRevealBucketKey returns a bucket's scoped access key AND secret to a
// Project Admin+, audited in the store. This mirrors GET .../s3/connection for
// the root credential; before it existed the per-bucket key was write-only and
// therefore useless to the operator who minted it (SIGMA-313).
func (s *Server) handleRevealBucketKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.domain.RevealBucketKey(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"),
		r.PathValue("bucket"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "reveal bucket key")
		return
	}
	writeJSON(w, http.StatusOK, key)
}

// ── Agent-facing ────────────────────────────────────────────────────────────

// handleAgentS3OpCredential releases the root credential (and the new per-bucket
// secret for a create-key op) for ONE open op scheduled on the calling agent's
// server. Every call is audited by the store (the per-op key-release invariant).
func (s *Server) handleAgentS3OpCredential(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	opID := r.URL.Query().Get("opId")
	if opID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "opId is required"})
		return
	}
	cred, err := s.store.S3OpCredentialForOp(r.Context(), srv.ID, opID)
	if err != nil {
		s.writeStoreErr(w, err, "s3 op credential")
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// handleAgentS3OpStatus records an op's terminal outcome. On success it applies
// the op (transitioning the bucket) and, for a measure op, records the measured
// bytes; on failure it marks the op failed with the detail. BOLA-scoped: only
// the op's own server may report.
func (s *Server) handleAgentS3OpStatus(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	var req struct {
		OpID          string `json:"opId"`
		OK            bool   `json:"ok"`
		Detail        string `json:"detail"`
		MeasuredBytes int64  `json:"measuredBytes"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.OpID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "opId is required"})
		return
	}
	if req.OK {
		if err := s.store.MarkS3OpApplied(r.Context(), srv.ID, req.OpID, req.Detail); err != nil {
			s.writeStoreErr(w, err, "s3 op status")
			return
		}
		// A measure op carries its result; record it — including 0 bytes for a
		// genuinely empty bucket, so metering sees an explicit daily row rather
		// than a gap (SIGMA-81). RecordStorageBytes filters to measure ops, so
		// calling it for every applied op is a no-op for create/quota/delete ops.
		if err := s.store.RecordStorageBytes(r.Context(), srv.ID, req.OpID, req.MeasuredBytes, time.Now()); err != nil {
			s.log.Error("record storage bytes", "err", err, "op", req.OpID)
		}
	} else {
		if err := s.store.MarkS3OpFailed(r.Context(), srv.ID, req.OpID, req.Detail); err != nil {
			s.writeStoreErr(w, err, "s3 op status")
			return
		}
	}
	// The op left the open set — re-render so its op drops out of the DSD.
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(srv.OrgID, srv.ID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}
