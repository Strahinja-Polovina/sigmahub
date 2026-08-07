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

// TestComposeManualForceOnlyWhileInFlight pins the same rule on the Compose
// renderer, which originally missed the SIGMA-139 fix and kept Force=true for
// every service forever after one Redeploy click (SIGMA-175).
func TestComposeManualForceOnlyWhileInFlight(t *testing.T) {
	spec := appResourceSpec{
		Compose: &composeDeploySpec{Services: []composeServiceSpec{
			{Name: "web", Build: ".", Ports: []int{80}, Rollout: "blue-green"},
		}},
	}
	raw, _ := json.Marshal(spec)
	rs := store.ResourceSpec{ResourceID: "res_c", ProjectID: "proj_c", Kind: "app", Spec: raw}

	buildForce := func(status string) bool {
		target := store.DeployTarget{
			DeploymentID: "dep_1", ResourceID: "res_c", ProjectID: "proj_c", Provider: "github",
			RepoFullName: "acme/app", Ref: "refs/heads/main", SHA: "abcdef1234", ConfigHash: "cfg",
			Trigger: "manual", Status: status,
		}
		ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil, target)
		if !ok {
			t.Fatal("render should succeed")
		}
		for _, op := range ops {
			if op.ID == "build:res_c:web" {
				var b struct {
					Force bool `json:"force"`
				}
				_ = json.Unmarshal(op.Spec, &b)
				return b.Force
			}
		}
		t.Fatal("no compose build op rendered")
		return false
	}

	if !buildForce("deploying") {
		t.Error("a manual Compose deploy in flight must force a rebuild")
	}
	if buildForce("success") {
		t.Error("a succeeded manual Compose deploy must NOT keep forcing rebuilds (SIGMA-175)")
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

// TestRenderDeployPinnedImageAndVolumes pins SIGMA-173 + SIGMA-169 on the
// single-container path: a building deploy tags under its own pin (no tag is
// ever rebuilt in place), and declared volumes are ensured + mounted with the
// rollout classed as recreate (a blue-green overlap would mount the same named
// volume into two live generations).
func TestRenderDeployPinnedImageAndVolumes(t *testing.T) {
	var spec appResourceSpec
	if err := json.Unmarshal(json.RawMessage(`{"volumes":[{"name":"data","mountPath":"/var/lib/data"}]}`), &spec); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(spec)
	rs := store.ResourceSpec{ResourceID: "res_v", ProjectID: "proj_v", Kind: "app", Spec: raw}
	target := store.DeployTarget{
		DeploymentID: "dep_10", ResourceID: "res_v", ProjectID: "proj_v", Provider: "github",
		RepoFullName: "acme/app", SHA: "abcdef1234", ConfigHash: "cfg", Trigger: "git",
		ImagePin: "dep10p",
	}
	ops, networkID, ok := renderDeployOps(rs, nil, nil, target)
	if !ok {
		t.Fatal("render should succeed")
	}
	byID := map[string]dsd.Op{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	vol, exists := byID["vol:res_v:data"]
	if !exists || vol.Kind != dsd.KindVolumeEnsure {
		t.Fatalf("declared volume must render a volume.ensure (SIGMA-169); ops=%v", opIDs(ops))
	}
	rollout, exists := byID["res:res_v"]
	if !exists {
		t.Fatalf("rollout missing; ops=%v", opIDs(ops))
	}
	if rollout.Kind != dsd.KindDeployRecreate {
		t.Fatalf("a volume-holding app must deploy via recreate, got %q", rollout.Kind)
	}
	if !dependsOn(rollout, "vol:res_v:data") || !dependsOn(rollout, networkID) || !dependsOn(rollout, "build:res_v") {
		t.Fatalf("rollout deps wrong: %v", rollout.DependsOn)
	}
	var rspec struct {
		Container struct {
			Image   string `json:"image"`
			Volumes []struct {
				Name      string `json:"name"`
				MountPath string `json:"mountPath"`
			} `json:"volumes"`
		} `json:"container"`
	}
	_ = json.Unmarshal(rollout.Spec, &rspec)
	if want := dsd.PinnedImageTag("res_v", "abcdef1234", "dep10p"); rspec.Container.Image != want {
		t.Fatalf("image = %q, want pinned %q (SIGMA-173)", rspec.Container.Image, want)
	}
	if len(rspec.Container.Volumes) != 1 || rspec.Container.Volumes[0].Name != dsd.VolumeName("res_v", "data") ||
		rspec.Container.Volumes[0].MountPath != "/var/lib/data" {
		t.Fatalf("volume mounts wrong: %+v (SIGMA-169)", rspec.Container.Volumes)
	}
	var b struct {
		ImageTag string `json:"imageTag"`
	}
	_ = json.Unmarshal(byID["build:res_v"].Spec, &b)
	if want := dsd.PinnedImageTag("res_v", "abcdef1234", "dep10p"); b.ImageTag != want {
		t.Fatalf("build tag = %q, want pinned %q", b.ImageTag, want)
	}
}

// TestRenderDeployReshipsRetainedImage pins SIGMA-173/166: a rollback — and a
// config deploy — renders rollout-only, shipping the SOURCE release's pinned
// tag, while a config row without any pinned source falls back to the full
// pipeline. The generation must follow the NEW deployment id, or the swap
// would collide with the standing generation (the SIGMA-166 wedge).
func TestRenderDeployReshipsRetainedImage(t *testing.T) {
	rs := store.ResourceSpec{ResourceID: "res_r", ProjectID: "proj_r", Kind: "app", Spec: json.RawMessage(`{}`)}
	base := store.DeployTarget{
		ResourceID: "res_r", ProjectID: "proj_r", Provider: "github",
		RepoFullName: "acme/app", SHA: "abcdef1234", ConfigHash: "cfg",
	}

	generation := func(op dsd.Op) string {
		var s struct {
			Generation string `json:"generation"`
			Container  struct {
				Image string `json:"image"`
			} `json:"container"`
		}
		_ = json.Unmarshal(op.Spec, &s)
		return s.Generation
	}

	for _, trigger := range []string{"rollback", "config"} {
		target := base
		target.DeploymentID, target.Trigger, target.ImagePin = "dep_20", trigger, "src111"
		target.ImageDigest = dsd.PinnedImageTag("res_r", "abcdef1234", "src111")
		ops, _, ok := renderDeployOps(rs, nil, nil, target)
		if !ok {
			t.Fatalf("%s render should succeed", trigger)
		}
		if len(ops) != 1 || ops[0].ID != "res:res_r" {
			t.Fatalf("%s must render rollout-only, got %v", trigger, opIDs(ops))
		}
		var s struct {
			Container struct {
				Image string `json:"image"`
			} `json:"container"`
		}
		_ = json.Unmarshal(ops[0].Spec, &s)
		if want := dsd.PinnedImageTag("res_r", "abcdef1234", "src111"); s.Container.Image != want {
			t.Fatalf("%s image = %q, want the source release's pinned %q", trigger, s.Container.Image, want)
		}
	}

	// The config deploy's generation follows its own id, not the standing one.
	standing, cfgTarget := base, base
	standing.DeploymentID, standing.Trigger, standing.ImagePin = "dep_1", "git", "p1"
	cfgTarget.DeploymentID, cfgTarget.Trigger, cfgTarget.ImagePin = "dep_2", "config", "p1"
	cfgTarget.ImageDigest = dsd.PinnedImageTag("res_r", "abcdef1234", "p1")
	opsA, _, _ := renderDeployOps(rs, nil, nil, standing)
	opsB, _, _ := renderDeployOps(rs, nil, nil, cfgTarget)
	genA, genB := "", ""
	for _, op := range opsA {
		if op.ID == "res:res_r" {
			genA = generation(op)
		}
	}
	genB = generation(opsB[0])
	if genA == "" || genA == genB {
		t.Fatalf("config generation must differ from the standing one (SIGMA-166): %q vs %q", genA, genB)
	}

	// A config row with no pinned source (legacy) rebuilds the full pipeline.
	legacy := base
	legacy.DeploymentID, legacy.Trigger = "dep_30", "config"
	ops, _, ok := renderDeployOps(rs, nil, nil, legacy)
	if !ok {
		t.Fatal("legacy config render should succeed")
	}
	ids := map[string]bool{}
	for _, op := range ops {
		ids[op.ID] = true
	}
	if !ids["clone:res_r"] || !ids["build:res_r"] {
		t.Fatalf("a pinless config deploy must fall back to clone+build, got %v", opIDs(ops))
	}
}

// TestRenderComposeRollbackReshipsWithoutGit pins SIGMA-168: a Compose rollback
// with a pinned source renders NO clone and NO builds — the retained per-service
// images ship as-is, so rollback no longer depends on a live git credential and
// a still-reachable commit. Prebuilt-image services keep their pull op.
func TestRenderComposeRollbackReshipsWithoutGit(t *testing.T) {
	spec := appResourceSpec{
		Compose: &composeDeploySpec{Services: []composeServiceSpec{
			{Name: "web", Build: ".", Ports: []int{80}, Rollout: "blue-green"},
			{Name: "db", Image: "postgres:16", Ports: []int{5432}, Rollout: "recreate"},
		}},
	}
	raw, _ := json.Marshal(spec)
	rs := store.ResourceSpec{ResourceID: "res_cr", ProjectID: "proj_cr", Kind: "app", Spec: raw}
	target := store.DeployTarget{
		DeploymentID: "dep_40", ResourceID: "res_cr", ProjectID: "proj_cr", Provider: "github",
		RepoFullName: "acme/app", SHA: "abcdef1234", ConfigHash: "cfg",
		Trigger: "rollback", ImagePin: "src222",
	}
	ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil, target)
	if !ok {
		t.Fatal("render should succeed")
	}
	byID := map[string]dsd.Op{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	if _, exists := byID["clone:res_cr"]; exists {
		t.Fatalf("a pinned Compose rollback must not clone (SIGMA-168); ops=%v", opIDs(ops))
	}
	if _, exists := byID["build:res_cr:web"]; exists {
		t.Fatalf("a pinned Compose rollback must not build; ops=%v", opIDs(ops))
	}
	if _, exists := byID["pull:res_cr:db"]; !exists {
		t.Fatalf("prebuilt service keeps its pull; ops=%v", opIDs(ops))
	}
	web, exists := byID["res:res_cr:web"]
	if !exists {
		t.Fatalf("web rollout missing; ops=%v", opIDs(ops))
	}
	var s struct {
		Container struct {
			Image string `json:"image"`
		} `json:"container"`
	}
	_ = json.Unmarshal(web.Spec, &s)
	if want := dsd.PinnedServiceImageTag("res_cr", "web", "abcdef1234", "src222"); s.Container.Image != want {
		t.Fatalf("web image = %q, want the source release's pinned %q", s.Container.Image, want)
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
