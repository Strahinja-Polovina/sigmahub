package reconciler

import (
	"encoding/json"
	"strings"
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

	ops, networkID, ok := renderComposeDeployOps(rs, spec, nil, nil, target, "")
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

	ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil, target, "")
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
		ops, _, ok := renderDeployOps(rs, nil, nil, target, "", registryRender{})
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
		ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil, target, "")
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
	ops, networkID, ok := renderDeployOps(rs, nil, nil, target, "", registryRender{})
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
		ops, _, ok := renderDeployOps(rs, nil, nil, target, "", registryRender{})
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
	opsA, _, _ := renderDeployOps(rs, nil, nil, standing, "", registryRender{})
	opsB, _, _ := renderDeployOps(rs, nil, nil, cfgTarget, "", registryRender{})
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
	ops, _, ok := renderDeployOps(rs, nil, nil, legacy, "", registryRender{})
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
	ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil, target, "")
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

// TestCrossHostReshipPullsFromRegistry pins SIGMA-356: a rollback / config deploy
// that re-ships a retained image on a dedicated-build-server topology must still
// emit an authenticated image.pull on the DEPLOY TARGET — the image lives in the
// private org registry and the target cannot read the build host's local daemon.
// The build server, which has no build to do on a re-ship, must render nothing.
func TestCrossHostReshipPullsFromRegistry(t *testing.T) {
	rs := store.ResourceSpec{
		ResourceID: "res_rr", ProjectID: "proj_rr", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}
	reg := registryRender{repository: "ghcr.io/acme", host: "ghcr.io"}
	for _, trigger := range []string{"rollback", "config"} {
		target := store.DeployTarget{
			DeploymentID: "dep_50", ResourceID: "res_rr", ProjectID: "proj_rr", Provider: "github",
			RepoFullName: "acme/app", SHA: "abcdef1234", ConfigHash: "cfg",
			Trigger: trigger, ImagePin: "src999", ImageDigest: dsd.PinnedImageTag("res_rr", "abcdef1234", "src999"),
			ServerID: "srv_run", BuildServerID: "srv_build", Status: "deploying",
		}

		// The deploy target: pull + rollout, and no clone/build (it re-ships).
		ops, _, ok := renderDeployOps(rs, nil, nil, target, "srv_run", reg)
		if !ok {
			t.Fatalf("%s: the deploy target must render", trigger)
		}
		byID := map[string]dsd.Op{}
		for _, op := range ops {
			byID[op.ID] = op
		}
		if _, built := byID["build:res_rr"]; built {
			t.Fatalf("%s: a re-ship must not build; ops=%v", trigger, opIDs(ops))
		}
		pull, found := byID["pull:res_rr"]
		if !found || pull.Kind != dsd.KindImagePull {
			t.Fatalf("%s: the deploy target must pull the retained image (SIGMA-356); ops=%v", trigger, opIDs(ops))
		}
		var ps imagePullOpSpec
		_ = json.Unmarshal(pull.Spec, &ps)
		if ps.RegistryHost != "ghcr.io" {
			t.Fatalf("%s: the pull goes out anonymous and a private registry 401s it: %+v", trigger, ps)
		}
		if !strings.HasPrefix(ps.Image, "ghcr.io/acme/") {
			t.Fatalf("%s: cross-host pull tag %q would resolve to docker.io", trigger, ps.Image)
		}
		rollout, ok := byID["res:res_rr"]
		if !ok || !dependsOn(rollout, "pull:res_rr") {
			t.Fatalf("%s: the rollout must depend on the pull: %+v", trigger, rollout.DependsOn)
		}

		// The build server has no build to do on a re-ship, so it renders nothing —
		// a stray rollout here would place the app container on the wrong host.
		if _, _, ok := renderDeployOps(rs, nil, nil, target, "srv_build", reg); ok {
			t.Fatalf("%s: the build server must render nothing on a re-ship", trigger)
		}
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

// composeMultiServer is a two-tier app: the API on the app server, Postgres on
// a dedicated database server, with the API depending on the database.
func composeMultiServer() (store.ResourceSpec, appResourceSpec) {
	spec := appResourceSpec{
		Env: map[string]string{"LOG_LEVEL": "info"},
		Compose: &composeDeploySpec{Services: []composeServiceSpec{
			{
				Name: "api", Build: ".", Ports: []int{8080},
				DependsOn: []string{"db"},
				ServerID:  "srv_app",
				Env:       map[string]string{"DB_HOST": "10.8.0.4", "LOG_LEVEL": "debug"},
			},
			{
				Name: "db", Image: "postgres:16", Ports: []int{5432},
				Rollout: "recreate", ServerID: "srv_db",
			},
		}},
	}
	raw, _ := json.Marshal(spec)
	return store.ResourceSpec{ResourceID: "res_m", ProjectID: "proj_m", Kind: "app", Spec: raw}, spec
}

func multiServerTarget(serviceStatus map[string]string) store.DeployTarget {
	return store.DeployTarget{
		DeploymentID: "dep_m", ResourceID: "res_m", ProjectID: "proj_m", Provider: "github",
		RepoFullName: "acme/app", Ref: "refs/heads/main", SHA: "beef01", ConfigHash: "cfg",
		Trigger: "git", ServerID: "srv_app", ServiceStatus: serviceStatus,
	}
}

// Each server's document renders only the services placed on it — a Compose app
// can span hosts instead of being pinned to one.
func TestComposePlacementSplitsServicesAcrossServers(t *testing.T) {
	rs, spec := composeMultiServer()

	dbOps, _, ok := renderComposeDeployOps(rs, spec, nil, nil, multiServerTarget(nil), "srv_db")
	if !ok {
		t.Fatal("the database server must render its placed service")
	}
	for _, op := range dbOps {
		if op.ID == "res:res_m:api" {
			t.Fatal("the api service is placed on srv_app; srv_db must not render it")
		}
	}
	if _, found := opByID(dbOps, "res:res_m:db"); !found {
		t.Fatalf("db rollout missing from the database server: %+v", opIDs(dbOps))
	}

	// The app server has nothing to render yet: its only service waits on a
	// database that has not reported success.
	if _, _, ok := renderComposeDeployOps(rs, spec, nil, nil, multiServerTarget(nil), "srv_app"); ok {
		t.Fatal("api must be gated until the remote db reports success")
	}
}

// The cross-server gate opens only once the dependency reports success — this is
// what stops an app from starting against a database that isn't up.
func TestComposeCrossServerDependencyGate(t *testing.T) {
	rs, spec := composeMultiServer()

	for _, state := range []string{"", "deploying", "failed"} {
		status := map[string]string{}
		if state != "" {
			status["db"] = state
		}
		if _, _, ok := renderComposeDeployOps(rs, spec, nil, nil, multiServerTarget(status), "srv_app"); ok {
			t.Fatalf("db=%q must keep the api gated", state)
		}
	}

	ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil,
		multiServerTarget(map[string]string{"db": "success"}), "srv_app")
	if !ok {
		t.Fatal("a successful db must release the api")
	}
	api, found := opByID(ops, "res:res_m:api")
	if !found {
		t.Fatalf("api rollout missing: %+v", opIDs(ops))
	}
	// The dependency lives in another document, so it must NOT appear as an op
	// dependency — a dangling reference would wedge the whole apply.
	for _, d := range api.DependsOn {
		if d == "res:res_m:db" {
			t.Fatal("a remotely-placed dependency must not become a local op dependency")
		}
	}
}

// Same-server dependencies keep using op ordering, unchanged.
func TestComposeSameServerDependencyStaysOpOrdered(t *testing.T) {
	spec := appResourceSpec{
		Compose: &composeDeploySpec{Services: []composeServiceSpec{
			{Name: "api", Build: ".", Ports: []int{8080}, DependsOn: []string{"cache"}, ServerID: "srv_1"},
			{Name: "cache", Image: "redis:7", Ports: []int{6379}, ServerID: "srv_1"},
		}},
	}
	raw, _ := json.Marshal(spec)
	rs := store.ResourceSpec{ResourceID: "res_s", ProjectID: "p", Kind: "app", Spec: raw}
	target := store.DeployTarget{
		DeploymentID: "dep_s", ResourceID: "res_s", SHA: "aa", Trigger: "git", ServerID: "srv_1",
	}

	// No service status at all: co-located services are ordered by the graph, so
	// they must NOT be gated on a status report that only exists cross-server.
	ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil, target, "srv_1")
	if !ok {
		t.Fatal("co-located services must render together")
	}
	api, _ := opByID(ops, "res:res_s:api")
	if !dependsOn(api, "res:res_s:cache") {
		t.Fatalf("api must depend on cache in-document: %+v", api.DependsOn)
	}
}

