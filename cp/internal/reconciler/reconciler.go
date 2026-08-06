// Package reconciler renders per-server Desired-State Documents from resource
// specs and is the ONLY writer of DSDs. It is level-triggered: callers nudge
// it on a mutation, and a background loop resyncs the whole fleet every 60s so
// a missed nudge still converges.
package reconciler

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// Store is the slice of the persistence layer the reconciler needs.
type Store interface {
	ResourceSpecsForServer(ctx context.Context, serverID string) ([]store.ResourceSpec, error)
	PendingDestructiveOpsForServer(ctx context.Context, orgID, serverID string) ([]store.PendingDestructiveOp, error)
	SecretRefsForServer(ctx context.Context, serverID string) (map[string][]store.SecretRefMeta, error)
	HostHardeningForServer(ctx context.Context, serverID string) (store.HostHardening, error)
	DomainsForServer(ctx context.Context, serverID string) (map[string][]store.Domain, error)
	DeployTargetsForServer(ctx context.Context, serverID string) (map[string]store.DeployTarget, error)
	DBTargetsForServer(ctx context.Context, serverID string) (map[string]store.DBTarget, error)
	S3TargetsForServer(ctx context.Context, serverID string) (map[string]store.S3Target, error)
	PendingS3OpsForServer(ctx context.Context, serverID string) ([]store.S3OpSpec, error)
	BackupRunsForServer(ctx context.Context, serverID string) ([]store.BackupRunSpec, error)
	StoreDSD(ctx context.Context, orgID, serverID string, ops []dsd.Op, docHash string, priv ed25519.PrivateKey) (dsd.Signed, bool, error)
	StampDeploymentDSDVersion(ctx context.Context, deploymentIDs []string, version int64) error
	AllServerIDs(ctx context.Context) ([]struct{ ServerID, OrgID string }, error)
	// LockServerReconcile serializes reconciles for one server (SIGMA-94). The
	// bool is false when the lock is already held elsewhere (SIGMA-120) — skip.
	LockServerReconcile(ctx context.Context, serverID string) (func(), bool, error)
}

// Reconciler renders and versions DSDs and notifies long-poll waiters.
type Reconciler struct {
	log  *slog.Logger
	st   Store
	priv ed25519.PrivateKey
	acme ACMEConfig

	mu      sync.Mutex
	waiters map[string][]chan struct{} // serverID -> notify channels
}

func New(log *slog.Logger, st Store, priv ed25519.PrivateKey) *Reconciler {
	return &Reconciler{log: log, st: st, priv: priv, waiters: map[string][]chan struct{}{}}
}

// SetACMEConfig installs the ACME issuance config rendered into proxy.traefik
// ops (Let's Encrypt account email + CA directory; the staging/Pebble URL is
// injected for e2e). Called at boot before serving.
func (r *Reconciler) SetACMEConfig(cfg ACMEConfig) { r.acme = cfg }

