package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// DSDStore is the slice of the store the DSD endpoints need.
type DSDStore interface {
	GetDSD(ctx context.Context, serverID string) (dsd.Signed, error)
	CurrentDSDVersion(ctx context.Context, serverID string) (int64, error)
	ApplyDSDStatus(ctx context.Context, serverID string, version int64, opStatus map[string]json.RawMessage, converged bool, failedOps []string) (bool, error)
	MarkDestructiveOpApplied(ctx context.Context, serverID, id string) error
}

// DSDWaiter lets the long-poll handler block until a server's DSD changes.
type DSDWaiter interface {
	Wait(serverID string) (<-chan struct{}, func())
}

// longPollTimeout bounds how long GET /v1/agent/dsd blocks before returning
// 204; the agent immediately re-requests, so this is just the keepalive
// cadence. Kept well under typical proxy idle timeouts. A var so integration
// tests can shorten it.
var longPollTimeout = 25 * time.Second

// SetLongPollTimeout overrides the DSD long-poll window (tests only).
func SetLongPollTimeout(d time.Duration) { longPollTimeout = d }

// LongPollTimeout reports the current window, so a test that shortens it can
// put back what it found rather than guessing the default.
func LongPollTimeout() time.Duration { return longPollTimeout }

// handleGetDSD is the agent's outbound-only long-poll for its DSD. With
// ?after=<version>, it returns immediately when a newer signed DSD exists,
// otherwise blocks up to longPollTimeout for the next change (204 on timeout).
func (s *Server) handleGetDSD(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)

	for {
		// Subscribe BEFORE reading the version. A notify only wakes waiters
		// that are already registered, so reading first and subscribing second
		// would lose a change committed in that window (the channel is closed
		// against an empty waiter set) and stall delivery until timeout. With
		// this ordering, any change committed after Wait fires our channel, and
		// any change committed before Wait is caught by the read below.
		ch, cancel := s.dsdWaiter.Wait(srv.ID)

		cur, err := s.dsdStore.CurrentDSDVersion(r.Context(), srv.ID)
		if err != nil {
			cancel()
			s.log.Error("dsd version", "err", err, "server", srv.ID)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if cur > after {
			cancel()
			signed, err := s.dsdStore.GetDSD(r.Context(), srv.ID)
			if err != nil {
				s.log.Error("dsd fetch", "err", err, "server", srv.ID)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			writeJSON(w, http.StatusOK, signed)
			return
		}

		select {
		case <-ch:
			cancel()
			// Loop: re-subscribe, re-read version, and return the new DSD.
		case <-time.After(longPollTimeout):
			cancel()
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			cancel()
			return
		}
	}
}

// deployPhaseRank orders the deploy pipeline phases so a batch of op statuses is
// applied clone→build→rollout regardless of map-iteration order.
func deployPhaseRank(phase string) int {
	switch phase {
	case "clone":
		return 0
	case "build":
		return 1
	case "rollout":
		return 2
	}
	return 3
}

// deployPhase maps a deploy-pipeline op id to its (phase, resourceID, service).
// A single-container app uses `<phase>:<res>` (service ""); a Compose service uses
// `<phase>:<res>:<svc>`. The rollout op keeps the res: id so a single-service
// status also routes to resources.status.
func deployPhase(opID string) (phase, resourceID, service string, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(opID, "clone:"):
		phase, rest = "clone", strings.TrimPrefix(opID, "clone:")
	case strings.HasPrefix(opID, "build:"):
		phase, rest = "build", strings.TrimPrefix(opID, "build:")
	case strings.HasPrefix(opID, "res:"):
		phase, rest = "rollout", strings.TrimPrefix(opID, "res:")
	default:
		return "", "", "", false
	}
	// Resource ids carry no colon, so a remaining ":" delimits the service name.
	if i := strings.Index(rest, ":"); i >= 0 {
		return phase, rest[:i], rest[i+1:], true
	}
	return phase, rest, "", true
}

