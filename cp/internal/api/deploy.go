package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// ── Deployments (P1-9) ────────────────────────────────────────────────────────

// handleListDeployments returns a resource's release history newest-first.
func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	deps, err := s.domain.ListDeployments(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"), limit)
	if err != nil {
		s.writeStoreErr(w, err, "list deployments")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": jsonList(deps)})
}

// handleListOrgDeployments returns the org-wide deploy feed: the most recent
// deployments plus the latest per resource, so the dashboard's overview,
// activity feed and per-resource "Last deploy"/"Version" reflect reality in CP
// mode instead of freezing at resource-creation time (SIGMA-161).
func (s *Server) handleListOrgDeployments(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	feed, err := s.domain.ListOrgDeployments(r.Context(), r.PathValue("orgId"), limit)
	if err != nil {
		s.writeStoreErr(w, err, "list org deployments")
		return
	}
	// Both arrays, not a null between them: the mirror sync maps over each of
	// these on every dashboard render, so this is the list whose `null` breaks
	// the most pages at once (SIGMA-337).
	feed.Recent, feed.Latest = jsonList(feed.Recent), jsonList(feed.Latest)
	writeJSON(w, http.StatusOK, feed)
}

// handleRollbackTargets lists the last-N successful releases with a retained
// image — the rebuild-free rollback candidates.
func (s *Server) handleRollbackTargets(w http.ResponseWriter, r *http.Request) {
	targets, err := s.domain.RollbackTargets(r.Context(), r.PathValue("orgId"), r.PathValue("resourceId"), 10)
	if err != nil {
		s.writeStoreErr(w, err, "rollback targets")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"targets": jsonList(targets)})
}

// handleRollback queues a rollback to a prior successful release (by its
// deployment id). The new rollback deployment reuses the target's image, so the
// reconciler renders only the rollout — no clone/build. Project Admin+.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetDeploymentID string `json:"targetDeploymentId"`
	}
	if err := decodeJSON(w, r, &req); err != nil || req.TargetDeploymentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "targetDeploymentId is required"})
		return
	}
	orgID := r.PathValue("orgId")
	dep, serverID, err := s.domain.CreateRollback(r.Context(), orgID, r.PathValue("resourceId"), req.TargetDeploymentID, principalFrom(r).Name)
	if err != nil {
		s.writeStoreErr(w, err, "rollback")
		return
	}
	// The rollback changed the server's deploy target — re-render its DSD so the
	// rollout op ships the retained image.
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusCreated, dep)
}

// handleRedeploy retriggers a resource's deploy UNCONDITIONALLY, in three
// escalating steps:
//
//  1. history to replay → a fresh clone→build→rollout of the same commit;
//  2. no history but a connected repo → resolve the mapped branch's HEAD and
//     deploy that commit (SIGMA-177);
//  3. neither — a database, object storage, a registry-image app, an errored
//     first apply → force a DSD re-issue so the agent re-runs the ops.
//
// Step 2 is what makes the button honest on a brand-new resource. Everything
// that could create a deployment used to read a PREVIOUS one, so a resource
// with a repo connected and a branch mapped still could not be deployed from
// the dashboard: the only way to reach the first build was to push a commit.
// "Redeploy did nothing" must not be a reachable outcome. Project Admin+.
func (s *Server) handleRedeploy(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgId")
	resourceID := r.PathValue("resourceId")
	actor := principalFrom(r).Name
	dep, serverID, err := s.domain.CreateManualRedeploy(r.Context(), orgID, resourceID, actor)
	if err == nil {
		if s.reconcile != nil && serverID != "" {
			s.reconcile.ReconcileAsync(orgID, serverID)
		}
		writeJSON(w, http.StatusCreated, dep)
		return
	}
	var inv store.ErrInvalid
	if !errors.As(err, &inv) {
		s.writeStoreErr(w, err, "redeploy")
		return
	}
	if dep, serverID, ok := s.deployRepoHead(r, orgID, resourceID, actor); ok {
		if s.reconcile != nil && serverID != "" {
			s.reconcile.ReconcileAsync(orgID, serverID)
		}
		writeJSON(w, http.StatusCreated, dep)
		return
	}
	// Not deployed from a repo at all — force a re-apply instead.
	serverID, ferr := s.domain.ForceReapplyResource(r.Context(), orgID, resourceID, actor)
	if ferr != nil {
		s.writeStoreErr(w, ferr, "redeploy (force re-apply)")
		return
	}
	if s.reconcile != nil && serverID != "" {
		s.reconcile.ReconcileAsync(orgID, serverID)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": "", "trigger": "reapply", "status": "queued"})
}

// deployRepoHead queues a deploy of the current head of the branch mapped to
// this resource's environment. Reports ok=false — never an HTTP error — when
// the resource deploys from no repo, when git isn't wired, or when the provider
// can't be reached: each of those is a reason to fall through to the re-apply
// path, not to fail the request.
func (s *Server) deployRepoHead(r *http.Request, orgID, resourceID, actor string) (store.Deployment, string, bool) {
	if s.git == nil || s.inspector == nil {
		return store.Deployment{}, "", false
	}
	origin, err := s.git.HeadDeployOriginForResource(r.Context(), orgID, resourceID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("deploy head: resolve origin", "resource", resourceID, "err", err)
		}
		return store.Deployment{}, "", false
	}
	token, _ := s.git.GitTokenForRepo(r.Context(), orgID, origin.RepoFullName)
	sha, err := s.inspector.BranchHead(r.Context(), origin.RepoFullName, origin.Branch, token)
	if err != nil {
		s.log.Warn("deploy head: branch head", "repo", origin.RepoFullName, "branch", origin.Branch, "err", err)
		return store.Deployment{}, "", false
	}
	dep, serverID, err := s.git.CreateHeadDeployment(r.Context(), orgID, resourceID, actor, origin, sha)
	if err != nil {
		s.log.Warn("deploy head: create deployment", "resource", resourceID, "err", err)
		return store.Deployment{}, "", false
	}
	return dep, serverID, true
}

