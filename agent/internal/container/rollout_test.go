package container

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// fakeRollout is an in-memory rolloutDocker that records the ordered sequence of
// lifecycle calls, so a test can assert the never-cut invariant.
type fakeRollout struct {
	events     []string
	byName     map[string]*ContainerState
	nextID     int
	startIP    string // IP assigned to a container on start ("" = none, stays unhealthy)
	failCreate bool
}

func newFakeRollout() *fakeRollout {
	return &fakeRollout{byName: map[string]*ContainerState{}, startIP: "10.0.0.9"}
}

func (f *fakeRollout) seed(name, resourceID, hash string, running bool) {
	f.nextID++
	f.byName[name] = &ContainerState{
		ID: fmt.Sprintf("id%d", f.nextID), Name: name, Running: running, IP: "10.0.0.1",
		Labels: map[string]string{LabelManaged: "true", LabelResourceID: resourceID, LabelSpecHash: hash},
	}
}

// seedSvc seeds a container tagged with a Compose service label.
func (f *fakeRollout) seedSvc(name, resourceID, service, hash string, running bool) {
	f.nextID++
	f.byName[name] = &ContainerState{
		ID: fmt.Sprintf("id%d", f.nextID), Name: name, Running: running, IP: "10.0.0.1",
		Labels: map[string]string{LabelManaged: "true", LabelResourceID: resourceID, LabelService: service, LabelSpecHash: hash},
	}
}

func (f *fakeRollout) ContainerList(context.Context) ([]ContainerState, error) {
	out := []ContainerState{}
	for _, c := range f.byName {
		out = append(out, *c)
	}
	return out, nil
}
func (f *fakeRollout) ContainerInspect(_ context.Context, name string) (ContainerState, bool, error) {
	c, ok := f.byName[name]
	if !ok {
		return ContainerState{}, false, nil
	}
	return *c, true, nil
}
func (f *fakeRollout) ContainerCreate(_ context.Context, name string, body any) (string, error) {
	if f.failCreate {
		return "", fmt.Errorf("create refused")
	}
	f.nextID++
	id := fmt.Sprintf("id%d", f.nextID)
	// The create body carries the labels; extract managed labels for realism.
	labels := map[string]string{}
	if m, ok := body.(map[string]any); ok {
		if l, ok := m["Labels"].(map[string]string); ok {
			labels = l
		}
	}
	f.byName[name] = &ContainerState{ID: id, Name: name, Running: false, Labels: labels}
	f.events = append(f.events, "create:"+name)
	return id, nil
}
func (f *fakeRollout) ContainerStart(_ context.Context, id string) error {
	for _, c := range f.byName {
		if c.ID == id {
			c.Running = true
			c.IP = f.startIP
			f.events = append(f.events, "start:"+c.Name)
			return nil
		}
	}
	return fmt.Errorf("no such container %s", id)
}
func (f *fakeRollout) ContainerStop(_ context.Context, id string, _ time.Duration) error {
	for _, c := range f.byName {
		if c.ID == id {
			f.events = append(f.events, "stop:"+c.Name)
			return nil
		}
	}
	return nil
}
func (f *fakeRollout) ContainerRemove(_ context.Context, id string, _ bool) error {
	for name, c := range f.byName {
		if c.ID == id {
			f.events = append(f.events, "remove:"+name)
			delete(f.byName, name)
			return nil
		}
	}
	return nil
}

func healthyProbe(context.Context, string, HealthProbe) error { return nil }
func unhealthyProbe(context.Context, string, HealthProbe) error {
	return fmt.Errorf("connection refused")
}

func noLog(string, ...any) {}

func rolloutSpec(res, gen, hash string) (RolloutSpec, any) {
	spec := RolloutSpec{
		Container:  ContainerSpec{ResourceID: res, Name: "sigmahub-" + res, Image: "img"},
		Generation: gen,
		Health:     HealthProbe{Type: "http", Port: 8080, Path: "/", IntervalSec: 1, TimeoutSec: 1},
	}
	body := map[string]any{"Labels": map[string]string{LabelManaged: "true", LabelResourceID: res, LabelSpecHash: hash}}
	return spec, body
}