// deployPrereqResource maps a deploy-pipeline PREREQUISITE op to the resource it
// belongs to. These ops — image.pull ("pull:<res>[:<svc>]"), volume.ensure
// ("vol:<res>:<name>") and the per-resource network.ensure ("net:res:<res>") —
// have no deploy PHASE: there is no status to advance TO when a volume is
// created, so deployPhase rejects them and their status only ever flipped the
// document's `converged` boolean (SIGMA-301).
//
// That is fine while they succeed and catastrophic when they don't. A registry
// 401 on the cross-host pull, a named volume colliding with an unmanaged one, a
// host out of disk: each fails one of these ops, the agent skips the rollout that
// depends on it, and the ONLY thing that reached the deployment was the skip. The
// operator saw "failed — prerequisite failed" with an empty build log — the
// docker daemon's actual message existed nowhere in the control plane.
//
// So a failure here is routed to the resource's in-flight deployment explicitly:
// one deploy-log line plus the terminal failure detail, both carrying the op's own
// error. The project-wide "net:<proj>" op is deliberately NOT mapped — it belongs
// to every resource in the project and to none of them in particular; its failure
// reaches the deployment through the agent-side skip message, which since
// SIGMA-301 names the op and quotes its error.
func deployPrereqResource(opID string) (resourceID string, ok bool) {
	var rest string
	switch {
	case strings.HasPrefix(opID, "pull:"):
		rest = strings.TrimPrefix(opID, "pull:")
	case strings.HasPrefix(opID, "vol:"):
		rest = strings.TrimPrefix(opID, "vol:")
	case strings.HasPrefix(opID, "net:res:"):
		rest = strings.TrimPrefix(opID, "net:res:")
	default:
		return "", false
	}
	// Trailing ":<service>" / ":<volume name>" is not part of the resource id.
	if i := strings.Index(rest, ":"); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}

type dsdStatusRequest struct {
	Version int64 `json:"version"`
	// Ops maps op id -> reported status object; resource.sync ops carry the
	// resource id in the op id ("res:<id>"), so the CP can route status into
	// resources.status.
	Ops map[string]json.RawMessage `json:"ops"`
}

