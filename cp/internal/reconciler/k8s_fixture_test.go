package reconciler

// The control-plane half of the k8s.apply wire contract (SIGMA-338).
//
// k8sApplyOpSpec says "mirrors the agent's k8s.ApplySpec", and until this file
// nothing checked that it did. The two structs live in two Go modules that
// cannot import each other, so the mirror is maintained by hand, and every test
// on either side of it works from a value that side constructed itself: the CP
// tests decode the rendered op back into k8sApplyOpSpec (the same struct that
// wrote it, so any tag is self-consistent), and the agent's manifest tests —
// including the real-Kubernetes e2e — build an ApplySpec literal in the test
// body. Neither can see the gap between them.
//
// The gap is not theoretical. Renaming this file's `hosts` tag to `domains`
// drops every cluster ingress: a customer's domain 404s, no Ingress object is
// ever created, the apply reports success and the deployment reports success.
// That mutation was applied and the whole cp module AND the whole agent module
// stayed green.
//
// So the CP writes the specs it would actually send to testdata/, checked in,
// and the agent decodes that file into its own ApplySpec
// (agent/internal/k8s/cp_spec_fixture_test.go) and feeds it to the real API
// server in the e2e. A tag the agent has no field for is a decode failure; a
// value the CP stops setting is an assertion failure. Regenerate with:
//
//	SIGMAHUB_UPDATE_K8S_FIXTURE=1 go test ./internal/reconciler -run TestK8sApplyFixtureIsUpToDate
//
// and read the diff before committing it — this file is the wire format.

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

const k8sApplyFixturePath = "testdata/k8s_apply_specs.json"

// k8sApplyFixture is the checked-in document. Specs are kept as raw JSON so the
// file holds the BYTES the DSD carries rather than this struct's idea of them —
// a field renamed on either side shows up in the file rather than being
// normalized away by a round-trip.
type k8sApplyFixture struct {
	Note  string                `json:"note"`
	Cases []k8sApplyFixtureCase `json:"cases"`
}

type k8sApplyFixtureCase struct {
	Name string          `json:"name"`
	OpID string          `json:"opId"`
	Spec json.RawMessage `json:"spec"`
}

// renderK8sFixtureCases renders the shapes a cluster actually deploys: a
// registry-image app, the same app deployed from git (pinned tag + private
// registry), and a two-service Compose graph. Between them they set every
// field k8sApplyOpSpec has an opinion about.
func renderK8sFixtureCases(t *testing.T) []k8sApplyFixtureCase {
	t.Helper()

	collect := func(name string, ops []dsd.Op, ok bool) []k8sApplyFixtureCase {
		t.Helper()
		if !ok || len(ops) == 0 {
			t.Fatalf("%s: rendered nothing (ok=%v ops=%d)", name, ok, len(ops))
		}
		out := make([]k8sApplyFixtureCase, 0, len(ops))
		for _, op := range ops {
			if op.Kind != dsd.KindK8sApply {
				continue
			}
			// Re-marshal through a generic map so the file's key order is
			// json.Marshal's (struct order) and stable across runs.
			var canon any
			if err := json.Unmarshal(op.Spec, &canon); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			raw, err := json.Marshal(canon)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			out = append(out, k8sApplyFixtureCase{Name: name, OpID: op.ID, Spec: raw})
		}
		return out
	}

	var cases []k8sApplyFixtureCase

	// (1) A registry-image app with a port and a domain: the plain path, no
	// build, no registry credential.
	imageApp := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: json.RawMessage(`{"image":"nginx:1.27","env":{"LOG_LEVEL":"info"},"ports":[{"container":8080}]}`),
	}
	imageOps, imageOK := renderClusterWorkloadOps(imageApp,
		[]store.SecretRefMeta{{Name: "DATABASE_URL", EnvVar: true}, {Name: "TLS_KEY"}},
		[]store.Domain{{Domain: "app.example.com"}},
		store.DeployTarget{}, controlPlane(), "k8s:node:cls_1", "", "")
	cases = append(cases, collect("registry-image-app", imageOps, imageOK)...)

	// (2) The same app deployed from git: a pinned per-deployment tag out of the
	// org registry, so registryHost and deploymentId are both set.
	gitApp := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":3000}]}`),
	}
	gitTarget := store.DeployTarget{
		DeploymentID: "dep_1", SHA: "abc1234567", ImagePin: "pin1",
		Status: "deploying", BuildServerID: "srv_build",
	}
	gitOps, gitOK := renderClusterWorkloadOps(gitApp, nil,
		[]store.Domain{{Domain: "shop.example.com"}},
		gitTarget, controlPlane(), "k8s:node:cls_1", "ghcr.io/acme", "ghcr.io")
	cases = append(cases, collect("git-deployed-app", gitOps, gitOK)...)

	// (3) A Compose graph: one workload per service, the ingress on the
	// source-built web service, a portless worker that gets no Service object.
	composeApp := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: composeSpec(t,
			composeServiceSpec{Name: "db", Image: "postgres:16", Ports: []int{5432}},
			// Portless on purpose: it must render a Deployment and NO Service.
			// A Service with no ports is rejected by the API server, so the e2e
			// needs a real portless workload in the fixture to keep asserting it.
			composeServiceSpec{Name: "worker", Image: "busybox:1.36"},
			composeServiceSpec{Name: "web", Build: ".", Ports: []int{8080},
				DependsOn: []string{"db"}, Env: map[string]string{"OWN": "x"}},
		),
	}
	composeTarget := store.DeployTarget{
		DeploymentID: "dep_2", SHA: "abc1234567", ImagePin: "pin1", Status: "deploying",
		BuildServerID: "srv_build", ServiceStatus: map[string]string{"web": "deploying"},
	}
	composeOps, composeOK := renderClusterWorkloadOps(composeApp, nil,
		[]store.Domain{{Domain: "compose.example.com"}},
		composeTarget, controlPlane(), "k8s:node:cls_1", "ghcr.io/acme", "ghcr.io")
	cases = append(cases, collect("compose-graph", composeOps, composeOK)...)

	return cases
}

