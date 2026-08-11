package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// ── Backup targets (P1-11) ──────────────────────────────────────────────────

func (s *Server) handleCreateBackupTarget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string `json:"name"`
		Endpoint       string `json:"endpoint"`
		Bucket         string `json:"bucket"`
		Region         string `json:"region"`
		ForcePathStyle *bool  `json:"forcePathStyle"`
		AccessKey      string `json:"accessKey"`
		SecretKey      string `json:"secretKey"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	forcePath := true
	if req.ForcePathStyle != nil {
		forcePath = *req.ForcePathStyle
	}
	t, err := s.domain.CreateBackupTarget(r.Context(), r.PathValue("orgId"), principalFrom(r).Name, store.CreateBackupTargetInput{
		Name:           strings.TrimSpace(req.Name),
		Endpoint:       strings.TrimSpace(req.Endpoint),
		Bucket:         strings.TrimSpace(req.Bucket),
		Region:         strings.TrimSpace(req.Region),
		ForcePathStyle: forcePath,
		AccessKey:      strings.TrimSpace(req.AccessKey),
		SecretKey:      req.SecretKey,
	})
	if err != nil {
		s.writeStoreErr(w, err, "create backup target")
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) handleListBackupTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.domain.ListBackupTargets(r.Context(), r.PathValue("orgId"))
	if err != nil {
		s.writeStoreErr(w, err, "list backup targets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": jsonList(targets)})
}

func (s *Server) handleDeleteBackupTarget(w http.ResponseWriter, r *http.Request) {
	err := s.domain.DeleteBackupTarget(r.Context(), r.PathValue("orgId"), r.PathValue("targetId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "delete backup target")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ── Backup policy + history ─────────────────────────────────────────────────

func (s *Server) handleUpdateBackupPolicy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetID    *string `json:"targetId"`
		Schedule    *string `json:"schedule"`
		KeepDaily   *int    `json:"keepDaily"`
		KeepWeekly  *int    `json:"keepWeekly"`
		KeepMonthly *int    `json:"keepMonthly"`
		Enabled     *bool   `json:"enabled"`
		PitrEnabled *bool   `json:"pitrEnabled"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	bp, err := s.domain.UpdateBackupPolicy(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"), principalFrom(r).Name, store.UpdateBackupPolicyInput{
		TargetID:    req.TargetID,
		Schedule:    req.Schedule,
		KeepDaily:   req.KeepDaily,
		KeepWeekly:  req.KeepWeekly,
		KeepMonthly: req.KeepMonthly,
		Enabled:     req.Enabled,
		PitrEnabled: req.PitrEnabled,
	})
	if err != nil {
		s.writeDBErr(w, err, "update backup policy")
		return
	}
	writeJSON(w, http.StatusOK, bp)
}

func (s *Server) handleListBackupRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.domain.ListBackupRuns(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"), limit)
	if err != nil {
		s.writeStoreErr(w, err, "list backup runs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": jsonList(runs)})
}