// renderOps builds the ordered op list for a server. "app" resources fan into
// container ops (network.ensure → image.pull → volume.ensure → container.apply);
// database kinds (P1-10) render their engine container bound to the mesh
// address; the remaining kinds (s3/llm) keep the P1-2 no-op "resource.sync"
// stub until they are containerised. Confirmed destructive ops are appended as
// volume.remove.
func renderOps(serverID string, specs []store.ResourceSpec, pending []store.PendingDestructiveOp, secretRefs map[string][]store.SecretRefMeta, hardening store.HostHardening, domains map[string][]store.Domain, deployTargets map[string]store.DeployTarget, dbTargets map[string]store.DBTarget, s3Targets map[string]store.S3Target, backupRuns []store.BackupRunSpec, s3Ops []store.S3OpSpec, acme ACMEConfig) ([]dsd.Op, string) {
	networks := map[string]string{} // net op id -> network name (deduped per project)
	var resourceOps []dsd.Op

	for _, rs := range specs {
		if target, isDB := dbTargets[rs.ResourceID]; isDB {
			if dbOps, netID, ok := renderDatabaseOps(rs, target, hardening.MeshIP); ok {
				resourceOps = append(resourceOps, dbOps...)
				networks[netID] = dsd.NetworkName(rs.ProjectID)
				continue
			}
		}
		if target, isS3 := s3Targets[rs.ResourceID]; isS3 {
			if s3Ops, netID, ok := renderS3Ops(rs, target, hardening.MeshIP); ok {
				resourceOps = append(resourceOps, s3Ops...)
				networks[netID] = dsd.NetworkName(rs.ProjectID)
				continue
			}
		}
		if rs.Kind == "app" {
			// A git-deployed app (has a deploy target) renders the build pipeline;
			// a registry-image app keeps the direct container.apply path.
			if target, isGit := deployTargets[rs.ResourceID]; isGit {
				if depOps, netID, ok := renderDeployOps(rs, secretRefs[rs.ResourceID], domains[rs.ResourceID], target); ok {
					resourceOps = append(resourceOps, depOps...)
					// A Compose app deploys onto its own per-resource network
					// ("net:res:<id>"); a single-container app shares the project network.
					if strings.HasPrefix(netID, "net:res:") {
						networks[netID] = dsd.ResourceNetworkName(rs.ResourceID)
					} else {
						networks[netID] = dsd.NetworkName(rs.ProjectID)
					}
					continue
				}
			}
			if appOps, netID, ok := renderAppOps(rs, secretRefs[rs.ResourceID], domains[rs.ResourceID]); ok {
				resourceOps = append(resourceOps, appOps...)
				networks[netID] = dsd.NetworkName(rs.ProjectID)
				continue
			}
		}
		// Not yet containerised (or an app with no image): a no-op stub keeps the
		// resource represented in the DSD.
		//
		// The stub is deliberately NOT a `res:<id>` op. The agent registers
		// resource.sync as an unconditional success, and handleDSDStatus routes a
		// bare `res:<id>` status straight into resources.status — so the stub was
		// reporting "applied" for a resource with nothing running, painting a
		// green "Running" badge on undeployed resources and even OVERWRITING the
		// real failed status of a resource whose only deployment failed
		// (SIGMA-172). Under its own prefix it still keeps the resource in the
		// document (and in whole-document convergence, which reads every op
		// regardless of prefix) without claiming anything about its state.
		stub, _ := json.Marshal(map[string]any{"resourceId": rs.ResourceID, "kind": rs.Kind, "spec": rs.Spec})
		resourceOps = append(resourceOps, dsd.Op{ID: "sync:" + rs.ResourceID, Kind: dsd.KindResourceSync, Spec: stub})
	}

	// One network.ensure op per distinct project, emitted first in a stable
	// order so the rendered document (and thus its hash) is deterministic.
	netIDs := make([]string, 0, len(networks))
	for id := range networks {
		netIDs = append(netIDs, id)
	}
	sort.Strings(netIDs)
	ops := make([]dsd.Op, 0, len(netIDs)+len(resourceOps)+len(pending))
	for _, id := range netIDs {
		ns, _ := json.Marshal(map[string]string{"name": networks[id]})
		ops = append(ops, dsd.Op{ID: id, Kind: dsd.KindNetworkEnsure, Spec: ns})
	}
	ops = append(ops, resourceOps...)
	// Host-hardening ops (P1-5) are appended in a fixed order so the document
	// hash stays deterministic. They have no dependencies (host-level, independent
	// of the container graph).
	ops = append(ops, renderHostOps(serverID, hardening)...)
	// Ingress (P1-8): a proxy-role server runs Traefik. P1-5's nftables op has
	// already opened 80/443 (proxyRole feeds renderHostOps), so this only stands
	// up the proxy + ACME resolver; the router labels ride on the app containers.
	if hardening.ProxyRole {
		var serverDomains []store.Domain
		for _, ds := range domains {
			serverDomains = append(serverDomains, ds...)
		}
		ops = append(ops, renderTraefikOp(serverID, acme, serverDomains))
	}
	// The set of op ids rendered so far (the resource graph), so backup and
	// s3.configure ops can depend on their resource's container op by id when it
	// is present in the same document.
	renderedIDs := make(map[string]bool, len(ops))
	for _, op := range ops {
		renderedIDs[op.ID] = true
	}
	// Backups (P1-11): open runs render as typed ops after the resource graph
	// so a backup op can depend on its database container op by id.
	if len(backupRuns) > 0 {
		ops = append(ops, renderBackupOps(backupRuns, renderedIDs)...)
	}
	// SIGMA-65: on-demand S3 bucket/key/quota/measure ops. Each depends on the
	// resource's container op (SIGMA-73) so it never runs before the engine is up.
	ops = append(ops, renderS3ConfigureOps(s3Ops, renderedIDs)...)
	for _, p := range pending {
		ops = append(ops, renderVolumeRemoveOp(p))
	}
	return ops, dsd.SpecHash(ops)
}

