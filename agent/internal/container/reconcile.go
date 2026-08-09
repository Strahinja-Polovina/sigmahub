package container

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// Reconcile converges every desired container to running. It reads only the
// local desired-state store, so it repairs drift (an out-of-band `docker stop`,
// a crash) even while the control plane is unreachable — satisfying the
// "workloads keep running if the CP is unreachable" invariant. Errors on one
// container never abort the others.
func (d *Driver) Reconcile(ctx context.Context) {
	desired, err := d.store.AllDesired()
	if err != nil {
		d.log.Warn("reconcile: read desired state", "err", err)
		return
	}
	for name, spec := range desired {
		if ctx.Err() != nil {
			return
		}
		d.reconcileOne(ctx, name, spec)
	}
}

// reconcileOne converges a single container under the driver lock, re-checking
// that it is still desired first — so a container GC removed (and pruned from
// the desired store) between this loop's snapshot and now is not recreated.
func (d *Driver) reconcileOne(ctx context.Context, name string, spec ContainerSpec) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if still, err := d.store.HasDesired(name); err != nil || !still {
		return // GC pruned it (or read error) — do not resurrect
	}
	cur, exists, err := d.docker.ContainerInspect(ctx, name)
	if err != nil {
		d.log.Warn("reconcile: inspect", "container", name, "err", err)
		return
	}
	if converged(spec, cur, exists) {
		return
	}
	d.log.Info("reconcile: repairing drift", "container", name, "exists", exists, "running", exists && cur.Running)
	if err := d.converge(ctx, spec); err != nil {
		d.log.Warn("reconcile: converge", "container", name, "err", err)
	}
}

// RunReconcile drives Reconcile on a ticker until ctx is cancelled. The
// interval must be well under the 60s drift-repair SLO. Best-effort: it never
// exits on error, mirroring the mesh loop.
func (d *Driver) RunReconcile(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	d.Reconcile(ctx) // converge immediately on startup (e.g. after a restart)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Reconcile(ctx)
		}
	}
}

// GC removes managed containers that the DSD no longer describes, converging
// actual state to desired after a resource deletion. It is called from the DSD
// loop BEFORE the document's ops apply (SIGMA-113), so a bare volume.remove for
// a deleted resource is not blocked by the still-running container that held the
// volume; the desired set is exactly the container.apply ops in that (validly
// signed) document, independent of apply. The desired-state store is pruned to
// match. Live rollout/recreate generations — and any group the document
// explicitly retains — are never reaped (see protectedGroups), so running
// before apply cannot cut a blue-green swap or a deploy the CP is holding back.
func (d *Driver) GC(ctx context.Context, doc dsd.Document) {
	// Held for the whole prune+remove so a concurrent reconcile cannot recreate
	// a container between the desired-store prune and the container removal.
	d.mu.Lock()
	defer d.mu.Unlock()
	want := desiredNames(doc)

	// Prune the local desired store first so a restart mid-GC does not resurrect
	// a removed container.
	stored, err := d.store.AllDesired()
	if err == nil {
		for name := range stored {
			if !want[name] {
				if err := d.store.DeleteDesired(name); err != nil {
					d.log.Warn("gc: prune desired", "container", name, "err", err)
				}
			}
		}
	}

	protected := protectedGroups(doc)

	managed, err := d.docker.ContainerList(ctx)
	if err != nil {
		d.log.Warn("gc: list managed containers", "err", err)
		return
	}
	for _, c := range managed {
		if !gcReap(c, want, protected, d.serverID) {
			continue
		}
		d.log.Info("gc: removing orphaned container", "container", c.Name)
		if err := d.docker.ContainerRemove(ctx, c.ID, true); err != nil {
			d.log.Warn("gc: remove", "container", c.Name, "err", err)
		}
	}
}

// gcReap is GC's whole decision for one managed container, split out so the
// keep rules are testable without a Docker daemon. A container is removed only
// when the document neither names it nor protects its (resource, service)
// group, and it is not a peer agent's object.
func gcReap(c ContainerState, want, protected map[string]bool, serverID string) bool {
	if want[c.Name] {
		return false
	}
	// Never reap a peer's object. On a real host this is always false — one
	// agent owns the daemon — but the fleet e2e runs several agents against ONE
	// daemon, and without this each one deleted its peers' containers as orphans
	// it had no ops for: the placed service came up, the other host's next
	// reconcile removed it, and a multi-server deploy could never converge. An
	// object with no owner label is still reaped: on a real host it can only be
	// this agent's own, from an older build.
	if ownedByAnotherServer(c.Labels, serverID) {
		return false
	}
	if rid := c.Labels[LabelResourceID]; rid != "" && protected[rolloutGroupKey(rid, c.Labels[LabelService])] {
		return false // a live rollout-owned or explicitly retained group
	}
	return true
}

