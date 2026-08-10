// Package reconciler renders per-server Desired-State Documents from resource
// specs and is the ONLY writer of DSDs. It is level-triggered: callers nudge
// it on a mutation, and a background loop resyncs the whole fleet every 60s so
// a missed nudge still converges.
package reconciler

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// Store is the slice of the persistence layer the reconciler needs.
type Store interface {
	ResourceSpecsForServer(ctx context.Context, serverID string) ([]store.ResourceSpec, error)
	// Kubernetes: a server's cluster membership, and the workloads deployed into
	// that cluster (rendered only into the control-plane node's document).
	ClusterMembershipForServer(ctx context.Context, serverID string) (store.ClusterMembership, bool, error)
	ResourceSpecsForCluster(ctx context.Context, clusterID string) ([]store.ResourceSpec, error)
	ClusterBuildSpecsForServer(ctx context.Context, serverID string) ([]store.ResourceSpec, error)
	DeployTargetForResource(ctx context.Context, resourceID string) (store.DeployTarget, error)
	// ImageRepositoryForOrg is the registry prefix cross-host image tags carry;
	// empty when the org has configured no registry.
	ImageRepositoryForOrg(ctx context.Context, orgID string) (string, error)
	PendingDestructiveOpsForServer(ctx context.Context, orgID, serverID string) ([]store.PendingDestructiveOp, error)
	SecretRefsForServer(ctx context.Context, serverID string) (map[string][]store.SecretRefMeta, error)
	HostHardeningForServer(ctx context.Context, serverID string) (store.HostHardening, error)
	DomainsForServer(ctx context.Context, serverID string) (map[string][]store.Domain, error)
	DeployTargetsForServer(ctx context.Context, serverID string) (map[string]store.DeployTarget, error)
	DBTargetsForServer(ctx context.Context, serverID string) (map[string]store.DBTarget, error)
	S3TargetsForServer(ctx context.Context, serverID string) (map[string]store.S3Target, error)
	LLMTargetsForServer(ctx context.Context, serverID string) (map[string]store.LLMTarget, error)
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
	// publicURL is this control plane's own base URL (CP_PUBLIC_URL). It is
	// rendered into agent.update so a self-updating agent downloads through this
	// control plane's /dl proxy instead of github.com (SIGMA-262). Empty when the
	// deployment has not set one, in which case the op omits the base and the
	// agent falls back to the public release repo.
	publicURL string

	mu      sync.Mutex
	waiters map[string][]chan struct{} // serverID -> notify channels

	// bus fans DSD changes to the long-poll waiters parked in OTHER control
	// plane replicas (SIGMA-291). Nil — the default, and every unit test — means
	// this process is the only one, and notify wakes local waiters only.
	bus ChangeBus

	// slots bounds how many reconciles run CONCURRENTLY (SIGMA-288). A buffered
	// channel used as a counting semaphore; nil disables the bound, which is
	// only the case for a zero-value Reconciler built outside New.
	slots chan struct{}

	// Observability hooks (SIGMA-248), both optional and both nil in tests.
	// onResync reports each fleet-resync pass's outcome so a wedged resync is
	// visible from outside the process; onRender times a single server's render,
	// which sits on the agent's poll path and is therefore fleet-wide latency
	// rather than one server's.
	onResync func(error)
	onRender func(time.Duration)
	// onResyncPass times a whole fleet-resync pass (SIGMA-320). Distinct from
	// onResync, which only says whether a pass converged: the resync IS the
	// drift-repair SLO the agent advertises ("a change lands within 60s"), and
	// the way that SLO dies is not an error but a pass that quietly takes four
	// minutes because the fleet grew. Nothing measured it, so the degradation
	// from 60s to 20 minutes produced no signal anywhere and the first symptom
	// was a customer noticing.
	onResyncPass func(time.Duration)
}

func New(log *slog.Logger, st Store, priv ed25519.PrivateKey) *Reconciler {
	return &Reconciler{
		log:     log,
		st:      st,
		priv:    priv,
		waiters: map[string][]chan struct{}{},
		slots:   make(chan struct{}, reconcileConcurrency),
	}
}