// TestRolloutNeverCuts proves the new container is created, started, and healthy
// BEFORE the old is stopped/removed.
func TestRolloutNeverCuts(t *testing.T) {
	f := newFakeRollout()
	f.seed("sigmahub-res_a", "res_a", "oldhash", true) // the currently-serving old gen

	spec, body := rolloutSpec("res_a", "gen2", "newhash")
	if err := performRollout(context.Background(), f, healthyProbe, spec, body, "newhash", nil, noLog); err != nil {
		t.Fatal(err)
	}

	seq := strings.Join(f.events, ",")
	createIdx := indexOf(f.events, "create:sigmahub-res_a-gen2")
	startIdx := indexOf(f.events, "start:sigmahub-res_a-gen2")
	removeOldIdx := indexOf(f.events, "remove:sigmahub-res_a")
	if createIdx < 0 || startIdx < 0 || removeOldIdx < 0 {
		t.Fatalf("missing lifecycle events: %s", seq)
	}
	if !(createIdx < startIdx && startIdx < removeOldIdx) {
		t.Fatalf("never-cut violated: new must be created+started before old removed: %s", seq)
	}
	// Old is gone; new remains.
	if _, ok := f.byName["sigmahub-res_a"]; ok {
		t.Error("old container should be drained")
	}
	if _, ok := f.byName["sigmahub-res_a-gen2"]; !ok {
		t.Error("new container should remain")
	}
}

// TestRolloutHealthFailKeepsOld proves a failing health gate removes ONLY the new
// container and leaves the old one serving.
func TestRolloutHealthFailKeepsOld(t *testing.T) {
	f := newFakeRollout()
	f.seed("sigmahub-res_a", "res_a", "oldhash", true)

	spec, body := rolloutSpec("res_a", "gen2", "newhash")
	err := performRollout(context.Background(), f, unhealthyProbe, spec, body, "newhash", nil, noLog)
	if err == nil {
		t.Fatal("expected a health-gate failure")
	}
	if _, ok := f.byName["sigmahub-res_a"]; !ok {
		t.Error("old container must keep serving after a failed rollout")
	}
	if _, ok := f.byName["sigmahub-res_a-gen2"]; ok {
		t.Error("the unhealthy new container must be removed")
	}
	for _, e := range f.events {
		if e == "stop:sigmahub-res_a" || e == "remove:sigmahub-res_a" {
			t.Fatalf("old container must never be touched on a failed rollout: %v", f.events)
		}
	}
}

// TestRolloutIdempotent proves a re-apply of an already-running generation drains
// stragglers but does not recreate the current container.
func TestRolloutIdempotent(t *testing.T) {
	f := newFakeRollout()
	f.seed("sigmahub-res_a-gen2", "res_a", "newhash", true) // already the current gen

	spec, body := rolloutSpec("res_a", "gen2", "newhash")
	if err := performRollout(context.Background(), f, healthyProbe, spec, body, "newhash", nil, noLog); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.events {
		if strings.HasPrefix(e, "create:") {
			t.Errorf("an unchanged current generation must not be recreated: %v", f.events)
		}
	}
}

func TestOlderGenerationsSelection(t *testing.T) {
	list := []ContainerState{
		{Name: "sigmahub-res_a", Labels: map[string]string{LabelResourceID: "res_a"}},
		{Name: "sigmahub-res_a-gen2", Labels: map[string]string{LabelResourceID: "res_a"}},
		{Name: "sigmahub-res_b", Labels: map[string]string{LabelResourceID: "res_b"}},
	}
	old := olderGenerations(list, "res_a", "", "sigmahub-res_a-gen2")
	if len(old) != 1 || old[0].Name != "sigmahub-res_a" {
		t.Fatalf("olderGenerations = %+v", old)
	}
}

