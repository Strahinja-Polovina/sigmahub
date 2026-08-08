package reconciler

// Cluster rendering. Every test here uses REAL identifier shapes (`res_<hex>`,
// `prj_<hex>`) rather than convenient short names, because the bug this file
// was written after was invisible to any test that did otherwise: the rendered
// workload name carried an underscore, the agent rejects it, and no cluster
// workload could ever be applied.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// agentDNSName is the agent's own validation rule, duplicated because the two
// modules can't share a package. Keeping a copy here is the point: it is what
// makes a divergence a test failure instead of a production one.
var agentDNSName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const (
	testResource = "res_0a1b2c3d4e5f6071"
	testProject  = "prj_9f8e7d6c5b4a3921"
)

func controlPlane() store.ClusterMembership {
	return store.ClusterMembership{
		ClusterID: "cls_1", Name: "prod", Role: store.NodeRoleControlPlane, JoinToken: "tok",
	}
}

func decodeApply(t *testing.T, op dsd.Op) k8sApplyOpSpec {
	t.Helper()
	var spec k8sApplyOpSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		t.Fatalf("decode %s: %v", op.ID, err)
	}
	return spec
}

func composeSpec(t *testing.T, services ...composeServiceSpec) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"env":     map[string]string{"SHARED": "1"},
		"compose": map[string]any{"services": services},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The name and namespace the control plane renders must be names the agent will
// accept. This is the whole bug in one assertion.
func TestClusterWorkloadNamesAreApplyable(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: json.RawMessage(`{"image":"nginx:1.27","ports":[{"container":80}]}`),
	}
	ops, ok := renderClusterWorkloadOps(rs, nil, nil, store.DeployTarget{}, controlPlane(), "k8s:node:cls_1", "", "")
	if !ok || len(ops) != 1 {
		t.Fatalf("ops = %+v ok = %v", ops, ok)
	}
	spec := decodeApply(t, ops[0])
	if !agentDNSName.MatchString(spec.Name) {
		t.Fatalf("workload name %q would be rejected by the agent", spec.Name)
	}
	if !agentDNSName.MatchString(spec.Namespace) {
		t.Fatalf("namespace %q would be rejected by the agent", spec.Namespace)
	}
	if spec.Image != "nginx:1.27" {
		t.Fatalf("a registry-image app must run its declared image, got %q", spec.Image)
	}
}

// A Compose app used to render NOTHING in a cluster: the plain path reads
// spec.Image, a Compose app has none, so the deploy sat in flight forever with
// no workload and no error.
func TestComposeAppRendersOneWorkloadPerService(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: composeSpec(t,
			composeServiceSpec{Name: "db", Image: "postgres:16", Ports: []int{5432}},
			composeServiceSpec{Name: "web", Build: ".", Ports: []int{8080},
				DependsOn: []string{"db"}, Env: map[string]string{"SHARED": "2", "OWN": "x"}},
		),
	}
	target := store.DeployTarget{
		DeploymentID: "dep_1", SHA: "abc1234567", ImagePin: "pin1", Status: "deploying",
		BuildServerID: "srv_build", ServiceStatus: map[string]string{"web": "deploying"},
	}
	ops, ok := renderClusterWorkloadOps(rs, nil, []store.Domain{{Domain: "app.example.com"}},
		target, controlPlane(), "k8s:node:cls_1", "ghcr.io/acme", "ghcr.io")
	if !ok || len(ops) != 2 {
		t.Fatalf("ops = %d, want one per service: %+v", len(ops), ops)
	}

	byID := map[string]k8sApplyOpSpec{}
	deps := map[string][]string{}
	for _, op := range ops {
		byID[op.ID] = decodeApply(t, op)
		deps[op.ID] = op.DependsOn
	}
	db, hasDB := byID["res:"+testResource+":db"]
	web, hasWeb := byID["res:"+testResource+":web"]
	if !hasDB || !hasWeb {
		t.Fatalf("op ids = %+v", byID)
	}
	// The per-service op id is what routes status into AdvanceDeploymentService;
	// getting it wrong means the deployment never completes.
	if db.Image != "postgres:16" {
		t.Fatalf("a prebuilt service must run its declared image, got %q", db.Image)
	}
	if web.Image != "ghcr.io/acme/"+testResource+"-web:abc1234567-pin1" {
		t.Fatalf("a built service must run its registry-qualified tag, got %q", web.Image)
	}
	// depends_on is real op ordering here: every service of a cluster app renders
	// into the same document.
	if !contains(deps["res:"+testResource+":web"], "res:"+testResource+":db") {
		t.Fatalf("web must be ordered after db: %v", deps["res:"+testResource+":web"])
	}
	if !contains(deps["res:"+testResource+":db"], "k8s:node:cls_1") {
		t.Fatalf("every workload must wait for the node: %v", deps["res:"+testResource+":db"])
	}
	// The web-facing service carries the ingress; the database must not.
	if len(web.Hosts) != 1 || web.Hosts[0] != "app.example.com" {
		t.Fatalf("web hosts = %v", web.Hosts)
	}
	if len(db.Hosts) != 0 {
		t.Fatalf("a database service must not be routed from the internet: %v", db.Hosts)
	}
	// Per-service env layers over the resource-level env.
	if web.Env["SHARED"] != "2" || web.Env["OWN"] != "x" || db.Env["SHARED"] != "1" {
		t.Fatalf("env not layered per service: web=%v db=%v", web.Env, db.Env)
	}
	// Both ops advertise the full workload set so the agent can prune.
	for id, spec := range byID {
		if len(spec.Workloads) != 2 {
			t.Fatalf("%s carries %d workload names, want the full set: %v", id, len(spec.Workloads), spec.Workloads)
		}
		if !agentDNSName.MatchString(spec.Name) {
			t.Fatalf("%s renders an unapplyable name %q", id, spec.Name)
		}
	}
}