const (
	// reconcileConcurrency caps in-flight reconciles (SIGMA-288).
	//
	// ONE reconcile needs TWO pool connections at the same time: it checks a
	// connection out of the pool for its session-scoped advisory lock and holds
	// it for the whole pass (LockServerReconcile), and every read after that
	// asks the pool for a second one. Nothing bounded the fan-out: reconcileOrg
	// spawns a ReconcileAsync per server in the org, the backup scheduler and
	// the deploy drain fan out the same way, and each one is a bare goroutine.
	//
	// An org with more servers than the pool has connections therefore wedged
	// the whole process. With a pool of 20 and 25 servers, 20 goroutines won a
	// connection for their lock, 5 queued for one, and then all 20 winners
	// queued for the second connection they needed to read anything — every
	// connection held by a goroutine waiting for a connection. No reconcile
	// converged, and because the pool is shared, every concurrent HTTP handler
	// (agent heartbeats, DSD long-polls, dashboard reads) blocked too until the
	// 10s ReconcileAsync timeouts unwound it. Bigger orgs made it worse.
	//
	// 4 is deliberately far below the pool floor of 20 (store.Open), so the
	// worst case is 8 connections on reconciles and the rest stays available to
	// request handling. Throughput is not the constraint — a render is a
	// handful of milliseconds, so a 25-server org still re-renders in well
	// under a second.
	//
	// It also sizes the fleet resync's worker pool (SIGMA-320): the resync
	// competes for the same connections, so widening the pass means using these
	// slots properly rather than adding a second, unaccounted bound.
	reconcileConcurrency = 4
	// reconcileQueueWait bounds how long a queued ReconcileAsync waits for a
	// slot before giving up. Fire-and-forget goroutines must not pile up
	// without limit, and dropping is safe: reconcile is level-triggered and the
	// 60s fleet resync re-renders anything skipped.
	reconcileQueueWait = 2 * time.Minute
)

// acquireSlot takes one of the bounded reconcile slots, blocking until one is
// free, ctx is done, or the queue wait expires. It returns a release func and
// whether the slot was taken; call release (deferred) only when ok is true.
func (r *Reconciler) acquireSlot(ctx context.Context) (func(), bool) {
	if r.slots == nil {
		return func() {}, true
	}
	timer := time.NewTimer(reconcileQueueWait)
	defer timer.Stop()
	select {
	case r.slots <- struct{}{}:
		return func() { <-r.slots }, true
	case <-ctx.Done():
		return nil, false
	case <-timer.C:
		return nil, false
	}
}

// SetObservers installs the resync heartbeat and the render timer (SIGMA-248).
// Called at boot before Run; either may be nil.
func (r *Reconciler) SetObservers(onResync func(error), onRender func(time.Duration)) {
	r.onResync, r.onRender = onResync, onRender
}

// SetResyncPassObserver installs the fleet-resync pass timer (SIGMA-320).
// Additive to SetObservers so existing callers need no change; optional and nil
// in most tests.
func (r *Reconciler) SetResyncPassObserver(fn func(time.Duration)) { r.onResyncPass = fn }

// ChangeBus carries DSD change announcements between control-plane replicas
// (SIGMA-291). cp/internal/store implements it over Postgres LISTEN/NOTIFY; the
// receiving side of the same bus drives WakeServer.
type ChangeBus interface {
	PublishDSDChange(ctx context.Context, serverID string) error
}

// SetChangeBus installs the cross-replica wake-up bus. Called at boot before
// serving; leaving it unset is the correct single-instance configuration.
func (r *Reconciler) SetChangeBus(bus ChangeBus) { r.bus = bus }

// SetACMEConfig installs the ACME issuance config rendered into proxy.traefik
// ops (Let's Encrypt account email + CA directory; the staging/Pebble URL is
// injected for e2e). Called at boot before serving.
func (r *Reconciler) SetACMEConfig(cfg ACMEConfig) { r.acme = cfg }

// SetPublicURL installs this control plane's own public base URL, which
// agent.update ops carry so the agent self-updates through the /dl proxy
// (SIGMA-262). Called at boot before serving; empty is supported and means
// "this control plane cannot name itself", so the op omits the base.
func (r *Reconciler) SetPublicURL(u string) {
	r.publicURL = strings.TrimRight(strings.TrimSpace(u), "/")
}

