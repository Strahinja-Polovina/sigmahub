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
