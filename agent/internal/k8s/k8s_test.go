package k8s

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
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

// Writing a manifest into k3s's auto-apply directory is not the same as running
// a workload: the image may not pull, the pods may crash-loop, the scheduler may
// have nowhere to put them. Returning nil at the moment of the write turns every
// one of those into a green deploy — the op reports applied, the deployment
// flips to success, and the release is recorded as a rollback target while no
// pod has ever served a request. The op must fail, and the failure must carry
// the cluster's own account of why, the same way the container path's health
// gate ships the failed container's startup logs.
func TestApplyWorkloadFailsWhenDeploymentNeverBecomesAvailable(t *testing.T) {
	d, cap := testDriver(t)
	d.rolloutTimeout = 30 * time.Millisecond
	d.rolloutInterval = time.Millisecond
	base := d.runner
	d.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if !strings.HasSuffix(name, "kubectl") {
			return base(ctx, name, args...)
		}
		cap.commands = append(cap.commands, append([]string{name}, args...))
		switch {
		case hasArg(args, "rollout"):
			// A stuck rollout: kubectl exits non-zero once its own timeout lapses.
			return []byte(`Waiting for deployment "api" rollout to finish: 0 of 2 updated replicas are available...
error: timed out waiting for the condition`), errors.New("exit status 1")
		case hasArg(args, "describe"):
			return []byte("Events:\n  Warning  Failed  Failed to pull image \"img:1\": not found"), nil
		case hasArg(args, "logs"):
			return []byte("Error from server (BadRequest): container is waiting to start"), nil
		}
		return nil, nil
	}

	spec := ApplySpec{
		ResourceID: "res_1", Name: "api", Namespace: "sigmahub-proj",
		Image: "img:1", Replicas: 2, Ports: []int{8080},
	}
	raw, _ := json.Marshal(spec)
	err := d.applyWorkload(context.Background(), dsd.Op{ID: "res:res_1", Kind: KindK8sApply, Spec: raw})
	if err == nil {
		t.Fatal("a workload whose rollout never completes must fail the op, not report a deploy that never ran")
	}
	if !strings.Contains(err.Error(), "Failed to pull image") {
		t.Fatalf("the failure must carry the cluster's own diagnosis, got: %v", err)
	}
	// The manifest stays on disk: k3s keeps retrying it, and a rollout that
	// completes a minute later must not need a human to re-create the file.
	if len(cap.files["/manifests/api.yaml"]) == 0 {
		t.Fatal("a failed rollout must not delete the manifest")
	}

	// The gate has to ask about THIS deployment in THIS namespace — a rollout
	// status read from the wrong namespace is a green light for nothing.
	var gate []string
	for _, c := range cap.commands {
		if hasArg(c, "rollout") {
			gate = c
		}
	}
	if gate == nil {
		t.Fatalf("no rollout status was ever requested: %v", cap.commands)
	}
	joined := strings.Join(gate, " ")
	for _, want := range []string{"-n sigmahub-proj", "rollout status", "deployment/api"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("rollout gate %q missing %q", joined, want)
		}
	}

	// And a rollout that does complete still applies cleanly.
	d.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.HasSuffix(name, "kubectl") {
			return []byte(`deployment "api" successfully rolled out`), nil
		}
		return base(ctx, name, args...)
	}
	if err := d.applyWorkload(context.Background(), dsd.Op{ID: "res:res_1", Kind: KindK8sApply, Spec: raw}); err != nil {
		t.Fatalf("a completed rollout must apply cleanly: %v", err)
	}
}

// SIGMA-312: a deleted cluster resource (or a deleted cluster) tears its
// workloads down through a typed op of its own.
//
// Pruning only ever happened as a side effect of applying a workload, so a
// resource with no apply op left — which is exactly what a deletion produces —
// kept its Deployment, Service and Ingress running in k3s with nothing in the
// product describing them. The op goes through the apply registry like every
// other kind, because a kind registered nowhere is rejected by Apply and would
// leave the control plane rendering a teardown no agent performs.
func TestK8sRemoveDeletesTheWorkloadManifests(t *testing.T) {
	d, cap, _ := reportingDriver(t)
	cap.files["/manifests/sigmahub-res-1-web.yaml"] = []byte("web")
	cap.files["/manifests/sigmahub-res-1-worker.yaml"] = []byte("worker")
	// Another resource's manifest must survive a teardown that does not name it.
	cap.files["/manifests/sigmahub-res-2-web.yaml"] = []byte("other resource")

	raw, err := json.Marshal(map[string]any{
		"resourceId": "res_1",
		"workloads":  []string{"sigmahub-res-1-web", "sigmahub-res-1-worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := apply.NewRegistry()
	d.Register(reg)
	j, err := apply.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	doc := dsd.Document{Version: 3, ServerID: "srv_cp", Ops: []dsd.Op{
		{ID: "k8srm:pdo_1", Kind: KindK8sRemove, Spec: raw},
	}}
	results, err := reg.Apply(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), j, doc)
	if err != nil {
		t.Fatal(err)
	}
	if res := results["k8srm:pdo_1"]; res.State != "applied" {
		t.Fatalf("op state = %q (%s), want applied", res.State, res.Err)
	}
	for _, gone := range []string{"/manifests/sigmahub-res-1-web.yaml", "/manifests/sigmahub-res-1-worker.yaml"} {
		if _, still := cap.files[gone]; still {
			t.Fatalf("%s survived the teardown: the workload keeps running", gone)
		}
	}
	if _, ok := cap.files["/manifests/sigmahub-res-2-web.yaml"]; !ok {
		t.Fatal("a teardown removed a manifest it does not name")
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
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