// clusterRender is a server's Kubernetes context for one render pass.
type clusterRender struct {
	member     bool
	membership store.ClusterMembership
	// workloads are the resources deployed into the cluster. Populated only for
	// the control-plane node, which is the single applier.
	workloads []store.ResourceSpec
	// builds are cluster workloads whose images THIS server compiles. A cluster
	// resource belongs to no server, so without this its clone+build ops would
	// land in no document at all and the manifests would reference images
	// nothing had ever been asked to produce. Independent of membership: a build
	// server usually is not in the cluster it builds for.
	builds []store.ResourceSpec
}

// registryRender is the org's image registry for one render pass. Cross-host
// images (a dedicated build server, every cluster workload) are qualified with
// the repository; the host rides along so the agent knows to authenticate.
// Both empty when no registry is configured.
type registryRender struct {
	repository string
	host       string
}

// renderOps builds the ordered op list for a server. "app" resources fan into
// container ops (network.ensure → image.pull → volume.ensure → container.apply);
// database kinds (P1-10) render their engine container bound to the mesh
// address; the remaining kinds (s3/llm) keep the P1-2 no-op "resource.sync"
// stub until they are containerised. Confirmed destructive ops are appended as
// volume.remove.
func renderOps(serverID string, specs []store.ResourceSpec, pending []store.PendingDestructiveOp, secretRefs map[string][]store.SecretRefMeta, hardening store.HostHardening, domains map[string][]store.Domain, deployTargets map[string]store.DeployTarget, dbTargets map[string]store.DBTarget, s3Targets map[string]store.S3Target, llmTargets map[string]store.LLMTarget, backupRuns []store.BackupRunSpec, s3Ops []store.S3OpSpec, acme ACMEConfig, cluster clusterRender, registry registryRender, publicURL string) ([]dsd.Op, string) {
	// A server being decommissioned renders ONE op and nothing else — see
	// renderUninstallOps for why the teardown cannot share a document with the
	// state it is tearing down. Checked before anything is rendered so no other
	// branch can add to it.
	if hardening.Decommissioning {
		ops := renderUninstallOps(serverID, hardening)
		return ops, dsd.SpecHash(ops)
	}
	networks := map[string]string{} // net op id -> network name (deduped per project)
	var resourceOps []dsd.Op

	// Kubernetes membership comes first: every cluster workload depends on this
	// node being up, and a worker that hasn't joined has nothing to schedule.
	nodeOpID := ""
	if cluster.member {
		if nodeOp, ok := renderClusterNodeOp(cluster.membership, hardening.MeshIP); ok {
			resourceOps = append(resourceOps, nodeOp)
			nodeOpID = nodeOp.ID
		}
	}
	// Cluster workloads render into the CONTROL-PLANE node only: kubectl talks
	// to the API server and the scheduler picks the node, so rendering them per
	// node would create competing appliers of the same Deployment.
	if cluster.member && cluster.membership.Role == store.NodeRoleControlPlane && nodeOpID != "" {
		for _, rs := range cluster.workloads {
			wl, ok := renderClusterWorkloadOps(rs, secretRefs[rs.ResourceID], domains[rs.ResourceID],
				deployTargets[rs.ResourceID], cluster.membership, nodeOpID, registry.repository, registry.host)
			if ok {
				resourceOps = append(resourceOps, wl...)
			}
		}
	}
	// Builds for cluster workloads. This server is the build server, which is a
	// different job from being a node — it is usually not in the cluster at all.
	for _, rs := range cluster.builds {
		if bops, ok := renderClusterBuildOps(rs, deployTargets[rs.ResourceID], registry.repository); ok {
			resourceOps = append(resourceOps, bops...)
		}
	}

	for _, rs := range specs {
		// A cluster workload reaches this node's spec read (its secrets and
		// domains have to), but Kubernetes runs it — on whichever node the
		// scheduler chose. Rendering it here as a plain container would put a
		// second, unscheduled copy on EVERY node of the cluster, under the same
		// `res:<id>` op id the control plane's k8s.apply already uses.
		if rs.ClusterID != "" {
			continue
		}
		if target, isDB := dbTargets[rs.ResourceID]; isDB {
			if dbOps, netID, ok := renderDatabaseOps(rs, target, hardening.MeshIP); ok {
				resourceOps = append(resourceOps, dbOps...)
				networks[netID] = dsd.NetworkName(rs.ProjectID)
				continue
			}
		}
		if target, isLLM := llmTargets[rs.ResourceID]; isLLM {
			if llmOps, netID, ok := renderLLMOps(rs, target, hardening.MeshIP, secretRefs[rs.ResourceID]); ok {
				resourceOps = append(resourceOps, llmOps...)
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
				if depOps, netID, ok := renderDeployOps(rs, secretRefs[rs.ResourceID], domains[rs.ResourceID], target, serverID, registry); ok {
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
		//
		// The stub also carries a `retain` list: the (resource, service) container
		// groups this server is supposed to be running for the resource. The agent
		// builds its GC keep-set from the document and runs GC BEFORE the ops, so a
		// resource whose rollout the renderer deliberately held back — a dedicated
		// build server still building, a Compose service gated on a remote
		// dependency — had its LIVE container reaped as an orphan and the app went
		// down for the length of the hold (SIGMA-230). Retention is stated
		// explicitly here so it does not depend on the renderer having emitted a
		// rollout op this pass.
		stubSpec := map[string]any{"resourceId": rs.ResourceID, "kind": rs.Kind, "spec": rs.Spec}
		if target, isGit := deployTargets[rs.ResourceID]; isGit {
			if retain := retainedContainerGroups(rs, target, serverID); len(retain) > 0 {
				stubSpec["retain"] = retain
			}
		}
		stub, _ := json.Marshal(stubSpec)
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
	ops = append(ops, renderHostOps(serverID, hardening, publicURL)...)
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
		// A confirmed destructive op is not always a volume: a deleted cluster
		// workload queues the removal of its Kubernetes manifests against the
		// control-plane node (SIGMA-312). Rendering that row as a volume.remove
		// would ask the agent to delete a Docker volume named after a Deployment,
		// and leave the workload itself running.
		if p.OpKind == dsd.KindK8sRemove {
			ops = append(ops, renderK8sRemoveOp(p))
			continue
		}
		ops = append(ops, renderVolumeRemoveOp(p))
	}
	return ops, dsd.SpecHash(ops)
}

// registryHost is the authentication host of a repository prefix — everything
// before the first slash. `ghcr.io/acme` authenticates against ghcr.io; a
// bare-host repository authenticates against itself.
func registryHost(repository string) string {
	if i := strings.Index(repository, "/"); i > 0 {
		return repository[:i]
	}
	return repository
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
	// Kubernetes context. A server in no cluster renders exactly as before.
	var cluster clusterRender
	membership, isMember, err := r.st.ClusterMembershipForServer(ctx, serverID)
	if err != nil {
		return err
	}
	if isMember {
		cluster.member = true
		cluster.membership = membership
		if membership.Role == store.NodeRoleControlPlane {
			workloads, werr := r.st.ResourceSpecsForCluster(ctx, membership.ClusterID)
			if werr != nil {
				return werr
			}
			cluster.workloads = workloads
			// Cluster workloads need their deploy targets, secrets and domains
			// too; they are keyed by resource id, so merging is enough.
			for _, w := range workloads {
				if t, terr := r.st.DeployTargetForResource(ctx, w.ResourceID); terr == nil && t.DeploymentID != "" {
					deployTargets[w.ResourceID] = t
				}
			}
		}
	}
	// Cluster workloads this server BUILDS for. Unrelated to membership, and
	// their deploy targets are keyed by resource id like any other.
	clusterBuilds, err := r.st.ClusterBuildSpecsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	cluster.builds = clusterBuilds
	for _, b := range clusterBuilds {
		if _, have := deployTargets[b.ResourceID]; have {
			continue
		}
		if t, terr := r.st.DeployTargetForResource(ctx, b.ResourceID); terr == nil && t.DeploymentID != "" {
			deployTargets[b.ResourceID] = t
		}
	}
	// The org's image registry: what qualifies every cross-host image tag.
	repository, err := r.st.ImageRepositoryForOrg(ctx, orgID)
	if err != nil {
		return err
	}
	registry := registryRender{repository: repository, host: registryHost(repository)}
	dbTargets, err := r.st.DBTargetsForServer(ctx, serverID)
	if err != nil {
		return err
	}
	llmTargets, err := r.st.LLMTargetsForServer(ctx, serverID)
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
	renderStart := time.Now()
	ops, hash := renderOps(serverID, specs, pending, secretRefs, hardening, domains, deployTargets, dbTargets, s3Targets, llmTargets, backupRuns, s3Ops, r.acme, cluster, registry, r.publicURL)
	if r.onRender != nil {
		r.onRender(time.Since(renderStart))
	}
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
		r.notify(ctx, serverID)
	}
	return nil
}

// safeReconcile is Reconcile with a recover, so a panic quarantines ONE server
// instead of the fleet (SIGMA-250).
//
// This is the only thing standing between a single malformed row and total
// unavailability. HTTP handler panics are caught by net/http; the background
// loops are not covered by anything, and the fleet resync walks every server in
// one goroutine. A compose service graph of an unexpected shape, a cluster
// membership with an empty role — anything that makes a render helper deref nil
// or index past the end — killed the process for every tenant. Worse, it was
// repeatable: the supervisor restarts, the resync reaches the same row 60
// seconds later, and it dies again, so every org loses the dashboard, deploys,
// backups, alerting and agent long-polls in a restart loop driven by one bad
// row.
//
// The recover sits OUTSIDE Reconcile so Reconcile's own defers — most
// importantly the advisory-lock unlock — still run as the stack unwinds; a
// leaked lock would wedge that server's reconciles for good.
//
// The panic becomes an ordinary error, which means the caller treats it like
// any other per-server failure: Run logs it and moves to the next server, and
// the pass is reported as failed so the resync's last-success clock (SIGMA-248)
// goes stale rather than a quarantined server being silently skipped. The stack
// is logged with the org and server, because "the CP is fine but srv_x never
// converges" is otherwise a very hard thing to explain.
func (r *Reconciler) safeReconcile(ctx context.Context, orgID, serverID string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.log.Error("reconcile panic — server quarantined, fleet continues",
				"org", orgID, "server", serverID, "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic reconciling server %s: %v", serverID, p)
		}
	}()
	return r.Reconcile(ctx, orgID, serverID)
}