// Reconcile renders a server's DSD; on a real change it bumps the version,
// signs, persists and wakes any long-poll waiter for that server.
func (r *Reconciler) Reconcile(ctx context.Context, orgID, serverID string) error {
	// Serialize reconciles for this server so an older snapshot can't overwrite a
	// newer DSD (SIGMA-94). Held across the reads AND the StoreDSD write. On
	// contention we skip rather than block (SIGMA-120): the holder converges the
	// same state and the 60s resync re-runs anything skipped.
	unlock, acquired, err := r.st.LockServerReconcile(ctx, serverID)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer unlock()
	specs, err := r.st.ResourceSpecsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	pending, err := r.st.PendingDestructiveOpsForServer(ctx, orgID, serverID)
	if err != nil {
		return err
	}
	secretRefs, err := r.st.SecretRefsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	hardening, err := r.st.HostHardeningForServer(ctx, serverID)
	if err != nil {
		return err
	}
	domains, err := r.st.DomainsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	deployTargets, err := r.st.DeployTargetsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	dbTargets, err := r.st.DBTargetsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	s3Targets, err := r.st.S3TargetsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	backupRuns, err := r.st.BackupRunsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	s3Ops, err := r.st.PendingS3OpsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	ops, hash := renderOps(serverID, specs, pending, secretRefs, hardening, domains, deployTargets, dbTargets, s3Targets, backupRuns, s3Ops, r.acme)
	signed, changed, err := r.st.StoreDSD(ctx, orgID, serverID, ops, hash, r.priv)
	if err != nil {
		return err
	}
	if changed {
		// Bind each in-flight deploy target to the version that first rendered it,
		// so a late op-status report from a superseded deployment (an older
		// version) is rejected instead of advancing the newer one (SIGMA-134).
		depIDs := make([]string, 0, len(deployTargets))
		for _, t := range deployTargets {
			if t.DeploymentID != "" {
				depIDs = append(depIDs, t.DeploymentID)
			}
		}
		if err := r.st.StampDeploymentDSDVersion(ctx, depIDs, signed.Document.Version); err != nil {
			r.log.Warn("stamp deployment dsd version", "server", serverID, "err", err)
		}
		r.log.Info("dsd rendered", "server", serverID, "ops", len(ops))
		r.notify(serverID)
	}
	return nil
}

// ReconcileAsync runs Reconcile in the background (fire-and-forget from an API
// handler that already returned) with its own short timeout.
func (r *Reconciler) ReconcileAsync(orgID, serverID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.Reconcile(ctx, orgID, serverID); err != nil {
			r.log.Error("reconcile", "err", err, "server", serverID)
		}
	}()
}

// Run resyncs the whole fleet every interval until ctx is cancelled. Blocks;
// run in a goroutine.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			servers, err := r.st.AllServerIDs(ctx)
			if err != nil {
				r.log.Error("resync: list servers", "err", err)
				continue
			}
			for _, sv := range servers {
				if err := r.Reconcile(ctx, sv.OrgID, sv.ServerID); err != nil {
					r.log.Error("resync: reconcile", "err", err, "server", sv.ServerID)
				}
			}
		}
	}
}

// Wait returns a channel that closes when the server's DSD next changes, plus
// a cancel to release the subscription. Used by the long-poll handler.
func (r *Reconciler) Wait(serverID string) (<-chan struct{}, func()) {
	ch := make(chan struct{})
	r.mu.Lock()
	r.waiters[serverID] = append(r.waiters[serverID], ch)
	r.mu.Unlock()
	cancel := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		list := r.waiters[serverID]
		for i, c := range list {
			if c == ch {
				r.waiters[serverID] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
	return ch, cancel
}

func (r *Reconciler) notify(serverID string) {
	r.mu.Lock()
	list := r.waiters[serverID]
	delete(r.waiters, serverID)
	r.mu.Unlock()
	for _, ch := range list {
		close(ch)
	}
}
