package reconciler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestRenderHostOpsDefault(t *testing.T) {
	ops := renderHostOps("srv_1", store.HostHardening{
		MeshIP: "10.77.0.5", MeshInterface: "sigma0",
		ProxyRole: true, KeepPublicSSH: false, CISEnabled: true,
		ExtraPorts: []store.PortException{{Port: 9000, Proto: "tcp"}},
	}, "")
	byID := map[string]dsd.Op{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	nft, ok := byID["host:nftables:srv_1"]
	if !ok || nft.Kind != dsd.KindHostNftables {
		t.Fatal("missing host.nftables op")
	}
	var nftSpec struct {
		AllowPublicSSH bool   `json:"allowPublicSSH"`
		ProxyRole      bool   `json:"proxyRole"`
		MeshInterface  string `json:"meshInterface"`
		ExtraPorts     []struct {
			Port  int    `json:"port"`
			Proto string `json:"proto"`
		} `json:"extraPorts"`
	}
	_ = json.Unmarshal(nft.Spec, &nftSpec)
	if nftSpec.AllowPublicSSH || !nftSpec.ProxyRole || nftSpec.MeshInterface != "sigma0" {
		t.Errorf("nftables spec = %+v", nftSpec)
	}
	if len(nftSpec.ExtraPorts) != 1 || nftSpec.ExtraPorts[0].Port != 9000 {
		t.Errorf("extra ports not rendered: %+v", nftSpec.ExtraPorts)
	}

	sshd, ok := byID["host:sshd:srv_1"]
	if !ok {
		t.Fatal("missing host.sshd op")
	}
	var sshdSpec struct {
		MeshIP         string `json:"meshIp"`
		ListenMeshOnly bool   `json:"listenMeshOnly"`
	}
	_ = json.Unmarshal(sshd.Spec, &sshdSpec)
	if sshdSpec.MeshIP != "10.77.0.5" || !sshdSpec.ListenMeshOnly {
		t.Errorf("sshd spec = %+v (expect mesh-only lockdown by default)", sshdSpec)
	}
	if _, ok := byID["host:cis:srv_1"]; !ok {
		t.Error("missing host.cis op when CIS enabled")
	}
}

// TestRenderHostOpsDoesNotRestateTheWireGuardPort is SIGMA-275.
//
// The WireGuard port was written three times with nothing linking the copies:
// agent/internal/mesh.ListenPort (what the agent LISTENS on and advertises as
// its endpoint), a bare 51820 literal here, and a second literal default in the
// agent's nftables renderer. cp and agent are separate Go modules, so the
// control plane cannot import the constant — but unlike SUPPORTED_ARCHES,
// nothing else held them together either.
//
// Moving the mesh off the WireGuard default would then have every agent listen
// on the new port while this control plane kept rendering a firewall admitting
// only 51820: the mesh stops forming fleet-wide on the next reconcile, the agent
// reports a healthy config, and the firewall silently drops the handshakes.
//
// The fix is not a fourth copy to keep in step. The port belongs to the host
// that opens the socket, so the control plane says nothing about it and the
// agent's own constant is the single source of truth. The spec field survives
// as an explicit override; this test is what stops a literal creeping back in.
func TestRenderHostOpsDoesNotRestateTheWireGuardPort(t *testing.T) {
	ops := renderHostOps("srv_wg", store.HostHardening{MeshInterface: "sigma0"}, "")
	var nft dsd.Op
	for _, op := range ops {
		if op.Kind == dsd.KindHostNftables {
			nft = op
		}
	}
	if nft.ID == "" {
		t.Fatal("missing host.nftables op")
	}
	var spec map[string]any
	if err := json.Unmarshal(nft.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if v, ok := spec["wireguardPort"]; ok {
		t.Errorf("host.nftables restates the WireGuard port as %v; the agent's mesh.ListenPort "+
			"is the single source of truth for it and the control plane cannot import that constant, "+
			"so a literal here is a copy that drifts silently", v)
	}
}

func TestRenderHostOpsKeepPublicSSH(t *testing.T) {
	ops := renderHostOps("srv_2", store.HostHardening{
		MeshIP: "10.77.0.6", MeshInterface: "sigma0", KeepPublicSSH: true, CISEnabled: false,
	}, "")
	byID := map[string]dsd.Op{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	var nftSpec struct {
		AllowPublicSSH bool `json:"allowPublicSSH"`
	}
	_ = json.Unmarshal(byID["host:nftables:srv_2"].Spec, &nftSpec)
	if !nftSpec.AllowPublicSSH {
		t.Error("keep-public-SSH opt-out must set allowPublicSSH")
	}
	var sshdSpec struct {
		ListenMeshOnly bool `json:"listenMeshOnly"`
	}
	_ = json.Unmarshal(byID["host:sshd:srv_2"].Spec, &sshdSpec)
	if sshdSpec.ListenMeshOnly {
		t.Error("keep-public-SSH must not bind sshd mesh-only")
	}
	if _, ok := byID["host:cis:srv_2"]; ok {
		t.Error("host.cis must not render when CIS disabled")
	}
}

// TestRenderHostOpsAgentUpdateCarriesDownloadBase is SIGMA-262.
//
// The control plane's own contract is that the agent upgrades through THIS
// control plane's /dl proxy — the route that exists so a private release
// repository is onboardable at all (SIGMA-217). The agent cannot know that URL;
// it only knows the control plane it polls. So the op has to carry it, and this
// test is what stops the op from going back to being a bare version string that
// leaves every agent reaching for github.com.
func TestRenderHostOpsAgentUpdateCarriesDownloadBase(t *testing.T) {
	ops := renderHostOps("srv_3", store.HostHardening{
		MeshInterface: "sigma0", AgentVersion: "v0.1.0", DesiredAgentVersion: "v0.2.0",
	}, "https://cp.example.test")

	var up dsd.Op
	for _, op := range ops {
		if op.Kind == dsd.KindAgentUpdate {
			up = op
		}
	}
	if up.ID == "" {
		t.Fatal("missing agent.update op")
	}
	var spec struct {
		Version      string `json:"version"`
		DownloadBase string `json:"downloadBase"`
	}
	if err := json.Unmarshal(up.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Version != "v0.2.0" {
		t.Errorf("version = %q, want v0.2.0", spec.Version)
	}
	if spec.DownloadBase != "https://cp.example.test/dl/v0.2.0" {
		t.Errorf("downloadBase = %q, want the control plane's own /dl route for the target version", spec.DownloadBase)
	}
}

// A control plane with no CP_PUBLIC_URL cannot name itself, so it must say
// nothing rather than render a base the agent would have to guess about. The
// agent then falls back to the public release repo — the pre-SIGMA-262
// behaviour, which is still correct for a public release repository.
func TestRenderHostOpsAgentUpdateOmitsDownloadBaseWithoutPublicURL(t *testing.T) {
	ops := renderHostOps("srv_4", store.HostHardening{
		MeshInterface: "sigma0", DesiredAgentVersion: "v0.2.0",
	}, "")
	found := false
	for _, op := range ops {
		if op.Kind != dsd.KindAgentUpdate {
			continue
		}
		found = true
		if strings.Contains(string(op.Spec), "downloadBase") {
			t.Errorf("agent.update spec must omit downloadBase when the CP has no public URL: %s", op.Spec)
		}
	}
	if !found {
		t.Fatal("missing agent.update op")
	}
}

// The agent reports its version WITHOUT the leading `v`, and the control plane
// stores the desired one WITH it (SIGMA-365).
//
// `.goreleaser.yaml` stamps `-X main.version={{ .Version }}`, and goreleaser's
// `.Version` is the tag minus the `v` — which is why install.sh computes
// `ver_noV="${SIGMAHUB_VERSION#v}"` and why installer.go and the agent's
// selfupdate both TrimPrefix before building a URL. Meanwhile the API validates
// the desired version against `^v[0-9]+\.[0-9]+\.[0-9]+`, so `v0.4.0` is the
// only spelling that can be stored.
//
// Compared raw, those two are never equal, and the consequences compound: the
// upgrade op is re-rendered into EVERY later document, and the agent's own
// idempotency guard was the same raw compare, so each one re-ran a ~30 MB
// download, a cosign verification, a binary rewrite and an os.Exit(0) restart of
// the root daemon on a customer's host — on every deploy, secret rotation or
// domain attach, with no way to clear it (an empty desired version means "use
// the CP's own release" and a non-`v` value is refused at the API).
//
// The existing vocabulary test compares the two REGEXPS out of source and cannot
// see this; only a comparison of the actual values can.
func TestAgentUpdateDropsOutWhenTheAgentIsAlreadyOnTheTargetVersion(t *testing.T) {
	for _, tc := range []struct{ desired, reported string }{
		{"v0.4.0", "0.4.0"}, // the real pairing: CP stores v-prefixed, agent stamps bare
		{"v0.4.0", "v0.4.0"},
		{"0.4.0", "0.4.0"},
	} {
		ops := renderHostOps("srv_x", store.HostHardening{
			MeshInterface: "sigma0", AgentVersion: tc.reported, DesiredAgentVersion: tc.desired,
		}, "https://cp.example.test")
		for _, op := range ops {
			if op.Kind == dsd.KindAgentUpdate {
				t.Errorf("desired %q vs reported %q rendered an upgrade op — the agent is already there, "+
					"so this op re-downloads and restarts sigmad on every future document",
					tc.desired, tc.reported)
			}
		}
	}
}

// ...and it must still render when the versions genuinely differ, whichever way
// each side happens to spell them.
func TestAgentUpdateStillRendersOnARealVersionChange(t *testing.T) {
	for _, tc := range []struct{ desired, reported string }{
		{"v0.5.0", "0.4.0"},
		{"v0.5.0", "v0.4.0"},
		{"v0.5.0", ""}, // never checked in
	} {
		ops := renderHostOps("srv_y", store.HostHardening{
			MeshInterface: "sigma0", AgentVersion: tc.reported, DesiredAgentVersion: tc.desired,
		}, "https://cp.example.test")
		found := false
		for _, op := range ops {
			if op.Kind == dsd.KindAgentUpdate {
				found = true
			}
		}
		if !found {
			t.Errorf("desired %q vs reported %q rendered no upgrade op", tc.desired, tc.reported)
		}
	}
}