// A service whose build has not reported yet must not be applied: its image tag
// does not exist in the registry, and a manifest pointing at it is an
// ImagePullBackOff the product would report as a successful apply.
func TestClusterComposeGatesOnEachServiceBuild(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: composeSpec(t,
			composeServiceSpec{Name: "web", Build: ".", Ports: []int{8080}},
			composeServiceSpec{Name: "worker", Build: "./worker"},
		),
	}
	target := store.DeployTarget{
		DeploymentID: "dep_1", SHA: "abc1234567", ImagePin: "pin1", Status: "deploying",
		BuildServerID: "srv_build", ServiceStatus: map[string]string{"web": "deploying"},
	}
	ops, ok := renderClusterWorkloadOps(rs, nil, nil, target, controlPlane(), "k8s:node:cls_1", "ghcr.io/acme", "ghcr.io")
	if !ok || len(ops) != 1 {
		t.Fatalf("only the built service may render, got %d ops: %+v", len(ops), ops)
	}
	if ops[0].ID != "res:"+testResource+":web" {
		t.Fatalf("rendered the wrong service: %s", ops[0].ID)
	}
	// The gated service must not be referenced as a dependency either — a
	// dangling op id wedges the whole apply, not just that workload.
	for _, d := range ops[0].DependsOn {
		if strings.HasSuffix(d, ":worker") {
			t.Fatalf("dependency on an unrendered service: %v", ops[0].DependsOn)
		}
	}
	// It is still in the prune set, so being behind does not delete it.
	if len(decodeApply(t, ops[0]).Workloads) != 2 {
		t.Fatal("a service waiting on its build must not be pruned away by the ones ahead of it")
	}
}

// Nothing on a cluster builds images, so a source deploy with no registry (or
// no build server) can only ever produce a manifest referencing a tag that does
// not exist. Render nothing instead.
func TestClusterSourceDeployNeedsRegistryAndBuildServer(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}
	base := store.DeployTarget{DeploymentID: "dep_1", SHA: "abc1234567", ImagePin: "pin1", Status: "deploying"}

	noRegistry := base
	noRegistry.BuildServerID = "srv_build"
	if _, ok := renderClusterWorkloadOps(rs, nil, nil, noRegistry, controlPlane(), "k8s:node:cls_1", "", ""); ok {
		t.Fatal("a source build with no registry must render nothing — the nodes have nowhere to pull from")
	}

	noBuilder := base
	if _, ok := renderClusterWorkloadOps(rs, nil, nil, noBuilder, controlPlane(), "k8s:node:cls_1", "ghcr.io/acme", "ghcr.io"); ok {
		t.Fatal("a source build with no build server must render nothing — nothing would produce the image")
	}

	inFlight := base
	inFlight.BuildServerID, inFlight.Status = "srv_build", "building"
	if _, ok := renderClusterWorkloadOps(rs, nil, nil, inFlight, controlPlane(), "k8s:node:cls_1", "ghcr.io/acme", "ghcr.io"); ok {
		t.Fatal("a workload must wait for its build rather than apply a tag that does not exist yet")
	}
}

// Workloads render into the control-plane document only: kubectl talks to the
// API server and the scheduler picks the node, so a worker rendering the same
// Deployment would be a second competing applier.
func TestWorkerRendersNoWorkloads(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: json.RawMessage(`{"image":"nginx:1.27"}`),
	}
	worker := controlPlane()
	worker.Role = store.NodeRoleWorker
	if _, ok := renderClusterWorkloadOps(rs, nil, nil, store.DeployTarget{}, worker, "k8s:node:cls_1", "", ""); ok {
		t.Fatal("a worker must not apply workloads")
	}
}

