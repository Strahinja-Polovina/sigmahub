package reconciler

import (
	"encoding/json"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func opByID(ops []dsd.Op, id string) (dsd.Op, bool) {
	for _, op := range ops {
		if op.ID == id {
			return op, true
		}
	}
	return dsd.Op{}, false
}

func TestRenderAppResourceFansIntoContainerOps(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{
		"image":   "nginxinc/nginx-unprivileged:1.27-alpine",
		"ports":   []map[string]any{{"container": 8080, "host": 8080}},
		"volumes": []map[string]any{{"name": "data", "mountPath": "/data"}},
		"user":    "101:101",
	})
	specs := []store.ResourceSpec{
		{ResourceID: "res_a", ProjectID: "proj_x", Kind: "app", Spec: spec},
	}
	ops, hash := renderOps("srv_t", specs, nil, nil, store.HostHardening{}, nil, nil, nil, ACMEConfig{})
	if hash == "" {
		t.Fatal("empty doc hash")
	}

	// network.ensure (deduped per project), image.pull, volume.ensure, container.apply
	if _, ok := opByID(ops, "net:proj_x"); !ok {
		t.Fatal("missing network op")
	}
	if _, ok := opByID(ops, "img:res_a"); !ok {
		t.Fatal("missing image op")
	}
	if _, ok := opByID(ops, "vol:res_a:data"); !ok {
		t.Fatal("missing volume op")
	}
	ctr, ok := opByID(ops, "res:res_a")
	if !ok {
		t.Fatal("missing container op (must keep res: id for status write-back)")
	}
	if ctr.Kind != dsd.KindContainerApply {
		t.Fatalf("container op kind = %q", ctr.Kind)
	}
	// container depends on network, image, and volume so the agent applies in order
	deps := map[string]bool{}
	for _, d := range ctr.DependsOn {
		deps[d] = true
	}
	for _, want := range []string{"net:proj_x", "img:res_a", "vol:res_a:data"} {
		if !deps[want] {
			t.Fatalf("container op missing dependency %q (deps=%v)", want, ctr.DependsOn)
		}
	}
	// container spec carries the resolved docker names
	var cs struct {
		Name    string `json:"name"`
		Network string `json:"network"`
		Image   string `json:"image"`
		Volumes []struct {
			Name      string `json:"name"`
			MountPath string `json:"mountPath"`
		} `json:"volumes"`
	}
	if err := json.Unmarshal(ctr.Spec, &cs); err != nil {
		t.Fatal(err)
	}
	if cs.Name != dsd.ContainerName("res_a") || cs.Network != dsd.NetworkName("proj_x") {
		t.Fatalf("names not resolved: %+v", cs)
	}
	if len(cs.Volumes) != 1 || cs.Volumes[0].Name != dsd.VolumeName("res_a", "data") {
		t.Fatalf("volume not resolved: %+v", cs.Volumes)
	}
}

func TestRenderDedupsProjectNetwork(t *testing.T) {
	mk := func(id string) store.ResourceSpec {
		spec, _ := json.Marshal(map[string]any{"image": "nginx:1.27"})
		return store.ResourceSpec{ResourceID: id, ProjectID: "proj_x", Kind: "app", Spec: spec}
	}
	ops, _ := renderOps("srv_t", []store.ResourceSpec{mk("res_a"), mk("res_b")}, nil, nil, store.HostHardening{}, nil, nil, nil, ACMEConfig{})
	netCount := 0
	for _, op := range ops {
		if op.Kind == dsd.KindNetworkEnsure {
			netCount++
		}
	}
	if netCount != 1 {
		t.Fatalf("expected 1 deduped network op for two same-project apps, got %d", netCount)
	}
}

func TestRenderStubFallback(t *testing.T) {
	// Non-app kind and an app with no image both fall back to resource.sync.
	noImage, _ := json.Marshal(map[string]any{"env": map[string]string{"K": "V"}})
	specs := []store.ResourceSpec{
		{ResourceID: "res_db", ProjectID: "proj_x", Kind: "postgres", Spec: json.RawMessage(`{}`)},
		{ResourceID: "res_app", ProjectID: "proj_x", Kind: "app", Spec: noImage},
	}
	ops, _ := renderOps("srv_t", specs, nil, nil, store.HostHardening{}, nil, nil, nil, ACMEConfig{})
	for _, id := range []string{"res:res_db", "res:res_app"} {
		op, ok := opByID(ops, id)
		if !ok || op.Kind != dsd.KindResourceSync {
			t.Fatalf("expected resource.sync stub for %s, got %+v (ok=%v)", id, op, ok)
		}
	}
}

func TestRenderAppendsDestructiveOps(t *testing.T) {
	pending := []store.PendingDestructiveOp{
		{ID: "pdo_1", OpKind: dsd.KindVolumeRemove, Target: "sigmahub-res_a-data"},
	}
	ops, _ := renderOps("srv_t", nil, pending, nil, store.HostHardening{}, nil, nil, nil, ACMEConfig{})
	op, ok := opByID(ops, "volrm:pdo_1")
	if !ok || op.Kind != dsd.KindVolumeRemove {
		t.Fatalf("missing volume.remove op: %+v (ok=%v)", op, ok)
	}
	var vs struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(op.Spec, &vs)
	if vs.Name != "sigmahub-res_a-data" {
		t.Fatalf("volume remove target = %q", vs.Name)
	}
}