// deployLogStreamTimeout bounds an SSE log stream so a stuck deployment can't
// hold the connection forever. A var so tests can shorten it.
var deployLogStreamTimeout = 10 * time.Minute

// handleDeployLogs serves a deployment's build/orchestration logs. With
// `Accept: text/event-stream` it streams SSE, emitting each new line and closing
// once the deployment is terminal; otherwise it returns a JSON snapshot
// `{deployment, logs, nextCursor, done}` for cursor polling (?after=<id>). Both
// are org-scoped via GetDeployment (BOLA guard).
func (s *Server) handleDeployLogs(w http.ResponseWriter, r *http.Request) {
	orgID, depID := r.PathValue("orgId"), r.PathValue("deploymentId")
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	dep, err := s.domain.GetDeployment(r.Context(), orgID, depID)
	if err != nil {
		s.writeStoreErr(w, err, "get deployment")
		return
	}

	if r.Header.Get("Accept") == "text/event-stream" {
		s.streamDeployLogs(w, r, orgID, dep, after)
		return
	}

	logs, err := s.domain.DeployLogsSince(r.Context(), depID, after, 1000)
	if err != nil {
		s.writeStoreErr(w, err, "deploy logs")
		return
	}
	cursor := after
	if n := len(logs); n > 0 {
		cursor = logs[n-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deployment": dep,
		"logs":       logs,
		"nextCursor": cursor,
		"done":       terminalDeploymentStatus(dep.Status),
	})
}

// streamDeployLogs pushes log lines over SSE until the deployment reaches a
// terminal status (then a final "done" event), the client disconnects, or the
// stream timeout fires.
func (s *Server) streamDeployLogs(w http.ResponseWriter, r *http.Request, orgID string, dep store.Deployment, after int64) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	deadline := time.NewTimer(deployLogStreamTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	cursor := after
	status := dep.Status
	emit := func() bool {
		logs, err := s.domain.DeployLogsSince(ctx, dep.ID, cursor, 500)
		if err != nil {
			return false
		}
		for _, l := range logs {
			cursor = l.ID
			b, _ := json.Marshal(l)
			fmt.Fprintf(w, "event: log\ndata: %s\n\n", b)
		}
		if len(logs) > 0 {
			flusher.Flush()
		}
		return true
	}

	// Drain what's already there before waiting.
	if !emit() {
		return
	}
	if terminalDeploymentStatus(status) {
		fmt.Fprintf(w, "event: done\ndata: {\"status\":%q}\n\n", status)
		flusher.Flush()
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-tick.C:
			if !emit() {
				return
			}
			// Re-read the deployment status to know when to stop.
			cur, err := s.domain.GetDeployment(ctx, orgID, dep.ID)
			if err == nil {
				status = cur.Status
			}
			if terminalDeploymentStatus(status) {
				emit() // flush any final lines
				fmt.Fprintf(w, "event: done\ndata: {\"status\":%q}\n\n", status)
				flusher.Flush()
				return
			}
		}
	}
}

// terminalDeploymentStatus mirrors the store's frozen-row set for the API layer.
func terminalDeploymentStatus(s string) bool {
	switch s {
	case "success", "failed", "superseded", "rolled_back":
		return true
	}
	return false
}