// ReconcileAsync runs Reconcile in the background (fire-and-forget from an API
// handler that already returned) with its own short timeout.
//
// Callers fan this out over a whole org (reconcileOrg on a registry change, the
// backup scheduler, the deploy drain), so it waits for one of the bounded
// reconcile slots first (SIGMA-288). The slot is taken BEFORE the 10s timeout
// starts, so time spent queueing behind other reconciles is not charged against
// the reconcile's own budget — otherwise the bound would just convert pool
// exhaustion into a wave of timeouts.
func (r *Reconciler) ReconcileAsync(orgID, serverID string) {
	go func() {
		release, ok := r.acquireSlot(context.Background())
		if !ok {
			// Safe to drop: reconcile is level-triggered and the 60s fleet
			// resync re-renders this server. Logged because a queue this long
			// means the CP is not keeping up with its own mutations.
			r.log.Warn("reconcile dropped: no slot within queue wait, resync will converge", "server", serverID)
			return
		}
		defer release()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Recovered for the same reason as the resync, and more urgently: this
		// goroutine has no caller left to return an error to, so an unrecovered
		// panic here is a process exit triggered by whatever mutation an API
		// handler just accepted.
		if err := r.safeReconcile(ctx, orgID, serverID); err != nil {
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
			passStart := time.Now()
			passErr := r.resyncPass(ctx)
			r.reportResyncPass(time.Since(passStart), interval)
			r.reportResync(passErr)
		}
	}
}