// TestK8sApplyFixtureIsUpToDate keeps testdata/k8s_apply_specs.json equal to
// what the reconciler renders today. It is deliberately a staleness check
// rather than a silent regeneration: the file is a wire contract the agent
// module reads, and a change to it has to be looked at.
func TestK8sApplyFixtureIsUpToDate(t *testing.T) {
	want, err := json.MarshalIndent(k8sApplyFixture{
		Note: "Generated by cp/internal/reconciler/k8s_fixture_test.go. " +
			"Read by agent/internal/k8s/cp_spec_fixture_test.go and the real-Kubernetes e2e. " +
			"Regenerate with SIGMAHUB_UPDATE_K8S_FIXTURE=1 go test ./internal/reconciler -run TestK8sApplyFixtureIsUpToDate",
		Cases: renderK8sFixtureCases(t),
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')

	got, readErr := os.ReadFile(k8sApplyFixturePath)
	if readErr == nil && string(got) == string(want) {
		return
	}
	if os.Getenv("SIGMAHUB_UPDATE_K8S_FIXTURE") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(k8sApplyFixturePath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", k8sApplyFixturePath)
		return
	}
	t.Fatalf("%s is stale — the k8s.apply wire format changed.\n"+
		"Regenerate with:\n"+
		"  SIGMAHUB_UPDATE_K8S_FIXTURE=1 go test ./internal/reconciler -run TestK8sApplyFixtureIsUpToDate\n"+
		"and check the diff: the agent decodes this file into its own ApplySpec, so a field\n"+
		"that changes name here is a field the agent silently stops receiving.\n"+
		"--- have (read err: %v) ---\n%s\n--- want ---\n%s",
		k8sApplyFixturePath, readErr, got, want)
}

// TestK8sApplyFixtureCoversTheContract stops the fixture from going vacuous.
// A case list that quietly lost its hosts, ports or registry host would keep
// the agent's side of the guard green while covering nothing.
func TestK8sApplyFixtureCoversTheContract(t *testing.T) {
	var (
		sawHosts, sawPorts, sawEnv, sawSecretRefs    bool
		sawRegistryHost, sawDeploymentID, sawService bool
	)
	for _, c := range renderK8sFixtureCases(t) {
		var spec k8sApplyOpSpec
		if err := json.Unmarshal(c.Spec, &spec); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if spec.Replicas < 1 {
			t.Errorf("%s (%s): replicas = %d — a workload that can never have a pod",
				c.Name, spec.Name, spec.Replicas)
		}
		if spec.Name == "" || spec.Namespace == "" || spec.Image == "" {
			t.Errorf("%s: name/namespace/image = %q/%q/%q", c.Name, spec.Name, spec.Namespace, spec.Image)
		}
		sawHosts = sawHosts || len(spec.Hosts) > 0
		sawPorts = sawPorts || len(spec.Ports) > 0
		sawEnv = sawEnv || len(spec.Env) > 0
		sawSecretRefs = sawSecretRefs || len(spec.SecretRefs) > 0
		sawRegistryHost = sawRegistryHost || spec.RegistryHost != ""
		sawDeploymentID = sawDeploymentID || spec.DeploymentID != ""
		sawService = sawService || spec.Service != ""
	}
	for _, f := range []struct {
		field string
		ok    bool
	}{
		{"hosts", sawHosts}, {"ports", sawPorts}, {"env", sawEnv},
		{"secretRefs", sawSecretRefs}, {"registryHost", sawRegistryHost},
		{"deploymentId", sawDeploymentID}, {"service", sawService},
	} {
		if !f.ok {
			t.Errorf("no fixture case sets %s — the agent-side guard would pass vacuously for it", f.field)
		}
	}
}
