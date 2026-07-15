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
		cur, exists, err := d.docker.ContainerInspect(ctx, name)
		if err != nil {
			d.log.Warn("reconcile: inspect", "container", name, "err", err)
			continue
		}
		if exists && cur.Running && cur.Labels[LabelSpecHash] == spec.SpecHash() {
			continue // converged
		}
		d.log.Info("reconcile: repairing drift", "container", name, "exists", exists, "running", exists && cur.Running)
		if err := d.converge(ctx, spec); err != nil {
			d.log.Warn("reconcile: converge", "container", name, "err", err)
		}
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

// GC removes managed containers that the applied DSD no longer describes,
// converging actual state to desired after a resource deletion. It is called
// from the DSD loop AFTER a document applies, so the desired set is exactly the
// container.apply ops in that (validly signed) document. The desired-state
// store is pruned to match.
func (d *Driver) GC(ctx context.Context, doc dsd.Document) {
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

	managed, err := d.docker.ContainerList(ctx)
	if err != nil {
		d.log.Warn("gc: list managed containers", "err", err)
		return
	}
	for _, c := range managed {
		if want[c.Name] {
			continue
		}
		d.log.Info("gc: removing orphaned container", "container", c.Name)
		if err := d.docker.ContainerRemove(ctx, c.ID, true); err != nil {
			d.log.Warn("gc: remove", "container", c.Name, "err", err)
		}
	}
}

// desiredNames is the set of container names the document wants running.
func desiredNames(doc dsd.Document) map[string]bool {
	names := map[string]bool{}
	for _, op := range doc.Ops {
		if op.Kind != KindContainerApply {
			continue
		}
		var spec ContainerSpec
		if err := json.Unmarshal(op.Spec, &spec); err != nil {
			continue
		}
		if spec.Name != "" {
			names[spec.Name] = true
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