func (s *Server) handleVerifyDays(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	out, err := s.domain.VerifyDays(r.Context(), r.PathValue("orgId"), days)
	if err != nil {
		s.writeStoreErr(w, err, "verify days")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": out})
}

// handleRestoreDatabase is the fire-drill flow: provision a FRESH database
// resource (full P1-10 path: new credentials, port, policy row) and queue a
// restore run that loads the source's latest snapshot into it. Redis restore
// is not supported in v1 (RDB loading needs a coordinated engine restart);
// verify still covers redis integrity via the checksum + redis-check-rdb path.
func (s *Server) handleRestoreDatabase(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		EnvironmentID string `json:"environmentId"`
		ServerID      string `json:"serverId"`
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" || req.EnvironmentID == "" || req.ServerID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, environmentId and serverId are required"})
		return
	}
	orgID, sourceID := r.PathValue("orgId"), r.PathValue("resourceId")
	actor := principalFrom(r).Name

	src, err := s.domain.GetDatabaseInfo(r.Context(), orgID, sourceID)
	if err != nil {
		s.writeDBErr(w, err, "restore database")
		return
	}
	if src.Engine == "redis" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "redis restore-into-new-resource is not supported in v1; restore-verify still checks snapshot integrity",
		})
		return
	}
	res, err := s.domain.CreateResource(r.Context(), orgID, store.CreateResourceInput{
		EnvironmentID: req.EnvironmentID,
		ServerID:      req.ServerID,
		Name:          strings.TrimSpace(req.Name),
		Kind:          src.Engine,
		Spec:          json.RawMessage(`{}`),
		// A recovery target, so the billing cap does not stand between a
		// delinquent org and its own verified backups (SIGMA-348).
		Recovery: true,
	}, actor)
	if err != nil {
		s.writeStoreErr(w, err, "restore: create resource")
		return
	}
	run, err := s.domain.CreateRestoreRun(r.Context(), orgID, sourceID, res.ID, actor)
	if err != nil {
		// The fresh resource stays (visible, empty) — the operator can retry the
		// restore or delete it; silently unwinding a provisioned DB is riskier.
		s.writeStoreErr(w, err, "restore: queue run")
		return
	}
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, res.ServerID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"resource": res, "run": run})
}

// handleRestoreToTimestamp is the PITR flow (P2-5b): provision a FRESH postgres
// resource and queue a restore-pitr run that recovers it to the requested time
// (newest base backup ≤ target, then WAL replay up to recovery_target_time).
// Postgres-only; the store validates the recoverable window before queuing.
func (s *Server) handleRestoreToTimestamp(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string `json:"name"`
		EnvironmentID string `json:"environmentId"`
		ServerID      string `json:"serverId"`
		TargetTime    string `json:"targetTime"` // RFC3339
	}
	if err := decodeJSON(w, r, &req); err != nil || strings.TrimSpace(req.Name) == "" || req.EnvironmentID == "" || req.ServerID == "" || req.TargetTime == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, environmentId, serverId and targetTime are required"})
		return
	}
	targetTime, err := time.Parse(time.RFC3339, req.TargetTime)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "targetTime must be an RFC3339 timestamp"})
		return
	}
	orgID, sourceID := r.PathValue("orgId"), r.PathValue("resourceId")
	actor := principalFrom(r).Name

	src, err := s.domain.GetDatabaseInfo(r.Context(), orgID, sourceID)
	if err != nil {
		s.writeDBErr(w, err, "restore database")
		return
	}
	if src.Engine != "postgres" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "point-in-time recovery is postgres-only",
		})
		return
	}
	res, err := s.domain.CreateResource(r.Context(), orgID, store.CreateResourceInput{
		EnvironmentID: req.EnvironmentID,
		ServerID:      req.ServerID,
		Name:          strings.TrimSpace(req.Name),
		Kind:          src.Engine,
		Spec:          json.RawMessage(`{}`),
		// A recovery target, so the billing cap does not stand between a
		// delinquent org and its own verified backups (SIGMA-348).
		Recovery: true,
	}, actor)
	if err != nil {
		s.writeStoreErr(w, err, "restore: create resource")
		return
	}
	run, err := s.domain.CreateRestoreToTimestampRun(r.Context(), orgID, sourceID, res.ID, targetTime, actor)
	if err != nil {
		// The fresh resource stays (visible, empty) — the operator can retry or
		// delete it; silently unwinding a provisioned DB is riskier.
		s.writeStoreErr(w, err, "restore: queue run")
		return
	}
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(orgID, res.ServerID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"resource": res, "run": run})
}

// ── Agent-facing (P1-11) ────────────────────────────────────────────────────

