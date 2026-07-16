package reconciler

import (
	"encoding/json"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestRenderHostOpsDefault(t *testing.T) {
	ops := renderHostOps("srv_1", store.HostHardening{
		MeshIP: "10.77.0.5", MeshInterface: "sigma0",
		ProxyRole: true, KeepPublicSSH: false, CISEnabled: true,
		ExtraPorts: []store.PortException{{Port: 9000, Proto: "tcp"}},
	})
	byID := map[string]dsd.Op{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	nft, ok := byID["host:nftables:srv_1"]
	if !ok || nft.Kind != dsd.KindHostNftables {
		t.Fatal("missing host.nftables op")
	}
	var nftSpec struct {
		AllowPublicSSH bool `json:"allowPublicSSH"`
		ProxyRole      bool `json:"proxyRole"`
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
	})
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
