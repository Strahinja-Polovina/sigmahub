package reconciler

import (
	"encoding/json"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// renderHostOps renders the opt-out hardening pass as typed host.* ops — the
// only channel for post-enrollment host changes. Ordering is fixed (nftables →
// sshd → cis) so the rendered document hash is deterministic. The op specs mirror
// the agent's host package JSON shapes exactly.
func renderHostOps(serverID string, hh store.HostHardening) []dsd.Op {
	var ops []dsd.Op

	nft, _ := json.Marshal(map[string]any{
		"wireguardPort":  51820,
		"meshInterface":  hh.MeshInterface,
		"allowPublicSSH": hh.KeepPublicSSH,
		"proxyRole":      hh.ProxyRole,
		"extraPorts":     extraPortsJSON(hh.ExtraPorts),
	})
	ops = append(ops, dsd.Op{ID: "host:nftables:" + serverID, Kind: dsd.KindHostNftables, Spec: nft})

	sshd, _ := json.Marshal(map[string]any{
		"meshIp": hh.MeshIP,
		// Bind sshd to the mesh only unless the operator opted to keep public SSH.
		"listenMeshOnly": !hh.KeepPublicSSH,
	})
	ops = append(ops, dsd.Op{ID: "host:sshd:" + serverID, Kind: dsd.KindHostSSHD, Spec: sshd})

	if hh.CISEnabled {
		cis, _ := json.Marshal(map[string]any{"level": 1})
		ops = append(ops, dsd.Op{ID: "host:cis:" + serverID, Kind: dsd.KindHostCIS, Spec: cis})
	}

	// Dashboard-driven agent upgrade: rendered while the desired version
	// differs from what the agent last reported; the agent's post-update
	// heartbeat carries the new version and the op drops out of the document.
	// The op id embeds the target version so a later retarget is a new op.
	if hh.DesiredAgentVersion != "" && hh.DesiredAgentVersion != hh.AgentVersion {
		up, _ := json.Marshal(map[string]string{"version": hh.DesiredAgentVersion})
		ops = append(ops, dsd.Op{
			ID:   "agent:update:" + serverID + ":" + hh.DesiredAgentVersion,
			Kind: dsd.KindAgentUpdate,
			Spec: up,
		})
	}
	return ops
}

// UninstallOpID is the op id of a server's agent.uninstall op. Deterministic
// and server-scoped: a resync re-renders it byte-identically (so the document
// hash does not churn and the agent's journal treats a re-delivered version as
// already applied), and the CP's ack handler can recognise it by prefix.
func UninstallOpID(serverID string) string { return "agent:uninstall:" + serverID }

// renderUninstallOps is the WHOLE document for a server being decommissioned
// (SIGMA-204).
//
// Not "the uninstall op appended to the usual ops": a document that still
// described containers, networks and a hardened firewall while asking the agent
// to delete all of it is incoherent, and the agent applies ops in dependency
// order with no notion that one of them ends the machine's participation. It
// would race its own reconcile loop — the 30s converge pass re-creating a
// container the uninstall handler had just removed — and the host.* ops would
// re-arm a firewall on a box that is about to have no agent. One op in, one op
// out.
func renderUninstallOps(serverID string, hh store.HostHardening) []dsd.Op {
	spec, _ := json.Marshal(map[string]any{
		"serverId": serverID,
		// The operator's explicit opt-in. Named volumes are the customer's data
		// (a Postgres data directory, uploaded files); "give me the machine
		// back" is not consent to destroy them, so this is false unless someone
		// ticked the box.
		"purgeVolumes": hh.PurgeVolumes,
		// The control plane is the authority for the names of things it told the
		// agent to create, mesh interface included (host.nftables already carries
		// it), so the teardown tears down the same interface the setup brought up.
		"meshInterface": hh.MeshInterface,
	})
	return []dsd.Op{{ID: UninstallOpID(serverID), Kind: dsd.KindAgentUninstall, Spec: spec}}
}

// extraPortsJSON normalizes to a non-nil slice of {port,proto} so the marshaled
// spec is stable ([] not null) and matches the agent's PortException shape.
func extraPortsJSON(ports []store.PortException) []map[string]any {
	out := make([]map[string]any, 0, len(ports))
	for _, p := range ports {
		proto := p.Proto
		if proto != "udp" {
			proto = "tcp"
		}
		out = append(out, map[string]any{"port": p.Port, "proto": proto})
	}
	return out
}