// resyncPass reconciles the whole fleet once, with the reconcile semaphore's
// worth of servers in flight at a time, and returns the pass's verdict.
//
// It used to be an inline serial loop: take a slot, reconcile, release, move to
// the next server (SIGMA-320). That cost N x one-reconcile no matter how much
// headroom the semaphore had — the bound from SIGMA-288 was being used as a
// serialiser rather than as a ceiling. A reconcile is a session advisory lock
// plus ~15 sequential round trips, so at a few thousand hosts the pass outran
// the 60s tick, time.Ticker dropped the ticks that arrived while it was still
// running, and the fleet's real drift-repair interval quietly became the pass
// duration. Everything downstream is written assuming it is 60s: the agent
// advertises that as its convergence SLO, and Reconcile's own lock-contention
// path drops work on the explicit promise that "the 60s resync re-runs anything
// skipped".
//
// The workers share the SAME semaphore ReconcileAsync uses, so a wide resync
// still cannot starve the pool of the connections request handlers need — the
// pass gets faster without the ceiling moving. Servers are claimed off a shared
// index rather than pre-partitioned, so one slow server delays only itself.
func (r *Reconciler) resyncPass(ctx context.Context) error {
	servers, err := r.st.AllServerIDs(ctx)
	if err != nil {
		r.log.Error("resync: list servers", "err", err)
		return err
	}
	if len(servers) == 0 {
		return nil
	}
	workers := reconcileConcurrency
	if workers > len(servers) {
		workers = len(servers)
	}

	// The pass's verdict for the heartbeat (SIGMA-248): a server that failed to
	// reconcile. A resync that could not converge some of the fleet has not done
	// its job, and dating it as a success would hide exactly the state worth
	// alerting on. Which of several failures is kept is not meaningful — the
	// verdict is read as a boolean — so it is simply the first one recorded.
	var (
		mu      sync.Mutex
		passErr error
		next    atomic.Int64
		wg      sync.WaitGroup
	)
	fail := func(e error) {
		mu.Lock()
		if passErr == nil {
			passErr = e
		}
		mu.Unlock()
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				idx := int(next.Add(1)) - 1
				if idx >= len(servers) {
					return
				}
				sv := servers[idx]
				// Each reconcile takes a slot, exactly as the serial loop did:
				// the resync shares the pool with whatever ReconcileAsync
				// fan-out is in flight, so its connections have to be accounted
				// against the same bound (SIGMA-288).
				release, ok := r.acquireSlot(ctx)
				if !ok {
					// Either shutdown or a queue so long the pass cannot
					// finish; the rest of the fleet is left to the next tick,
					// and the pass is reported as failed so the heartbeat's
					// last-success clock goes stale (SIGMA-248).
					fail(fmt.Errorf("resync: no reconcile slot for server %s", sv.ServerID))
					return
				}
				// safeReconcile, not Reconcile: one bad row must quarantine one
				// server, not end the process for every tenant (SIGMA-250).
				err := r.safeReconcile(ctx, sv.OrgID, sv.ServerID)
				release()
				if err != nil {
					r.log.Error("resync: reconcile", "err", err, "server", sv.ServerID)
					fail(err)
				}
			}
		}()
	}
	wg.Wait()
	return passErr
}

