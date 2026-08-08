package k8s

// Per-service manifests, pruning, registry pull secrets, and the node report —
// the four things that stand between "the control plane rendered ops" and "a
// Compose app is actually running in the cluster".

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// reportingDriver is testDriver plus the pieces the cluster paths need: a
// manifest directory it can list and delete from, and a captured report.
func reportingDriver(t *testing.T) (*Driver, *capture, *[]NodeReport) {
	t.Helper()
	d, cap := testDriver(t)
	var reports []NodeReport
	d.report = func(_ context.Context, rep NodeReport) error {
		reports = append(reports, rep)
		return nil
	}
	d.readDir = func(string) ([]string, error) {
		names := make([]string, 0, len(cap.files))
		for path := range cap.files {
			names = append(names, path[strings.LastIndexByte(path, '/')+1:])
		}
		return names, nil
	}
	d.removeFile = func(path string) error {
		if _, ok := cap.files[path]; !ok {
			return os.ErrNotExist
		}
		delete(cap.files, path)
		cap.removed = append(cap.removed, path)
		return nil
	}
	return d, cap, &reports
}

func applyOp(t *testing.T, spec ApplySpec) dsd.Op {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return dsd.Op{ID: "res:" + spec.ResourceID, Kind: KindK8sApply, Spec: raw}
}

