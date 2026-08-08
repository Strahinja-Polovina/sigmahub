package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// testDriver returns a driver whose host effects are captured, not performed.
func testDriver(t *testing.T) (*Driver, *capture) {
	t.Helper()
	cap := &capture{
		files:  map[string][]byte{},
		perms:  map[string]os.FileMode{},
		active: map[string]bool{},
	}
	return &Driver{
		runner: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "systemctl" && len(args) == 2 && args[0] == "is-active" {
				if cap.active[args[1]] {
					return []byte("active\n"), nil
				}
				return []byte("inactive\n"), os.ErrNotExist
			}
			cap.commands = append(cap.commands, append([]string{name}, args...))
			return nil, nil
		},
		installScript: func(_ context.Context, env []string, args ...string) error {
			cap.installs = append(cap.installs, install{env: env, args: args})
			return nil
		},
		writeFile: func(path string, data []byte, perm os.FileMode) error {
			cap.files[path] = data
			cap.perms[path] = perm
			return nil
		},
		mkdirAll:    func(string, os.FileMode) error { return nil },
		euid:        0,
		manifestDir: "/manifests",
	}, cap
}

type install struct {
	env  []string
	args []string
}

type capture struct {
	installs []install
	commands [][]string
	files    map[string][]byte
	perms    map[string]os.FileMode
	active   map[string]bool
	removed  []string
}

func nodeOp(t *testing.T, spec NodeSpec) dsd.Op {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return dsd.Op{ID: "k8s:node:c1", Kind: KindK8sNode, Spec: raw}
}

func TestControlPlaneBindsToTheMeshAddress(t *testing.T) {
	d, cap := testDriver(t)

	err := d.applyNode(context.Background(), nodeOp(t, NodeSpec{
		ClusterID: "c1", Name: "prod", Role: RoleControlPlane,
		JoinToken: "tok", AdvertiseIP: "10.8.0.2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cap.installs) != 1 {
		t.Fatalf("installs = %d, want 1", len(cap.installs))
	}
	got := strings.Join(cap.installs[0].args, " ")
	// The API server must never be published on a public interface — the same
	// mesh-only invariant databases and object storage hold to.
	for _, want := range []string{
		"server", "--advertise-address 10.8.0.2", "--bind-address 10.8.0.2",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("control-plane args missing %q: %s", want, got)
		}
	}
	if !strings.Contains(got, "--disable traefik") {
		t.Fatalf("k3s traefik must be disabled (we run our own proxy): %s", got)
	}
	if !containsEnv(cap.installs[0].env, "K3S_TOKEN=tok") {
		t.Fatalf("join token not passed: %v", cap.installs[0].env)
	}
}

func TestWorkerJoinsTheControlPlane(t *testing.T) {
	d, cap := testDriver(t)

	err := d.applyNode(context.Background(), nodeOp(t, NodeSpec{
		ClusterID: "c1", Role: RoleWorker, JoinToken: "tok",
		AdvertiseIP: "10.8.0.5", ServerURL: "https://10.8.0.2:6443",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cap.installs[0].args[0] != "agent" {
		t.Fatalf("worker must install as an agent: %v", cap.installs[0].args)
	}
	if !containsEnv(cap.installs[0].env, "K3S_URL=https://10.8.0.2:6443") {
		t.Fatalf("worker must dial the control plane: %v", cap.installs[0].env)
	}
}

func TestWorkerWithoutServerURLIsRefused(t *testing.T) {
	d, _ := testDriver(t)
	err := d.applyNode(context.Background(), nodeOp(t, NodeSpec{
		ClusterID: "c1", Role: RoleWorker, JoinToken: "tok", AdvertiseIP: "10.8.0.5",
	}))
	if err == nil {
		t.Fatal("a worker with nothing to join must fail loudly, not install a stray node")
	}
}

func TestNodeApplyIsIdempotent(t *testing.T) {
	d, cap := testDriver(t)
	cap.active["k3s"] = true // already running

	err := d.applyNode(context.Background(), nodeOp(t, NodeSpec{
		ClusterID: "c1", Role: RoleControlPlane, JoinToken: "tok", AdvertiseIP: "10.8.0.2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	// A resync must not reinstall k3s under running workloads.
	if len(cap.installs) != 0 {
		t.Fatalf("an active node must not be reinstalled: %+v", cap.installs)
	}
}

func TestWorkloadManifestCarriesSecretsSafely(t *testing.T) {
	d, cap := testDriver(t)
	d.fetchSecrets = func(context.Context, string) ([]Secret, error) {
		return []Secret{
			// A value containing YAML structure must not break out of its field.
			{Name: "DATABASE_URL", Value: "postgres://u:p@h:5432/db\nevil: true", EnvVar: true},
			{Name: "TLS_KEY", Value: "-----BEGIN KEY-----", EnvVar: false},
		}, nil
	}
	spec := ApplySpec{
		ResourceID: "res_1", Name: "api", Namespace: "sigmahub-proj", Image: "img:1",
		Replicas: 2, Ports: []int{8080},
		Env:        map[string]string{"LOG_LEVEL": "info"},
		SecretRefs: []SecretRef{{Name: "DATABASE_URL", EnvVar: true}, {Name: "TLS_KEY"}},
		Hosts:      []string{"app.example.com"},
	}
	raw, _ := json.Marshal(spec)
	if err := d.applyWorkload(context.Background(), dsd.Op{Kind: KindK8sApply, Spec: raw}); err != nil {
		t.Fatal(err)
	}

	manifest := string(cap.files["/manifests/api.yaml"])
	if manifest == "" {
		t.Fatalf("no manifest written: %v", keys(cap.files))
	}
	// The secret VALUE is base64 in the Secret object and never inline in the
	// Deployment, so a leaked manifest listing doesn't leak the plaintext line.
	if strings.Contains(manifest, "evil: true") {
		t.Fatal("a secret value must be quoted/encoded, never emitted as structure")
	}
	enc := base64.StdEncoding.EncodeToString([]byte("postgres://u:p@h:5432/db\nevil: true"))
	if !strings.Contains(manifest, enc) {
		t.Fatal("env secret missing from the Secret object")
	}
	for _, want := range []string{
		"kind: Namespace", "kind: Deployment", "kind: Service", "kind: Ingress",
		"replicas: 2", `image: "img:1"`, `host: "app.example.com"`,
		"secretKeyRef", secretsMountDir,
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}
	// The manifest embeds secret material, so it must not be world-readable.
	if p := cap.perms["/manifests/api.yaml"]; p != 0o600 {
		t.Fatalf("manifest perms = %v, want 0600", p)
	}
}

func TestWorkloadRejectsInvalidNames(t *testing.T) {
	d, _ := testDriver(t)
	raw, _ := json.Marshal(ApplySpec{ResourceID: "r", Name: "Not_A_Name", Image: "img"})
	if err := d.applyWorkload(context.Background(), dsd.Op{Kind: KindK8sApply, Spec: raw}); err == nil {
		t.Fatal("an invalid Kubernetes name must fail the op, not write a manifest the API server rejects")
	}
}

func TestManifestIsDeterministic(t *testing.T) {
	// A resync must not rewrite the file just because Go map order changed.
	spec := ApplySpec{
		ResourceID: "r", Name: "api", Namespace: "ns", Image: "img", Ports: []int{80},
		Env: map[string]string{"B": "2", "A": "1", "C": "3"},
	}
	first, err := renderManifests(spec, "ns", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := renderManifests(spec, "ns", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatal("manifest rendering is not deterministic")
		}
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