// The build for a cluster workload has to land in the build server's document —
// a cluster resource belongs to no server, so before this it landed nowhere and
// the manifest referenced an image nothing had been asked to produce.
func TestClusterBuildOpsPushToTheRegistry(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: composeSpec(t,
			composeServiceSpec{Name: "db", Image: "postgres:16"},
			composeServiceSpec{Name: "web", Build: ".", Ports: []int{8080}},
		),
	}
	target := store.DeployTarget{
		DeploymentID: "dep_1", SHA: "abc1234567", ImagePin: "pin1", Status: "queued",
		BuildServerID: "srv_build", Provider: "github", RepoFullName: "acme/app", Ref: "main",
	}
	ops, ok := renderClusterBuildOps(rs, target, "ghcr.io/acme")
	if !ok {
		t.Fatal("no build ops rendered")
	}
	var builds []buildImageOpSpec
	sawClone := false
	for _, op := range ops {
		switch op.Kind {
		case dsd.KindGitClone:
			sawClone = true
		case dsd.KindImageBuild:
			var b buildImageOpSpec
			if err := json.Unmarshal(op.Spec, &b); err != nil {
				t.Fatal(err)
			}
			builds = append(builds, b)
		}
	}
	if !sawClone {
		t.Fatal("a source build needs the repo")
	}
	// Only the service with a build context: a prebuilt image is pulled by the
	// nodes directly and building it here would be nonsense.
	if len(builds) != 1 {
		t.Fatalf("builds = %d, want one (only 'web' builds from source): %+v", len(builds), builds)
	}
	b := builds[0]
	if !b.PushImage {
		t.Fatal("a cluster build must push — the nodes, not this machine, run the result")
	}
	if b.RegistryHost != "" && b.RegistryHost != "ghcr.io" {
		t.Fatalf("registry host = %q", b.RegistryHost)
	}
	if b.ImageTag != "ghcr.io/acme/"+testResource+"-web:abc1234567-pin1" {
		t.Fatalf("build tag %q is not registry-qualified", b.ImageTag)
	}
	if b.ContextSubdir != "." {
		t.Fatalf("build context = %q", b.ContextSubdir)
	}

	// Without a registry there is nowhere to push, so there is no point building.
	if _, ok := renderClusterBuildOps(rs, target, ""); ok {
		t.Fatal("a build with no registry must not render")
	}
}

// The dedicated build server has the same problem as a cluster: the image
// crosses a host boundary. Its tag has to name the registry, and without one
// the pipeline cannot complete, so it must not render.
func TestDedicatedBuildServerQualifiesTheImage(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}
	target := store.DeployTarget{
		DeploymentID: "dep_1", SHA: "abc1234567", ImagePin: "pin1", Status: "queued",
		ServerID: "srv_run", BuildServerID: "srv_build",
	}
	reg := registryRender{repository: "ghcr.io/acme", host: "ghcr.io"}

	ops, _, ok := renderDeployOps(rs, nil, nil, target, "srv_build", reg)
	if !ok {
		t.Fatal("the build server must render the pipeline")
	}
	var build buildImageOpSpec
	for _, op := range ops {
		if op.Kind == dsd.KindImageBuild {
			if err := json.Unmarshal(op.Spec, &build); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !build.PushImage || build.RegistryHost != "ghcr.io" {
		t.Fatalf("cross-host build must push with credentials: %+v", build)
	}
	if !strings.HasPrefix(build.ImageTag, "ghcr.io/acme/") {
		t.Fatalf("cross-host image tag %q would resolve to docker.io", build.ImageTag)
	}

	if _, _, ok := renderDeployOps(rs, nil, nil, target, "srv_build", registryRender{}); ok {
		t.Fatal("a cross-host build with no registry must not render: the push can only 401")
	}
}

// A same-host build pushes nothing, so it must not be told to authenticate
// against a registry it has no reason to touch.
func TestSameHostBuildNeedsNoRegistry(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}
	target := store.DeployTarget{
		DeploymentID: "dep_1", SHA: "abc1234567", ImagePin: "pin1", Status: "queued", ServerID: "srv_run",
	}
	ops, _, ok := renderDeployOps(rs, nil, nil, target, "srv_run", registryRender{repository: "ghcr.io/acme", host: "ghcr.io"})
	if !ok {
		t.Fatal("a same-host deploy must render")
	}
	for _, op := range ops {
		if op.Kind != dsd.KindImageBuild {
			continue
		}
		var b buildImageOpSpec
		if err := json.Unmarshal(op.Spec, &b); err != nil {
			t.Fatal(err)
		}
		if b.PushImage || b.RegistryHost != "" {
			t.Fatalf("a same-host build must not push or authenticate: %+v", b)
		}
		if strings.Contains(b.ImageTag, "ghcr.io") {
			t.Fatalf("a same-host image must keep its local tag, got %q", b.ImageTag)
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
