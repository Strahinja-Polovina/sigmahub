package reconciler

import (
	"encoding/json"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestRenderComposeDeployOps pins the multi-service render: one shared clone, a
// build per build-context service and an image.pull per prebuilt service, a
// rollout (blue-green) for stateless services and a recreate for stateful ones,
// service-name network aliases, and depends_on → op ordering.
func TestRenderComposeDeployOps(t *testing.T) {
	spec := appResourceSpec{
		Env: map[string]string{"FOO": "bar"},
		Compose: &composeDeploySpec{Services: []composeServiceSpec{
			{Name: "web", Build: ".", Ports: []int{80}, Rollout: "blue-green", DependsOn: []string{"db"}},
			{Name: "db", Image: "postgres:16", Ports: []int{5432}, Rollout: "recreate"},
		}},
	}
	raw, _ := json.Marshal(spec)
	rs := store.ResourceSpec{ResourceID: "res_a", ProjectID: "proj_a", Kind: "app", Spec: raw}
	target := store.DeployTarget{
		DeploymentID: "dep_1", ResourceID: "res_a", ProjectID: "proj_a", Provider: "github",
		RepoFullName: "acme/app", Ref: "refs/heads/main", SHA: "abcdef1234", ConfigHash: "cfg", Trigger: "git",
	}

	ops, networkID, ok := renderComposeDeployOps(rs, spec, nil, nil, target)
	if !ok {
		t.Fatal("render should succeed")
	}
	byID := map[string]dsd.Op{}
	for _, op := range ops {
		byID[op.ID] = op
	}

	// One shared clone.
	if _, ok := byID["clone:res_a"]; !ok {
		t.Fatalf("missing shared clone; ops=%v", opIDs(ops))
	}
	// web builds from source; db pulls a prebuilt image.
	build, ok := byID["build:res_a:web"]
	if !ok || build.Kind != dsd.KindImageBuild {
		t.Fatalf("web should have a build op; ops=%v", opIDs(ops))
	}
	if _, ok := byID["pull:res_a:db"]; !ok {
		t.Fatalf("db should have an image.pull op; ops=%v", opIDs(ops))
	}
	if _, ok := byID["build:res_a:db"]; ok {
		t.Fatal("a prebuilt-image service must not have a build op")
	}

	// web → rollout (blue-green); db → recreate.
	web, ok := byID["res:res_a:web"]
	if !ok || web.Kind != dsd.KindDeployRollout {
		t.Fatalf("web should be a rollout; got %+v", web)
	}
	db, ok := byID["res:res_a:db"]
	if !ok || db.Kind != dsd.KindDeployRecreate {
		t.Fatalf("db should be a recreate; got %+v", db)
	}

	// web depends on its build, the network, AND db's rollout (depends_on).
	if !dependsOn(web, "build:res_a:web") || !dependsOn(web, networkID) || !dependsOn(web, "res:res_a:db") {
		t.Fatalf("web rollout deps wrong: %v", web.DependsOn)
	}

	// The web container carries its service name as a network alias + service tag.
	var cs struct {
		Container struct {
			Service        string   `json:"service"`
			NetworkAliases []string `json:"networkAliases"`
			Image          string   `json:"image"`
		} `json:"container"`
	}
	_ = json.Unmarshal(web.Spec, &cs)
	if cs.Container.Service != "web" || len(cs.Container.NetworkAliases) != 1 || cs.Container.NetworkAliases[0] != "web" {
		t.Fatalf("web container service/alias wrong: %+v", cs.Container)
	}
	if cs.Container.Image != dsd.DeployServiceImageTag("res_a", "web", "abcdef1234") {
		t.Fatalf("web image = %q", cs.Container.Image)
	}

	// Compose apps deploy onto a per-resource network (bare service aliases can't
	// collide across apps in the same project).
	if networkID != "net:res:res_a" {
		t.Fatalf("compose network id = %q, want per-resource", networkID)
	}
}

// TestRenderComposePortlessAndInvalid pins two edge rules: a portless worker gets
// a "none" health probe (running = ready, no bogus port-80 gate), and a service
// with neither build nor image is filtered out (matching composeServiceCount).
func TestRenderComposePortlessAndInvalid(t *testing.T) {
	spec := appResourceSpec{
		Compose: &composeDeploySpec{Services: []composeServiceSpec{
			{Name: "worker", Build: "."}, // no ports
			{Name: "ghost"},              // neither build nor image → filtered
		}},
	}
	raw, _ := json.Marshal(spec)
	rs := store.ResourceSpec{ResourceID: "res_p", ProjectID: "proj_p", Kind: "app", Spec: raw}
	target := store.DeployTarget{DeploymentID: "dep_2", Provider: "github", RepoFullName: "acme/w", SHA: "beef1234", Trigger: "git"}

	ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil, target)
	if !ok {
		t.Fatal("render should succeed")
	}
	byID := map[string]dsd.Op{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	if _, exists := byID["res:res_p:ghost"]; exists {
		t.Fatal("a service with neither build nor image must be filtered out")
	}
	worker, exists := byID["res:res_p:worker"]
	if !exists {
		t.Fatalf("worker rollout missing; ops=%v", opIDs(ops))
	}
	var ws struct {
		Health struct {
			Type string `json:"type"`
		} `json:"health"`
	}
	_ = json.Unmarshal(worker.Spec, &ws)
	if ws.Health.Type != "none" {
		t.Fatalf("portless worker health = %q, want none", ws.Health.Type)
	}
}

// TestManualForceOnlyWhileInFlight pins SIGMA-139: a manual deploy forces a
// rebuild only while in flight; once it has succeeded the lingering deploy
// target must not keep forcing a docker rebuild on every unrelated DSD version
// bump.
func TestManualForceOnlyWhileInFlight(t *testing.T) {
	spec := appResourceSpec{Env: map[string]string{"FOO": "bar"}}
	raw, _ := json.Marshal(spec)
	rs := store.ResourceSpec{ResourceID: "res_a", ProjectID: "proj_a", Kind: "app", Spec: raw}

	buildForce := func(status string) bool {
		target := store.DeployTarget{
			DeploymentID: "dep_1", ResourceID: "res_a", ProjectID: "proj_a", Provider: "github",
			RepoFullName: "acme/app", Ref: "refs/heads/main", SHA: "abcdef1234", ConfigHash: "cfg",
			Trigger: "manual", Status: status,
		}
		ops, _, ok := renderDeployOps(rs, nil, nil, target)
		if !ok {
			t.Fatal("render should succeed")
		}
		for _, op := range ops {
			if op.ID == "build:res_a" {
				var b struct {
					Force bool `json:"force"`
				}
				_ = json.Unmarshal(op.Spec, &b)
				return b.Force
			}
		}
		t.Fatal("no build op rendered")
		return false
	}

	if !buildForce("deploying") {
		t.Error("a manual deploy in flight must force a rebuild")
	}
	if buildForce("success") {
		t.Error("a succeeded manual deploy must NOT keep forcing a rebuild (SIGMA-139)")
	}
}

// TestDeployHealthNeverProbesUnknownPort pins SIGMA-160: with no declared port
// the probe must be "none" (gate on running), never a TCP probe on port 0 —
// the agent rewrites 0 to 80, so that would fail the gate for every app that
// listens anywhere else, which is the normal case.
func TestDeployHealthNeverProbesUnknownPort(t *testing.T) {
	// No ports, no healthCheck → nothing to probe.
	if got := deployHealth(appResourceSpec{}, json.RawMessage(`{}`)); got.Type != "none" {
		t.Fatalf("portless probe = %+v, want type none", got)
	}
	// A declared port is probed over TCP as before.
	spec := appResourceSpec{}
	spec.Ports = append(spec.Ports, struct {
		Container int    `json:"container"`
		Host      int    `json:"host"`
		Protocol  string `json:"protocol"`
	}{Container: 3000})
	got := deployHealth(spec, json.RawMessage(`{}`))
	if got.Type != "tcp" || got.Port != 3000 {
		t.Fatalf("declared port probe = %+v, want tcp/3000", got)
	}
	// A detected http health check still wins.
	got = deployHealth(spec, json.RawMessage(`{"healthCheck":{"type":"http","path":"/healthz","port":8080}}`))
	if got.Type != "http" || got.Path != "/healthz" || got.Port != 8080 {
		t.Fatalf("http probe = %+v", got)
	}
}

func opIDs(ops []dsd.Op) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = op.ID
	}
	return out
}

func dependsOn(op dsd.Op, dep string) bool {
	for _, d := range op.DependsOn {
		if d == dep {
			return true
		}
	}
	return false
}