func (s *Server) handleDSDStatus(w http.ResponseWriter, r *http.Request) {
	srv := serverFrom(r)
	var req dsdStatusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	// Advance the in-flight deployment (P1-9) as its pipeline ops report, in
	// deterministic phase order (clone→build→rollout) so a single status POST
	// carrying several ops in arbitrary map order yields a stable outcome. The
	// store transition is independently monotonic; ordering here just fixes the
	// recorded detail/timing. A no-op for non-git resources (no in-flight deploy).
	type deployAdvance struct {
		phase, resID, service, errText string
		ok                             bool
	}
	var advances []deployAdvance
	// Route "res:<id>" op statuses into resources.status, and mark applied
	// "volrm:<pendingId>" destructive ops so they drop out of future DSDs.
	byResource := map[string]json.RawMessage{}
	// composeAgg collects a Compose app's per-service rollout statuses so a
	// synthetic per-resource status still lands in resources.status (the agent
	// reports a full version's ops in one POST, so the aggregate is coherent).
	type opStatus struct {
		State string `json:"state"`
		Error string `json:"error,omitempty"`
	}
	composeAgg := map[string][]opStatus{}
	// Failed deploy-pipeline prerequisite ops (pull:/vol:/net:res:) keyed by op
	// id, in the deterministic order the op ids sort in, so a batch carrying
	// several of them always reports the same one first (SIGMA-301).
	type prereqFailure struct{ opID, resID, errText string }
	var prereqFailures []prereqFailure
	// Whole-document convergence (SIGMA-117): the version is converged only if
	// EVERY reported op applied. Computed from the full req.Ops set — including
	// host:*, proxy, and volrm: ops that never enter byResource — so a failed
	// non-resource op still clears apply_ok and triggers the SIGMA-104 re-drive.
	converged := true
	// The ids behind that boolean (SIGMA-247). A host/proxy/agent op has no
	// resource to route its status into, so before this its only trace anywhere
	// was flipping `converged` to false — an operator could see that a machine
	// had stopped converging but never which op was refusing to apply. Collected
	// here (the only place that sees the full op set) and stored alongside
	// apply_ok.
	var failedOps []string
	for opID, st := range req.Ops {
		var os struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(st, &os)
		if os.State == "failed" || os.State == "skipped" {
			converged = false
			failedOps = append(failedOps, opID)
		}
		if phase, resID, service, isDeploy := deployPhase(opID); isDeploy && os.State != "" {
			advances = append(advances, deployAdvance{phase: phase, resID: resID, service: service, ok: os.State == "applied", errText: os.Error})
		}
		if os.State == "failed" {
			if resID, isPrereq := deployPrereqResource(opID); isPrereq {
				prereqFailures = append(prereqFailures, prereqFailure{opID: opID, resID: resID, errText: os.Error})
			}
		}
		switch {
		case strings.HasPrefix(opID, "res:"):
			// A Compose service op (res:<res>:<svc>) is not a resource id — only a
			// bare res:<res> routes to resources.status directly; service ops are
			// aggregated into a synthetic per-resource status below.
			if key := strings.TrimPrefix(opID, "res:"); !strings.Contains(key, ":") {
				byResource[key] = st
			} else {
				resID := key[:strings.Index(key, ":")]
				composeAgg[resID] = append(composeAgg[resID], opStatus{State: os.State, Error: os.Error})
			}
		case strings.HasPrefix(opID, "volrm:"), strings.HasPrefix(opID, "k8srm:"):
			// Both are pending_destructive_ops rows — a Docker volume removal and a
			// cluster workload teardown (SIGMA-312) — so both retire the same way:
			// mark the row applied and it stops being rendered.
			var os struct {
				State string `json:"state"`
			}
			if json.Unmarshal(st, &os) == nil && os.State == "applied" {
				pdoID := opID[strings.IndexByte(opID, ':')+1:]
				if err := s.dsdStore.MarkDestructiveOpApplied(r.Context(), srv.ID, pdoID); err != nil {
					s.log.Error("mark destructive op applied", "err", err, "op", opID)
				}
			}
		case strings.HasPrefix(opID, "bkr:"):
			// Backup runs (P1-11): the dedicated /v1/agent/backup-status report is
			// authoritative (it carries snapshot id + dump sha). This is the safety
			// net for op-level failures (dependency skip, unknown kind) so a run
			// whose handler never executed still lands terminal-failed.
			if os.State == "failed" {
				if err := s.store.FailBackupRunFromOpStatus(r.Context(), srv.ID, strings.TrimPrefix(opID, "bkr:"), os.Error); err != nil {
					s.log.Error("fail backup run from op status", "err", err, "op", opID)
				}
			}
		case strings.HasPrefix(opID, "s3cfg:"):
			// S3 ops (SIGMA-65): the dedicated /v1/agent/s3-op-status report is the
			// authoritative success path (it carries measured bytes). This is the
			// honest fallback for op-level failures (dependency skip, unknown
			// action) so an op whose handler never executed lands terminal-failed.
			if os.State == "failed" {
				if err := s.store.FailS3OpFromOpStatus(r.Context(), srv.ID, strings.TrimPrefix(opID, "s3cfg:"), os.Error); err != nil {
					s.log.Error("fail s3 op from op status", "err", err, "op", opID)
				}
			}
		}
	}
	// Prerequisite failures are applied BEFORE the phase advances, and that
	// ordering is the whole point: the rollout op that depended on the broken
	// prerequisite reports as skipped in this same batch, and a deployment
	// freezes on its FIRST terminal transition. Going first means the detail the
	// operator reads is the docker daemon's own message ("denied: requested
	// access to the resource is denied") rather than the dependent op's second-
	// hand account of it (SIGMA-301).
	sort.SliceStable(prereqFailures, func(i, j int) bool { return prereqFailures[i].opID < prereqFailures[j].opID })
	for _, p := range prereqFailures {
		if err := s.store.FailDeploymentFromPrereqOp(r.Context(), srv.ID, p.resID, p.opID, p.errText, req.Version); err != nil {
			s.log.Error("fail deployment from prerequisite op", "err", err, "op", p.opID, "resource", p.resID)
		}
	}
	sort.SliceStable(advances, func(i, j int) bool {
		return deployPhaseRank(advances[i].phase) < deployPhaseRank(advances[j].phase)
	})
	for _, a := range advances {
		if a.service == "" {
			// Single-container app (or the shared clone of a Compose deploy).
			// req.Version lets the store reject a report from a superseded
			// deployment (older DSD version) instead of advancing the newer one
			// (SIGMA-134).
			if err := s.store.AdvanceDeploymentForResource(r.Context(), srv.ID, a.resID, a.phase, a.ok, a.errText, req.Version); err != nil {
				s.log.Error("advance deployment", "err", err, "phase", a.phase, "resource", a.resID)
			}
			continue
		}
		// A Compose service op: track per-service status; the deployment flips to
		// success only when every service succeeds (failed the moment one does).
		if err := s.store.AdvanceDeploymentService(r.Context(), srv.ID, a.resID, a.service, a.phase, a.ok, a.errText, req.Version); err != nil {
			s.log.Error("advance deployment service", "err", err, "phase", a.phase, "resource", a.resID, "service", a.service)
		}
	}
	// Re-render the peers a multi-machine deploy holds back. The build lives in
	// the build server's document, a placed Compose service in its host's, a
	// cluster workload in the control plane's — and each is gated on the
	// deployment reaching the step this report just delivered. Without the nudge
	// they would sit until the next 60s resync, once per stage.
	//
	// Deduped and ordered so a batch carrying several ops of one resource
	// produces one nudge per peer, deterministically.
	seenRes := map[string]bool{}
	var advanced []string
	for _, a := range advances {
		if !seenRes[a.resID] {
			seenRes[a.resID] = true
			advanced = append(advanced, a.resID)
		}
	}
	s.nudgeDeployPeers(r.Context(), srv.ID, advanced)
	// Synthesize a per-resource status for Compose apps from their service ops:
	// failed if any service failed (first error carried), applied only when every
	// reported service applied. Never overrides a direct bare res:<id> status.
	for resID, sts := range composeAgg {
		if _, ok := byResource[resID]; ok {
			continue
		}
		agg := opStatus{State: "applied"}
		for _, st := range sts {
			if st.State != "applied" {
				agg = opStatus{State: st.State, Error: st.Error}
				break
			}
		}
		if b, err := json.Marshal(agg); err == nil {
			byResource[resID] = b
		}
	}
	// A prerequisite failure with no reported op for its resource still has to
	// show on the resource card (SIGMA-301). Normally the dependent res: op
	// reports skipped and carries the story, but a prerequisite can also break
	// for a resource whose ops are all held back this version — and then the
	// card would sit on its last good state while the deployment says failed.
	// Never overrides a status the agent actually reported.
	for _, p := range prereqFailures {
		if _, ok := byResource[p.resID]; ok {
			continue
		}
		if b, err := json.Marshal(opStatus{State: "failed", Error: p.opID + ": " + p.errText}); err == nil {
			byResource[p.resID] = b
		}
	}
	applied, err := s.dsdStore.ApplyDSDStatus(r.Context(), srv.ID, req.Version, byResource, converged, failedOps)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no DSD for this server"})
		return
	}
	if err != nil {
		s.log.Error("dsd status", "err", err, "server", srv.ID)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": applied})
}

// nudgeDeployPeers re-renders the other servers a resource's deploy spans.
// Best-effort: a failure here only costs the 60s resync, so it is logged rather
// than failing the agent's status POST.
func (s *Server) nudgeDeployPeers(ctx context.Context, reportingServerID string, resourceIDs []string) {
	if s.reconcile == nil || len(resourceIDs) == 0 {
		return
	}
	for _, resID := range resourceIDs {
		peers, err := s.store.DeployPeersForResource(ctx, resID, reportingServerID)
		if err != nil {
			s.log.Error("deploy peers", "err", err, "resource", resID)
			continue
		}
		for _, p := range peers {
			s.reconcile.ReconcileAsync(p.OrgID, p.ServerID)
		}
	}
}

// dsdPublicKeyB64 is the base64 of the CP's DSD-signing public key, served in
// the register response so agents can pin it.
func (s *Server) dsdPublicKeyB64() string {
	if s.dsdPub == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(s.dsdPub)
}

var _ = ed25519.PublicKey(nil)
