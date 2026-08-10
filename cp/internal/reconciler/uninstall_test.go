package reconciler

import (
	"encoding/json"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// A decommissioning server's document is the uninstall op and NOTHING else.
//
// The assertion that matters is the exclusion, not the inclusion. A document
// that still carried the container graph and the host.* ops while asking the
// agent to delete all of it would have the agent racing its own 30-second
// reconcile loop — reaping a container and then restoring it from the desired
// state the same document had just re-asserted — and re-arming a firewall on a
// box that is about to have no agent at all.
func TestDecommissioningRendersOnlyTheUninstallOp(t *testing.T) {
	hardening := store.HostHardening{
		MeshIP: "10.8.0.5", MeshInterface: "sigma0",
		// Everything that would otherwise render something, on at once.
		ProxyRole: true, CISEnabled: true, DesiredAgentVersion: "v9.9.9",
		Decommissioning: true,
	}
	ops, hash := renderOps("srv_gone", dbSpecs("postgres"), nil, nil,
		hardening, nil, nil, dbTargets("postgres", "database"), nil, nil, nil, nil,
		ACMEConfig{}, clusterRender{}, registryRender{}, "")

	if len(ops) != 1 {
		ids := make([]string, 0, len(ops))
		for _, op := range ops {
			ids = append(ids, op.ID+"("+op.Kind+")")
		}
		t.Fatalf("decommissioning document has %d ops: %v — it must be the uninstall op alone", len(ops), ids)
	}
	op := ops[0]
	if op.Kind != dsd.KindAgentUninstall {
		t.Fatalf("op kind = %q, want %q", op.Kind, dsd.KindAgentUninstall)
	}
	if op.ID != UninstallOpID("srv_gone") {
		t.Fatalf("op id = %q, want %q", op.ID, UninstallOpID("srv_gone"))
	}
	if hash == "" {
		t.Fatal("no document hash rendered")
	}

	var spec struct {
		ServerID      string `json:"serverId"`
		PurgeVolumes  bool   `json:"purgeVolumes"`
		MeshInterface string `json:"meshInterface"`
	}
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.ServerID != "srv_gone" || spec.MeshInterface != "sigma0" {
		t.Fatalf("spec = %+v", spec)
	}
	// The default is the product promise: the machine goes, the customer's
	// data stays.
	if spec.PurgeVolumes {
		t.Fatal("purgeVolumes defaulted ON — a disconnect would destroy database volumes nobody asked it to")
	}
}

// The opt-in reaches the agent. One assignment, and without it the checkbox in
// the dialog is decoration: the CP would store the choice and render an op that
// never mentions it.
func TestPurgeVolumesOptInReachesTheOpSpec(t *testing.T) {
	ops := renderUninstallOps("srv_1", store.HostHardening{
		MeshInterface: "sigma0", Decommissioning: true, PurgeVolumes: true,
	})
	var spec struct {
		PurgeVolumes bool `json:"purgeVolumes"`
	}
	if err := json.Unmarshal(ops[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.PurgeVolumes {
		t.Fatal("the operator opted into deleting application data and the op does not say so")
	}
}

// The op id is deterministic: a resync re-renders it byte-identically, so the
// document hash does not churn and the agent's journal recognises an already-
// applied version instead of tearing the host down a second time.
func TestUninstallOpIDIsStable(t *testing.T) {
	hh := store.HostHardening{MeshInterface: "sigma0", Decommissioning: true}
	a, hashA := renderOps("srv_x", nil, nil, nil, hh, nil, nil, nil, nil, nil, nil, nil,
		ACMEConfig{}, clusterRender{}, registryRender{}, "")
	b, hashB := renderOps("srv_x", nil, nil, nil, hh, nil, nil, nil, nil, nil, nil, nil,
		ACMEConfig{}, clusterRender{}, registryRender{}, "")
	if hashA != hashB {
		t.Fatalf("hash churned across identical renders: %s vs %s", hashA, hashB)
	}
	if a[0].ID != b[0].ID || string(a[0].Spec) != string(b[0].Spec) {
		t.Fatalf("op is not deterministic: %+v vs %+v", a[0], b[0])
	}
}