// TestOlderGenerationsServiceScoped proves a Compose service's generations are
// isolated: draining service "web" never selects service "db"'s container.
func TestOlderGenerationsServiceScoped(t *testing.T) {
	list := []ContainerState{
		{Name: "sigmahub-res_a-web-g1", Labels: map[string]string{LabelResourceID: "res_a", LabelService: "web"}},
		{Name: "sigmahub-res_a-web-g2", Labels: map[string]string{LabelResourceID: "res_a", LabelService: "web"}},
		{Name: "sigmahub-res_a-db-g1", Labels: map[string]string{LabelResourceID: "res_a", LabelService: "db"}},
	}
	old := olderGenerations(list, "res_a", "web", "sigmahub-res_a-web-g2")
	if len(old) != 1 || old[0].Name != "sigmahub-res_a-web-g1" {
		t.Fatalf("service-scoped olderGenerations = %+v", old)
	}
}

func TestRolloutName(t *testing.T) {
	if got := rolloutName("sigmahub-res_a", "abc1234"); got != "sigmahub-res_a-abc1234" {
		t.Errorf("rolloutName = %q", got)
	}
	if got := rolloutName("sigmahub-res_a", ""); got != "sigmahub-res_a" {
		t.Errorf("empty generation should return the base name, got %q", got)
	}
}

func indexOf(events []string, e string) int {
	for i, x := range events {
		if x == e {
			return i
		}
	}
	return -1
}

// fakeImages is an in-memory imageRetainer recording removals.
type fakeImages struct {
	imgs    []ImageSummary
	removed []string
}

func (f *fakeImages) ImageList(_ context.Context, _ string) ([]ImageSummary, error) {
	// Already sorted newest-first by the caller's contract; return as seeded.
	return f.imgs, nil
}
func (f *fakeImages) ImageRemove(_ context.Context, ref string, _ bool) error {
	f.removed = append(f.removed, ref)
	return nil
}

// TestRetainImages pins keep-last-N: the newest `keep` tags survive, older ones
// are pruned, and a tag in use (the running generation) is never removed even
// when it's old.
func TestRetainImages(t *testing.T) {
	prefix := "sigmahub/res_a:"
	tag := func(n int) string { return prefix + fmt.Sprintf("sha%02d", n) }
	// 12 images newest-first (sha11 … sha00).
	fi := &fakeImages{}
	for n := 11; n >= 0; n-- {
		fi.imgs = append(fi.imgs, ImageSummary{ID: fmt.Sprintf("id%d", n), RepoTags: []string{tag(n)}, Created: int64(n)})
	}
	// Keep 10; the running image is the oldest (sha00) — it must survive despite
	// being past the window, and it must not consume a keep slot.
	inUse := map[string]bool{tag(0): true}
	retainImages(context.Background(), fi, prefix, 10, inUse, func(string, ...any) {})

	// Newest 10 (sha11…sha02) kept by window; sha00 kept by in-use; sha01 pruned.
	if len(fi.removed) != 1 || fi.removed[0] != tag(1) {
		t.Fatalf("retention removed = %v, want only %s", fi.removed, tag(1))
	}
}

// TestRetainImagesNeverTouchesInUse guards the serving image even under a tiny
// keep budget.
func TestRetainImagesNeverTouchesInUse(t *testing.T) {
	prefix := "sigmahub/res_b:"
	tag := func(s string) string { return prefix + s }
	fi := &fakeImages{imgs: []ImageSummary{
		{ID: "i2", RepoTags: []string{tag("new")}, Created: 2},
		{ID: "i1", RepoTags: []string{tag("old")}, Created: 1},
	}}
	retainImages(context.Background(), fi, prefix, 1, map[string]bool{tag("old"): true}, func(string, ...any) {})
	for _, r := range fi.removed {
		if r == tag("old") {
			t.Fatalf("retention removed the in-use image %s", r)
		}
	}
}

