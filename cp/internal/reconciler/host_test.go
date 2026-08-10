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