// protectedGroups is the set of (resource, service) groups GC must leave alone
// entirely, from both of the ways a document can claim one.
//
// The first is a deploy.rollout/deploy.recreate op: such a group owns its own
// container lifecycle — the swap creates the new generation and drains the old
// ONLY after the new one is healthy, and a health-gate failure deliberately
// keeps the old generation serving. A blind reap here (the old generation is
// never in `want`) would defeat the never-cut invariant and take a live app
// down.
//
// The second is an explicit `retain` list on a resource.sync stub. The control
// plane deliberately renders NO rollout op for a resource whose deploy it is
// holding back — a dedicated build server still building, a Compose service
// gated on a remote dependency — and those resources fall through to the stub.
// Before SIGMA-230 the stub named nothing, so GC (which runs BEFORE the ops)
// saw the live generation-suffixed container as an orphan and removed it: the
// app was down for the entire build rather than the swap window, and for a
// gated dependency that never succeeds, indefinitely.
//
// Scoping to the (resource, service) PAIR (not the whole resource) is what
// keeps a Compose service REMOVED from the compose file — absent from both the
// ops and the retain list — correctly garbage-collected.
func protectedGroups(doc dsd.Document) map[string]bool {
	out := rolloutManagedGroups(doc)
	for _, op := range doc.Ops {
		if op.Kind != KindResourceSync {
			continue
		}
		var stub resourceSyncSpec
		if err := json.Unmarshal(op.Spec, &stub); err != nil || stub.ResourceID == "" {
			continue
		}
		for _, svc := range stub.Retain {
			out[rolloutGroupKey(stub.ResourceID, svc)] = true
		}
	}
	return out
}

// ownedByAnotherServer reports whether a managed object belongs to a DIFFERENT
// agent than this one.
//
// On a real host it is always false — one agent owns the daemon, which is why
// nothing stamped an owner and GC felt free to reap anything wearing the
// managed label. The fleet e2e runs several agents against ONE daemon, and
// there each agent saw its peers' containers as orphans it had no ops for and
// removed them: a placed Compose service came up on its own host, the other
// host's next reconcile deleted it, and a multi-server deploy could never
// converge — with nothing in the log of the host that was trying to deploy.
//
// An object with NO owner label is ours: on a real host it can only be this
// agent's own from an older build, and refusing to reap it would leak every
// pre-upgrade container forever.
func ownedByAnotherServer(labels map[string]string, serverID string) bool {
	owner := labels[LabelServerID]
	return owner != "" && serverID != "" && owner != serverID
}

// rolloutGroupKey identifies a (resource, service) rollout group; service is ""
// for a single-container app.
func rolloutGroupKey(resourceID, service string) string {
	return resourceID + "\x00" + service
}

// rolloutManagedGroups is the set of (resource, service) groups the document
// deploys via a deploy.rollout/deploy.recreate op — the groups GC must leave
// entirely to the swap ops.
func rolloutManagedGroups(doc dsd.Document) map[string]bool {
	out := map[string]bool{}
	for _, op := range doc.Ops {
		var container ContainerSpec
		switch op.Kind {
		case KindDeployRollout:
			var rs RolloutSpec
			if err := json.Unmarshal(op.Spec, &rs); err != nil {
				continue
			}
			container = rs.Container
		case KindDeployRecreate:
			var rs RecreateSpec
			if err := json.Unmarshal(op.Spec, &rs); err != nil {
				continue
			}
			container = rs.Container
		default:
			continue
		}
		if container.ResourceID != "" {
			out[rolloutGroupKey(container.ResourceID, container.Service)] = true
		}
	}
	return out
}

// desiredNames is the set of container names the document wants running — the
// GC keep-set. It covers both container.apply workloads AND the Traefik proxy
// (a proxy.traefik op), which carries the managed label but is not a
// container.apply, so it would otherwise be force-removed by GC on every apply.
// Keying the proxy off the op's presence also gives correct teardown: when the
// proxy role is cleared the op disappears from the document and GC removes it.
func desiredNames(doc dsd.Document) map[string]bool {
	names := map[string]bool{}
	for _, op := range doc.Ops {
		switch op.Kind {
		case KindProxyTraefik:
			names[traefikContainerName] = true
		case KindContainerApply:
			var spec ContainerSpec
			if err := json.Unmarshal(op.Spec, &spec); err != nil {
				continue
			}
			if spec.Name != "" {
				names[spec.Name] = true
			}
		case KindDeployRollout:
			// A rollout's live container carries a generation-suffixed name; keep
			// it so GC doesn't reap the freshly-deployed version.
			var rs RolloutSpec
			if err := json.Unmarshal(op.Spec, &rs); err != nil {
				continue
			}
			if rs.Container.Name != "" {
				names[rolloutName(rs.Container.Name, rs.Generation)] = true
			}
		case KindDeployRecreate:
			var rs RecreateSpec
			if err := json.Unmarshal(op.Spec, &rs); err != nil {
				continue
			}
			if rs.Container.Name != "" {
				names[rolloutName(rs.Container.Name, rs.Generation)] = true
			}
		}
	}
	return names
}

// Probe reports whether Docker is reachable and its version, for host facts. It
// uses a short timeout so a dead daemon never stalls a heartbeat.
func Probe(ctx context.Context, docker *DockerClient) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ver, err := docker.Ping(ctx)
	if err != nil {
		return false, ""
	}
	return true, ver
}
