package container

// Decommission teardown (SIGMA-204): removing everything this agent put on the
// Docker daemon.
//
// This is not GC. GC compares the daemon against a desired document and reaps
// the difference, protecting live rollout generations and a peer agent's
// objects. Here there is no desired state left — the control plane has asked
// this host to stop being part of the fleet — so the whole managed set goes,
// including generations a rollout still owns.
//
// The peer-ownership rule is kept, because the reason it exists is unchanged:
// the fleet e2e runs several agents against one daemon, and an uninstall that
// swept objects by the managed label alone would take a peer's workloads down
// with it.

import (
	"context"
	"errors"
	"fmt"
)

// RemoveManagedContainers stops and removes every container this agent owns,
// and empties the desired-state store FIRST.
//
// Order inside this function matters as much as the order of the steps around
// it: the driver's reconcile loop re-creates any container still recorded as
// desired, every 30 seconds, from local state and with no control-plane
// round-trip. Removing the containers without clearing that store would have
// the agent racing itself — reaping a workload and then dutifully restoring it
// — right up until the moment it deletes its own data directory.
//
// Errors are collected rather than returned on the first failure: a daemon that
// refuses one container must not leave the other nine running, and the caller
// reports the aggregate to the control plane.
func (d *Driver) RemoveManagedContainers(ctx context.Context) error {
	// Same lock GC takes, for the same reason: no concurrent reconcile may
	// recreate a container between the store prune and the removal.
	d.mu.Lock()
	defer d.mu.Unlock()

	stored, err := d.store.AllDesired()
	if err != nil {
		return fmt.Errorf("read desired containers: %w", err)
	}
	var errs []error
	for name := range stored {
		if err := d.store.DeleteDesired(name); err != nil {
			errs = append(errs, fmt.Errorf("forget desired container %s: %w", name, err))
		}
	}

	managed, err := d.docker.ContainerList(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list managed containers: %w", err))...)
	}
	for _, c := range managed {
		if ownedByAnotherServer(c.Labels, d.serverID) {
			continue
		}
		d.log.Info("uninstall: removing container", "container", c.Name)
		// force=true removes a running container in one call; the daemon sends
		// SIGKILL after its own grace period. A separate stop-then-remove would
		// double the number of ways this can half-fail on a host we are about to
		// stop managing.
		if err := d.docker.ContainerRemove(ctx, c.ID, true); err != nil {
			errs = append(errs, fmt.Errorf("remove container %s: %w", c.Name, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveManagedNetworks removes every sigmahub-managed Docker network. Must run
// AFTER the containers: Docker refuses to remove a network that still has an
// endpoint attached.
func (d *Driver) RemoveManagedNetworks(ctx context.Context) error {
	nets, err := d.docker.ManagedNetworks(ctx)
	if err != nil {
		return fmt.Errorf("list managed networks: %w", err)
	}
	var errs []error
	for _, name := range nets {
		d.log.Info("uninstall: removing network", "network", name)
		if err := d.docker.NetworkRemove(ctx, name); err != nil {
			errs = append(errs, fmt.Errorf("remove network %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// RemoveManagedVolumes destroys every sigmahub-managed named volume. This is
// the ONLY step gated on an explicit operator opt-in (SIGMA-205): a volume is a
// database's data directory or a user's uploads, and disconnecting the machine
// it happens to sit on is not consent to delete them. Must run after the
// containers, which hold them.
func (d *Driver) RemoveManagedVolumes(ctx context.Context) error {
	vols, err := d.docker.ManagedVolumes(ctx)
	if err != nil {
		return fmt.Errorf("list managed volumes: %w", err)
	}
	var errs []error
	for _, name := range vols {
		d.log.Info("uninstall: removing volume", "volume", name)
		if err := d.docker.VolumeRemove(ctx, name, true); err != nil {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