// A Compose app is N workloads. Writing them all to one file, keyed by the
// resource, would have each service silently overwrite the last.
func TestEachServiceOwnsItsManifest(t *testing.T) {
	d, cap, _ := reportingDriver(t)
	for _, svc := range []string{"web", "worker"} {
		op := applyOp(t, ApplySpec{
			ResourceID: "res_1", Service: svc, Name: "sigmahub-res-1-" + svc,
			Namespace: "sigmahub-prj-1", Image: "img:1",
			Workloads: []string{"sigmahub-res-1-web", "sigmahub-res-1-worker"},
		})
		if err := d.applyWorkload(context.Background(), op); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{"/manifests/sigmahub-res-1-web.yaml", "/manifests/sigmahub-res-1-worker.yaml"} {
		if len(cap.files[want]) == 0 {
			t.Fatalf("missing %s, have %v", want, keys(cap.files))
		}
	}
}

// k3s keeps applying whatever is in the manifest directory, so a service
// removed from the Compose file keeps running forever unless its file goes too.
func TestRemovedServiceIsPruned(t *testing.T) {
	d, cap, _ := reportingDriver(t)
	both := []string{"sigmahub-res-1-web", "sigmahub-res-1-worker"}
	for _, svc := range []string{"web", "worker"} {
		if err := d.applyWorkload(context.Background(), applyOp(t, ApplySpec{
			ResourceID: "res_1", Service: svc, Name: "sigmahub-res-1-" + svc,
			Namespace: "ns", Image: "img:1", Workloads: both,
		})); err != nil {
			t.Fatal(err)
		}
	}
	// A manifest belonging to a DIFFERENT resource must survive.
	cap.files["/manifests/sigmahub-res-2-web.yaml"] = []byte("other resource")

	// 'worker' is dropped from the Compose file: the next apply of 'web' carries
	// the shortened set.
	if err := d.applyWorkload(context.Background(), applyOp(t, ApplySpec{
		ResourceID: "res_1", Service: "web", Name: "sigmahub-res-1-web",
		Namespace: "ns", Image: "img:2", Workloads: []string{"sigmahub-res-1-web"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, still := cap.files["/manifests/sigmahub-res-1-worker.yaml"]; still {
		t.Fatal("a service removed from the Compose file must stop running")
	}
	if _, gone := cap.files["/manifests/sigmahub-res-2-web.yaml"]; !gone {
		t.Fatal("pruning one resource must not touch another's manifests")
	}
	if len(cap.files["/manifests/sigmahub-res-1-web.yaml"]) == 0 {
		t.Fatal("the surviving service lost its manifest")
	}
}

// An op with no workload set (an older control plane) must prune nothing —
// deleting every manifest because the field is absent would take the whole app
// down on a version skew.
func TestNoWorkloadSetPrunesNothing(t *testing.T) {
	d, cap, _ := reportingDriver(t)
	cap.files["/manifests/sigmahub-res-1-worker.yaml"] = []byte("existing")
	if err := d.applyWorkload(context.Background(), applyOp(t, ApplySpec{
		ResourceID: "res_1", Service: "web", Name: "sigmahub-res-1-web", Namespace: "ns", Image: "img:1",
	})); err != nil {
		t.Fatal(err)
	}
	if _, ok := cap.files["/manifests/sigmahub-res-1-worker.yaml"]; !ok {
		t.Fatal("an op with no workload set must not delete anything")
	}
}

// The node that runs the pod needs the registry credential, not just whoever
// built the image — otherwise every image we push is an ImagePullBackOff.
func TestPrivateImageGetsAPullSecret(t *testing.T) {
	d, cap, _ := reportingDriver(t)
	d.fetchRegistry = func(context.Context) (RegistryCredential, error) {
		return RegistryCredential{Host: "ghcr.io", Username: "bot", Password: "s3cret"}, nil
	}
	if err := d.applyWorkload(context.Background(), applyOp(t, ApplySpec{
		ResourceID: "res_1", Name: "sigmahub-res-1", Namespace: "ns",
		Image: "ghcr.io/acme/res_1:abc", RegistryHost: "ghcr.io",
	})); err != nil {
		t.Fatal(err)
	}
	manifest := string(cap.files["/manifests/sigmahub-res-1.yaml"])
	for _, want := range []string{
		"kubernetes.io/dockerconfigjson", "imagePullSecrets", "sigmahub-res-1-registry",
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("manifest missing %q:\n%s", want, manifest)
		}
	}
	// The password must be encoded inside the Secret, never a bare field of the
	// pod spec where a manifest listing would show it.
	if strings.Contains(manifest, "password: s3cret") {
		t.Fatalf("registry password emitted in plaintext:\n%s", manifest)
	}

	// A credential that cannot be resolved fails the op. Applying a manifest
	// that can only ImagePullBackOff would report success for a workload that
	// never starts.
	d.fetchRegistry = func(context.Context) (RegistryCredential, error) {
		return RegistryCredential{}, errors.New("control plane unreachable")
	}
	if err := d.applyWorkload(context.Background(), applyOp(t, ApplySpec{
		ResourceID: "res_2", Name: "sigmahub-res-2", Namespace: "ns",
		Image: "ghcr.io/acme/res_2:abc", RegistryHost: "ghcr.io",
	})); err == nil {
		t.Fatal("an unresolvable registry credential must fail the op")
	}
}

// The control plane is the only thing that knows whether the API server is up,
// and "the systemd unit is active" is not the same answer: k3s is active while
// it is still starting.
func TestControlPlaneReportsOnlyWhenTheAPIServerAnswers(t *testing.T) {
	d, cap, reports := reportingDriver(t)
	cap.active["k3s"] = true
	d.binDir = "/usr/local/bin"

	// kubectl cannot reach the API server yet.
	base := d.runner
	d.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.HasSuffix(name, "kubectl") {
			return []byte("The connection to the server was refused"), errors.New("exit 1")
		}
		return base(ctx, name, args...)
	}
	if err := d.applyNode(context.Background(), nodeOp(t, NodeSpec{
		ClusterID: "c1", Role: RoleControlPlane, JoinToken: "tok", AdvertiseIP: "10.8.0.2",
	})); err != nil {
		t.Fatal(err)
	}
	if len(*reports) != 1 {
		t.Fatalf("reports = %+v", *reports)
	}
	if (*reports)[0].Ready {
		t.Fatal("a control plane whose API server is refusing connections must not report ready")
	}
	if (*reports)[0].Message == "" {
		t.Fatal("a not-ready report must say why — that message is the whole diagnostic")
	}

	// Now it answers.
	d.runner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.HasSuffix(name, "kubectl") {
			return []byte(`{"serverVersion":{"gitVersion":"v1.31.4+k3s1"}}`), nil
		}
		return base(ctx, name, args...)
	}
	if err := d.applyNode(context.Background(), nodeOp(t, NodeSpec{
		ClusterID: "c1", Role: RoleControlPlane, JoinToken: "tok", AdvertiseIP: "10.8.0.2",
	})); err != nil {
		t.Fatal(err)
	}
	last := (*reports)[len(*reports)-1]
	if !last.Ready || last.Version != "v1.31.4+k3s1" {
		t.Fatalf("ready report = %+v", last)
	}
	if last.APIEndpoint != "https://10.8.0.2:6443" {
		t.Fatalf("the endpoint must be the mesh address: %q", last.APIEndpoint)
	}
}

// A failure the control plane never hears about is the exact state this
// reporting exists to end: the cluster would read 'provisioning' forever with
// no indication that anything went wrong.
func TestFailedNodeStillReports(t *testing.T) {
	d, _, reports := reportingDriver(t)
	d.euid = 1000 // not root: the install cannot proceed

	if err := d.applyNode(context.Background(), nodeOp(t, NodeSpec{
		ClusterID: "c1", Role: RoleControlPlane, JoinToken: "tok", AdvertiseIP: "10.8.0.2",
	})); err == nil {
		t.Fatal("a node that cannot install must fail its op")
	}
	if len(*reports) != 1 || (*reports)[0].Ready || (*reports)[0].Message == "" {
		t.Fatalf("a failed node must report the failure: %+v", *reports)
	}
	if (*reports)[0].ClusterID != "c1" {
		t.Fatalf("report is not attributed to a cluster: %+v", (*reports)[0])
	}
}

// A worker has no API server to interrogate; asking the control plane's would
// prove something about that node, not this one.
func TestWorkerReportsItsOwnService(t *testing.T) {
	d, cap, reports := reportingDriver(t)
	cap.active["k3s-agent"] = true

	if err := d.applyNode(context.Background(), nodeOp(t, NodeSpec{
		ClusterID: "c1", Role: RoleWorker, JoinToken: "tok",
		AdvertiseIP: "10.8.0.5", ServerURL: "https://10.8.0.2:6443",
	})); err != nil {
		t.Fatal(err)
	}
	if len(*reports) != 1 || !(*reports)[0].Ready {
		t.Fatalf("a running worker must report ready: %+v", *reports)
	}
	if (*reports)[0].APIEndpoint != "" || (*reports)[0].Version != "" {
		t.Fatalf("a worker must not claim to describe the API server: %+v", (*reports)[0])
	}
}