// TestImageRepoPrefix pins retention scoping for single-container and Compose
// per-service tags, and the no-op for prebuilt (non-sigmahub) images.
func TestImageRepoPrefix(t *testing.T) {
	for in, want := range map[string]string{
		"sigmahub/res_a:abc1234":     "sigmahub/res_a:",
		"sigmahub/res_a-web:abc1234": "sigmahub/res_a-web:",
		"postgres:16":                "",
		"redis":                      "",
	} {
		if got := imageRepoPrefix(in); got != want {
			t.Errorf("imageRepoPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPerformRolloutRefusesHardCut proves a live container already holding the new
// generation's name with a DIFFERENT spec is NOT force-removed (never cut): the
// rollout refuses rather than tear down a serving container before a replacement
// is health-gated.
func TestPerformRolloutRefusesHardCut(t *testing.T) {
	f := newFakeRollout()
	// A running container occupies the target generation name but with a stale hash.
	f.seed("sigmahub-res_a-gen2", "res_a", "stalehash", true)

	spec, body := rolloutSpec("res_a", "gen2", "newhash")
	err := performRollout(context.Background(), f, healthyProbe, spec, body, "newhash", nil, noLog)
	if err == nil {
		t.Fatal("expected performRollout to refuse cutting a running same-name container")
	}
	if _, ok := f.byName["sigmahub-res_a-gen2"]; !ok {
		t.Fatal("the serving container must NOT have been removed (never cut)")
	}
	for _, e := range f.events {
		if e == "remove:sigmahub-res_a-gen2" {
			t.Fatal("hard cut: the serving container was removed")
		}
	}
}

// TestPerformRolloutReplacesStoppedLeftover proves a STOPPED same-name leftover
// (e.g. a crashed prior attempt) is safely removed and recreated — it is not
// serving, so create-before-destroy does not apply.
func TestPerformRolloutReplacesStoppedLeftover(t *testing.T) {
	f := newFakeRollout()
	f.seed("sigmahub-res_a-gen2", "res_a", "stalehash", false) // stopped leftover

	spec, body := rolloutSpec("res_a", "gen2", "newhash")
	if err := performRollout(context.Background(), f, healthyProbe, spec, body, "newhash", nil, noLog); err != nil {
		t.Fatalf("stopped leftover should be replaceable: %v", err)
	}
	if indexOf(f.events, "remove:sigmahub-res_a-gen2") < 0 {
		t.Fatal("the stopped leftover should have been removed")
	}
	if indexOf(f.events, "create:sigmahub-res_a-gen2") < 0 {
		t.Fatal("a fresh generation should have been created")
	}
}

// TestRolloutManagedGroups proves a (resource, service) group with a
// deploy.rollout/recreate op is recognised as rollout-owned (GC never reaps its
// generations), while a service REMOVED from the compose file — no op in the doc
// — is NOT protected, so its orphaned container is garbage-collected.
func TestRolloutManagedGroups(t *testing.T) {
	spec, _ := rolloutSpec("res_a", "gen2", "h") // single-container (service "")
	rb, _ := json.Marshal(spec)
	svcSpec, _ := recreateSpec("res_c", "db", "g1", "h")
	rc, _ := json.Marshal(svcSpec)
	appSpec, _ := json.Marshal(ContainerSpec{Name: "sigmahub-res_b"})
	doc := dsd.Document{Ops: []dsd.Op{
		{ID: "res:res_a", Kind: KindDeployRollout, Spec: rb},
		{ID: "res:res_c:db", Kind: KindDeployRecreate, Spec: rc},
		{ID: "res:res_b", Kind: KindContainerApply, Spec: appSpec},
	}}
	m := rolloutManagedGroups(doc)
	if !m[rolloutGroupKey("res_a", "")] {
		t.Fatal("res_a (deploy.rollout, single-container) must be rollout-managed")
	}
	if !m[rolloutGroupKey("res_c", "db")] {
		t.Fatal("res_c/db (deploy.recreate) must be rollout-managed")
	}
	// A service dropped from the compose file has no op → its containers are
	// GC-able even though the resource still has other rollout ops.
	if m[rolloutGroupKey("res_c", "worker")] {
		t.Fatal("a removed compose service must NOT be protected from GC")
	}
	if m[rolloutGroupKey("res_b", "")] {
		t.Fatal("res_b (plain container.apply) must NOT be rollout-managed")
	}
}

// recreateSpec builds a RecreateSpec + create body for a Compose service.
func recreateSpec(res, service, gen, hash string) (RecreateSpec, any) {
	spec := RecreateSpec{
		Container: ContainerSpec{
			ResourceID: res, Service: service, Name: "sigmahub-" + res + "-" + service, Image: "img",
		},
		Generation: gen,
		Health:     HealthProbe{Type: "http", Port: 8080, Path: "/", IntervalSec: 1, TimeoutSec: 1},
	}
	body := map[string]any{"Labels": map[string]string{
		LabelManaged: "true", LabelResourceID: res, LabelService: service, LabelSpecHash: hash,
	}}
	return spec, body
}

// TestPerformRecreateRemovesOldFirst proves the recreate swap removes the old
// generation BEFORE creating the new one (an exclusive resource can't be held
// twice), and scopes to the service so a sibling service is untouched.
func TestPerformRecreateRemovesOldFirst(t *testing.T) {
	f := newFakeRollout()
	// db service, old generation running; plus an unrelated web service.
	f.seedSvc("sigmahub-res_a-db-g1", "res_a", "db", "oldhash", true)
	f.seedSvc("sigmahub-res_a-web-g1", "res_a", "web", "whash", true)

	spec, body := recreateSpec("res_a", "db", "g2", "newhash")
	if err := performRecreate(context.Background(), f, healthyProbe, spec, body, "newhash", nil, noLog); err != nil {
		t.Fatal(err)
	}
	// The old db generation must be removed before the new db is created.
	rmOld := indexOf(f.events, "remove:sigmahub-res_a-db-g1")
	crNew := indexOf(f.events, "create:sigmahub-res_a-db-g2")
	if rmOld < 0 || crNew < 0 || rmOld > crNew {
		t.Fatalf("recreate must remove old before creating new; events=%v", f.events)
	}
	// The sibling web service must be untouched.
	for _, e := range f.events {
		if strings.Contains(e, "web") {
			t.Fatalf("recreate touched a sibling service: %s", e)
		}
	}
}

// TestPerformRecreateReGatesHealthOnResume proves a resumed apply re-runs the
// health gate on an already-running matching generation instead of reporting
// success blindly — else an unhealthy recreate is recorded as a good deploy
// (SIGMA-147).
func TestPerformRecreateReGatesHealthOnResume(t *testing.T) {
	spec, body := recreateSpec("res_a", "db", "g2", "newhash")

	// Unhealthy: the new gen is running with a matching hash but never became
	// healthy; the short-circuit must re-gate and fail.
	fBad := newFakeRollout()
	fBad.seedSvc("sigmahub-res_a-db-g2", "res_a", "db", "newhash", true)
	if err := performRecreate(context.Background(), fBad, unhealthyProbe, spec, body, "newhash", nil, noLog); err == nil {
		t.Fatal("resumed recreate of an unhealthy running generation must fail, not report success")
	}

	// Healthy: the same short-circuit converges and returns nil.
	fOK := newFakeRollout()
	fOK.seedSvc("sigmahub-res_a-db-g2", "res_a", "db", "newhash", true)
	if err := performRecreate(context.Background(), fOK, healthyProbe, spec, body, "newhash", nil, noLog); err != nil {
		t.Fatalf("resumed recreate of a healthy running generation should converge: %v", err)
	}
}

// TestPersistRolloutGeneration proves a deployed generation is recorded as
// desired (so reconcile can repair it), an older generation of the same
// (resource, service) is pruned (so a drained one is not resurrected), and an
// unrelated resource is left alone (SIGMA-146).
func TestPersistRolloutGeneration(t *testing.T) {
	st, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := &Driver{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := st.PutDesired("sigmahub-res_a-g1", ContainerSpec{ResourceID: "res_a", Name: "sigmahub-res_a-g1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutDesired("sigmahub-res_b", ContainerSpec{ResourceID: "res_b", Name: "sigmahub-res_b"}); err != nil {
		t.Fatal(err)
	}

	genSpec := ContainerSpec{ResourceID: "res_a", Name: "sigmahub-res_a-g2"}
	if err := d.persistRolloutGeneration(genSpec); err != nil {
		t.Fatal(err)
	}

	all, err := st.AllDesired()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := all["sigmahub-res_a-g2"]; !ok {
		t.Error("new generation must be recorded as desired")
	}
	if _, ok := all["sigmahub-res_a-g1"]; ok {
		t.Error("the older generation of the same resource must be pruned")
	}
	if _, ok := all["sigmahub-res_b"]; !ok {
		t.Error("an unrelated resource must be left alone")
	}
}