// reportResyncPass publishes how long a fleet-resync pass took and complains
// when it outran the tick (SIGMA-320).
//
// A pass longer than the interval means every subsequent tick fires late —
// time.Ticker drops the ticks that arrive while a pass is running — so the
// fleet's drift-repair interval silently becomes the pass duration instead of
// the configured 60s. That is the moment the SLO the agent advertises stops
// being true, and it is worth a line in the log as well as a metric, because
// the metric only helps whoever already knew to look.
func (r *Reconciler) reportResyncPass(d, interval time.Duration) {
	if r.onResyncPass != nil {
		r.onResyncPass(d)
	}
	if interval > 0 && d > interval {
		r.log.Warn("resync pass outran its tick — drift repair is now slower than configured",
			"pass", d, "interval", interval)
	}
}

func (r *Reconciler) reportResync(err error) {
	if r.onResync != nil {
		r.onResync(err)
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

// notify wakes every long-poll waiter for a server: the ones parked in THIS
// process, and — through the change bus — the ones parked in any other replica
// (SIGMA-291).
//
// The local wake happens first and unconditionally. The bus is an addition, not
// a replacement: a publish failure (or no bus configured at all, which is every
// unit test) leaves single-process behaviour exactly as it was, rather than
// making the common case depend on a database round trip.
func (r *Reconciler) notify(ctx context.Context, serverID string) {
	r.WakeServer(serverID)
	if r.bus == nil {
		return
	}
	if err := r.bus.PublishDSDChange(ctx, serverID); err != nil {
		// Degrades to what every cross-replica change did before the bus
		// existed: the other replica's waiter finds the change when its
		// long-poll window expires. Worth a line, not worth failing a
		// reconcile that has already committed.
		r.log.Warn("publish dsd change to other replicas", "err", err, "server", serverID)
	}
}

// WakeServer closes the long-poll waiters registered in THIS process for a
// server. Exported so the cross-replica listener can drive it with the server
// id another replica published (SIGMA-291); a wake for a server nothing here is
// waiting on is a no-op.
func (r *Reconciler) WakeServer(serverID string) {
	r.mu.Lock()
	list := r.waiters[serverID]
	delete(r.waiters, serverID)
	r.mu.Unlock()
	for _, ch := range list {
		close(ch)
	}
}