// handleAgentBackupCredential releases the restic repo key + S3 credentials
// for ONE open run scheduled on the calling agent's server. Every call is
// audited by the store (the per-run key-release invariant).
func (s *Server) handleAgentBackupCredential(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	runID := r.URL.Query().Get("runId")
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "runId is required"})
		return
	}
	cred, err := s.store.BackupCredentialForRun(r.Context(), srv.ID, runID)
	if err != nil {
		s.writeStoreErr(w, err, "backup credential")
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// handleAgentBackupStatus records a run's terminal outcome with its metadata
// (snapshot id, dump sha). BOLA-scoped: only the run's own server may report.
func (s *Server) handleAgentBackupStatus(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	var req struct {
		RunID      string `json:"runId"`
		OK         bool   `json:"ok"`
		SnapshotID string `json:"snapshotId"`
		DumpSha256 string `json:"dumpSha256"`
		Detail     string `json:"detail"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.RunID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "runId is required"})
		return
	}
	if err := s.store.SetBackupRunResult(r.Context(), srv.ID, req.RunID, req.OK, req.SnapshotID, req.DumpSha256, req.Detail); err != nil {
		s.writeStoreErr(w, err, "backup status")
		return
	}
	// The run left the open set — re-render so its op drops out of the DSD.
	if s.reconcile != nil {
		s.reconcile.ReconcileAsync(srv.OrgID, srv.ID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// ── WAL shipping (P2-5) ─────────────────────────────────────────────────────

// handleAgentWALTargets lists the PITR-enabled resources whose spool the
// calling agent should be draining.
func (s *Server) handleAgentWALTargets(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	targets, err := s.store.WALTargetsForServer(r.Context(), srv.ID)
	if err != nil {
		s.writeStoreErr(w, err, "wal targets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": targets})
}

// handleAgentWALCredential releases the restic credential for one resource's
// WAL shipping. Audited per release; the agent caches it for ~50 minutes.
func (s *Server) handleAgentWALCredential(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	resourceID := r.URL.Query().Get("resourceId")
	if resourceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "resourceId is required"})
		return
	}
	cred, err := s.store.WALCredentialForResource(r.Context(), srv.ID, resourceID)
	if err != nil {
		s.writeStoreErr(w, err, "wal credential")
		return
	}
	writeJSON(w, http.StatusOK, cred)
}

// handleAgentWALStatus records a shipping cycle's high-water mark.
func (s *Server) handleAgentWALStatus(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	var req struct {
		ResourceID  string `json:"resourceId"`
		LastSegment string `json:"lastSegment"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.ResourceID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "resourceId is required"})
		return
	}
	if err := s.store.SetWALStatus(r.Context(), srv.ID, req.ResourceID, req.LastSegment, time.Now()); err != nil {
		s.writeStoreErr(w, err, "wal status")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}

// ── Repo-key custody (SIGMA-170) ────────────────────────────────────────────

// handleListArchivedRepoKeys lists the repo keys retained for deleted
// resources: which snapshot sets can still be opened, and when their resource
// went away. Metadata only — no key material.
func (s *Server) handleListArchivedRepoKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.domain.ListArchivedRepoKeys(r.Context(), r.PathValue("orgId"))
	if err != nil {
		s.writeStoreErr(w, err, "list archived repo keys")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": jsonList(keys)})
}

// handleExportRepoKey returns the plaintext restic repository password for a
// resource, live or deleted.
//
// Without it the repo key was only ever handed to an agent for a scheduled run,
// so an operator who deleted a database — or lost the control plane — held a
// bucket full of snapshots they were still paying for and could never open
// (SIGMA-170). Org Admin only, and every export lands in the audit log: handing
// out a decryption key is precisely the event an auditor needs to see.
func (s *Server) handleExportRepoKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.domain.ExportRepoKey(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"), principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "export repo key")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"repoKey": key,
		// Named so the caller knows what to do with it: restic reads the
		// repository password from this environment variable.
		"envVar": "RESTIC_PASSWORD",
	})
}
