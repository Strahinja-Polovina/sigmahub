// Package uninstall applies the "agent.uninstall" DSD op: a graceful
// decommission (SIGMA-204). The agent removes the workloads it is running and
// then removes ITSELF — the WireGuard interface and config, the systemd unit,
// the data directory and the binary — and tells the control plane it is done.
//
// It is modelled on the agent.update precedent (internal/selfupdate), the other
// op that manipulates the agent's own binary and unit, and registered behind the
// same apply registry: removing a host is a TYPED op, not a shell script the
// control plane pushes.
//
// # The ordering, and why it is not the obvious one
//
// The obvious reading of "remove everything, then report" produces a bug that
// is invisible in a unit test of each step: the ack has to travel over the
// network, authenticated by a credential that lives in the data directory, and
// two of the teardown steps destroy exactly what it needs.
//
//   - The WireGuard interface is the network path on any fleet whose control
//     plane sits inside the mesh. `wg-quick down` removes the routes with it.
//   - The data directory holds state.json — the agent token. Once it is gone,
//     an agent that restarts has no identity and can never ack; the control
//     plane waits out the full timeout for a machine that finished minutes ago,
//     and completes the decommission as a FAILURE it did not have.
//
// So the ack is sent as soon as the part the control plane actually cares about
// — the workloads — is done, and before anything that could break the channel.
// The steps that follow only remove the agent, and nobody but the agent needs
// to hear about them.
//
// This is also why the ack is a dedicated call rather than the op's ordinary
// status report: op status is posted by the DSD loop AFTER the handler returns,
// which is far too late.
//
// # Failure
//
// Every step runs even when an earlier one failed, and the ack is sent either
// way, carrying the detail. A host whose Docker daemon is down cannot remove
// its containers, and stopping there would leave the agent in place with a
// credential the control plane is about to revoke — which is precisely today's
// defect. Reporting honestly and continuing gives the operator a completed
// disconnect, an audit entry saying what did not work, and the manual cleanup
// script (agent/packaging/uninstall.sh) to finish by hand.
package uninstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// Kind is the DSD op kind this package handles.
const Kind = "agent.uninstall"

// Spec is the op payload the control plane renders (cp/internal/reconciler).
type Spec struct {
	// ServerID is the server this op decommissions. The DSD is already bound to
	// one server, so this is a redundancy on purpose: it is the one op that ends
	// a host, and it refuses to run against an identity that is not ours.
	ServerID string `json:"serverId"`
	// PurgeVolumes destroys named volumes too — off unless the operator ticked
	// the box, because that is the customer's data.
	PurgeVolumes bool `json:"purgeVolumes"`
	// MeshInterface is the WireGuard link name, rendered by the CP (which is the
	// authority for the names of everything it asked the agent to create).
	MeshInterface string `json:"meshInterface"`
}

// Steps are the teardown actions, injected so the handler's ORDER can be tested
// without a Docker daemon, a systemd, or a host to destroy. Production wiring
// lives in cmd/sigmad; every field is required except as noted.
type Steps struct {
	// RemoveK3s uninstalls k3s when this host was a cluster node. Runs before
	// the ack because the ack claims phase 1 is done, and a host that kept
	// running k3s, /var/lib/rancher/k3s and every workload the scheduler had
	// placed on it would be one the dashboard called clean while the cluster it
	// belonged to was still using it. A host that never joined a cluster has no
	// k3s uninstall script and this is a no-op.
	RemoveK3s func(ctx context.Context) error
	// RemoveContainers stops and removes the managed containers and empties the
	// agent's desired-state store, so its reconcile loop cannot restore them.
	RemoveContainers func(ctx context.Context) error
	// RemoveNetworks removes the managed Docker networks. After the containers:
	// Docker refuses to remove a network with an endpoint attached.
	RemoveNetworks func(ctx context.Context) error
	// RemoveVolumes destroys managed named volumes. Called ONLY when the op spec
	// opts in.
	RemoveVolumes func(ctx context.Context) error
	// Ack reports the outcome to the control plane, which then writes the
	// tombstone and revokes this agent's token. Everything after it is
	// unreportable, so nothing that the ack depends on may run before it.
	Ack func(ctx context.Context, ok bool, detail string) error
	// TearDownMesh brings the WireGuard interface down and removes its config
	// and key.
	TearDownMesh func(ctx context.Context, iface string) error
	// RemoveUnit disables and deletes the systemd units.
	RemoveUnit func(ctx context.Context) error
	// RemoveDataDir deletes the agent's data directory — identity, journal,
	// desired-state store and all.
	RemoveDataDir func(ctx context.Context) error
	// RemoveBinary deletes the sigmad executable.
	RemoveBinary func(ctx context.Context) error
	// Exit ends the process. Called last, from inside the handler rather than
	// from the DSD loop: by this point the data dir holding the journal is gone,
	// so there is nothing left that could reliably carry a "please exit" flag
	// back out to the caller.
	Exit func()
}

