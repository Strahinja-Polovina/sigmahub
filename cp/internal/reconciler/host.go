package reconciler

import (
	"encoding/json"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// renderHostOps renders the opt-out hardening pass as typed host.* ops — the
// only channel for post-enrollment host changes. Ordering is fixed (nftables →
// sshd → cis) so the rendered document hash is deterministic. The op specs mirror
// the agent's host package JSON shapes exactly.
//
// publicURL is this control plane's own base URL (CP_PUBLIC_URL), used to point
// agent.update at the /dl proxy; empty when the deployment has not set one.
func renderHostOps(serverID string, hh store.HostHardening, publicURL string) []dsd.Op {
	var ops []dsd.Op

	// No "wireguardPort" (SIGMA-275). The port the mesh listens on is the
	// agent's own constant (agent/internal/mesh.ListenPort), and cp cannot
	// import it — separate Go modules. A literal here was therefore a copy that
	// nothing held to the original: move the mesh off the WireGuard default and
	// every agent would listen on the new port while this control plane kept
	// rendering a ruleset admitting only the old one, dropping every handshake
	// fleet-wide while each agent reported a healthy config.
	//
	// So the control plane says nothing and the agent's constant decides. The
	// spec field survives as an explicit override for the day a deployment needs
	// one; omitting it is what makes "the port we open" and "the port we listen
	// on" the same value by construction instead of by two numbers agreeing.
	nft, _ := json.Marshal(map[string]any{
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
		spec := map[string]string{"version": hh.DesiredAgentVersion}
		// SIGMA-262: tell the agent to download the release through THIS control
		// plane's /dl proxy rather than github.com.
		//
		// SIGMA-217 moved every onboarding fetch behind that proxy because on a
		// PRIVATE release repository the unauthenticated github.com URLs 404 and
		// not one server can be onboarded. Self-update was left on github.com, so
		// the same fleet onboarded fine and then could not be UPGRADED: every
		// agent.update op died with a 404 and the only way to ship a CVE fix was
		// SSH to every host — the exact thing the no-SSH upgrade button removes.
		//
		// The agent cannot derive this URL. It knows the control plane it polls,
		// but the DSD says nothing about where that control plane lives, so the
		// base has to travel in the op. It is a hint, not a requirement: an
		// agent that receives no base falls back to the public release repo, so a
		// control plane with no CP_PUBLIC_URL keeps working exactly as before
		// (and a public release repo never needed the proxy in the first place).
		//
		// Trust is unaffected — the agent cosign-verifies checksums.txt against
		// the release workflow's OIDC identity and the archive against that
		// checksums.txt, whoever served the bytes. See cp/internal/api/installer.go.
		if base := strings.TrimRight(publicURL, "/"); base != "" {
			spec["downloadBase"] = base + "/dl/" + hh.DesiredAgentVersion
		}
		up, _ := json.Marshal(spec)
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