// Per-service env is layered over the resource env, so services on different
// hosts can carry different values for the same key.
func TestComposePerServiceEnvOverridesResourceEnv(t *testing.T) {
	rs, spec := composeMultiServer()
	ops, _, ok := renderComposeDeployOps(rs, spec, nil, nil,
		multiServerTarget(map[string]string{"db": "success"}), "srv_app")
	if !ok {
		t.Fatal("render should succeed")
	}
	apiOp, _ := opByID(ops, "res:res_m:api")
	var rollout rolloutOpSpec
	if err := json.Unmarshal(apiOp.Spec, &rollout); err != nil {
		t.Fatal(err)
	}
	if got := rollout.Container.Env["DB_HOST"]; got != "10.8.0.4" {
		t.Fatalf("service env not applied: DB_HOST = %q", got)
	}
	if got := rollout.Container.Env["LOG_LEVEL"]; got != "debug" {
		t.Fatalf("service env must win over resource env: LOG_LEVEL = %q", got)
	}

	// The resource-level map must not be mutated — the db service on the other
	// server still sees the original value.
	if spec.Env["LOG_LEVEL"] != "info" {
		t.Fatalf("resource env was mutated: %+v", spec.Env)
	}
}

// SIGMA-230. The agent's GC keep-set comes from the document it is about to
// apply, and GC runs BEFORE the ops — so a resource whose rollout the renderer
// deliberately holds back must still be named as something to KEEP, or the live
// container is reaped and the app is down for the whole hold. Both hold-back
// paths fall through to the `sync:<id>` stub, which must carry the retain list.
func TestHeldBackRenderStillNamesContainersToRetain(t *testing.T) {
	retainOf := func(t *testing.T, ops []dsd.Op, resourceID string) []string {
		t.Helper()
		stub, found := opByID(ops, "sync:"+resourceID)
		if !found {
			t.Fatalf("expected the resource.sync stub for %s; ops=%v", resourceID, opIDs(ops))
		}
		var spec struct {
			Retain []string `json:"retain"`
		}
		if err := json.Unmarshal(stub.Spec, &spec); err != nil {
			t.Fatal(err)
		}
		return spec.Retain
	}
	render := func(serverID string, rs store.ResourceSpec, target store.DeployTarget, reg registryRender) []dsd.Op {
		ops, _ := renderOps(serverID, []store.ResourceSpec{rs}, nil, nil, store.HostHardening{}, nil,
			map[string]store.DeployTarget{rs.ResourceID: target}, nil, nil, nil, nil, nil,
			ACMEConfig{}, clusterRender{}, reg, "")
		return ops
	}

	// A dedicated build server holds the deploy target's rollout while the
	// deployment is queued/building.
	single := store.ResourceSpec{
		ResourceID: testResource, ProjectID: testProject, Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}
	reg := registryRender{repository: "ghcr.io/acme", host: "ghcr.io"}
	for _, status := range []string{"queued", "building"} {
		target := store.DeployTarget{
			DeploymentID: "dep_1", SHA: "abc1234567", ImagePin: "pin1", Status: status,
			ServerID: "srv_web", BuildServerID: "srv_build",
		}
		if _, _, ok := renderDeployOps(single, nil, nil, target, "srv_web", reg); ok {
			t.Fatalf("precondition: %s must hold the deploy target's rollout", status)
		}
		got := retainOf(t, render("srv_web", single, target, reg), single.ResourceID)
		if len(got) != 1 || got[0] != "" {
			t.Fatalf("%s: deploy target must retain its live container group, got %#v", status, got)
		}
		// The build server runs no container of this resource — it renders the
		// clone + build and no stub at all — so it claims nothing to retain.
		// Retaining there would pin a stale container on the wrong host.
		if stub, found := opByID(render("srv_build", single, target, reg), "sync:"+single.ResourceID); found {
			t.Fatalf("the build server must not retain this resource, got stub %s", stub.Spec)
		}
		if got := retainedContainerGroups(single, target, "srv_build"); len(got) != 0 {
			t.Fatalf("the build server must retain nothing, got %#v", got)
		}
	}

	// A cross-server Compose render where every locally-placed service is gated
	// on a remote dependency: the gated service is still ours to keep running.
	compose, spec := composeMultiServer()
	target := multiServerTarget(nil)
	if _, _, ok := renderComposeDeployOps(compose, spec, nil, nil, target, "srv_app"); ok {
		t.Fatal("precondition: api must be gated until the remote db reports success")
	}
	got := retainOf(t, render("srv_app", compose, target, registryRender{}), compose.ResourceID)
	if len(got) != 1 || got[0] != "api" {
		t.Fatalf("the gated app server must retain its own service, got %#v", got)
	}
}