// Uninstaller executes agent.uninstall ops.
type Uninstaller struct {
	Log *slog.Logger
	// ServerID is this agent's own identity, checked against the spec.
	ServerID string
	Steps    Steps
}

func (u *Uninstaller) Register(r *apply.Registry) { r.Register(Kind, u.Handle) }

// Handle runs the teardown. It returns an error describing what failed, for the
// journal and for the op status report — neither of which the control plane is
// likely to ever see, which is exactly why the ack exists.
func (u *Uninstaller) Handle(ctx context.Context, op dsd.Op) error {
	var spec Spec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("agent.uninstall: bad spec: %w", err)
	}
	// Refuse an op addressed to another server. The DSD loop already checks the
	// document's server id; this is the same check one layer in, because the
	// cost of being wrong here is a wiped host.
	if spec.ServerID != "" && u.ServerID != "" && spec.ServerID != u.ServerID {
		return fmt.Errorf("agent.uninstall: op addressed to %q, this agent is %q", spec.ServerID, u.ServerID)
	}
	iface := spec.MeshInterface

	u.Log.Info("agent.uninstall: decommissioning this host",
		"purgeVolumes", spec.PurgeVolumes, "meshInterface", iface)

	// ── Phase 1: the workloads. This is the part the control plane is waiting
	// to hear about, and the part that must finish before the ack.
	var errs []error
	run := func(name string, fn func(context.Context) error) {
		if fn == nil {
			return
		}
		if err := fn(ctx); err != nil {
			u.Log.Error("agent.uninstall: step failed", "step", name, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	// k3s first: its workloads are containerd's, not Docker's, so the Docker
	// sweep below cannot see them, and stopping k3s releases the mounts and
	// network namespaces they hold.
	run("k3s", u.Steps.RemoveK3s)
	run("containers", u.Steps.RemoveContainers)
	run("networks", u.Steps.RemoveNetworks)
	if spec.PurgeVolumes {
		// Opt-in only. The absence of this call on the default path is the
		// product promise: a disconnected machine's database volume is still
		// there tomorrow.
		run("volumes", u.Steps.RemoveVolumes)
	}

	// ── The ack. Sent here — with the WireGuard tunnel still up, the data
	// directory still holding the token, and the binary still on disk — because
	// every one of those is required to send it, and every one of them is about
	// to be destroyed. `ok` reflects phase 1 only: that is the whole of what the
	// control plane can act on.
	if u.Steps.Ack != nil {
		if err := u.Steps.Ack(ctx, len(errs) == 0, joinDetail(errs)); err != nil {
			// The control plane did not hear us. Carry on regardless: the host
			// teardown is already half-done and cannot be undone, and the CP's
			// decommission timeout is the designed catch for exactly this. Going
			// back to a normal life here would leave a machine running an agent
			// whose token is minutes from revocation.
			u.Log.Error("agent.uninstall: ack failed; the control plane will complete this on its timeout", "err", err)
			errs = append(errs, fmt.Errorf("ack: %w", err))
		}
	}

	// ── Phase 2: the agent removes itself. Nothing below this line is
	// reportable, so ordering is by dependency only: the mesh first (it needs
	// its config file), then the unit, then the data dir, then the binary.
	if u.Steps.TearDownMesh != nil {
		if err := u.Steps.TearDownMesh(ctx, iface); err != nil {
			u.Log.Error("agent.uninstall: step failed", "step", "mesh", "err", err)
			errs = append(errs, fmt.Errorf("mesh: %w", err))
		}
	}
	run("unit", u.Steps.RemoveUnit)
	run("dataDir", u.Steps.RemoveDataDir)
	run("binary", u.Steps.RemoveBinary)

	u.Log.Info("agent.uninstall: teardown complete; exiting", "errors", len(errs))
	if u.Steps.Exit != nil {
		u.Steps.Exit()
	}
	return errors.Join(errs...)
}

// joinDetail renders the collected failures as the one sentence the control
// plane records in its audit entry. Capped, because it is written to a log an
// operator reads, not to a debugger.
func joinDetail(errs []error) string {
	if len(errs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		parts = append(parts, err.Error())
	}
	detail := strings.Join(parts, "; ")
	const max = 500
	if len(detail) > max {
		detail = detail[:max] + "…"
	}
	return detail
}
