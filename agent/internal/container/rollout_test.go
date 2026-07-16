package container

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
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
	old := olderGenerations(list, "res_a", "sigmahub-res_a-gen2")
	if len(old) != 1 || old[0].Name != "sigmahub-res_a" {
		t.Fatalf("olderGenerations = %+v", old)
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
	res := "res_a"
	tag := func(n int) string { return imageTagPrefix(res) + fmt.Sprintf("sha%02d", n) }
	// 12 images newest-first (sha11 … sha00).
	fi := &fakeImages{}
	for n := 11; n >= 0; n-- {
		fi.imgs = append(fi.imgs, ImageSummary{ID: fmt.Sprintf("id%d", n), RepoTags: []string{tag(n)}, Created: int64(n)})
	}
	// Keep 10; the running image is the oldest (sha00) — it must survive despite
	// being past the window, and it must not consume a keep slot.
	inUse := map[string]bool{tag(0): true}
	retainImages(context.Background(), fi, res, 10, inUse, func(string, ...any) {})

	// Newest 10 (sha11…sha02) kept by window; sha00 kept by in-use; sha01 pruned.
	if len(fi.removed) != 1 || fi.removed[0] != tag(1) {
		t.Fatalf("retention removed = %v, want only %s", fi.removed, tag(1))
	}
}

// TestRetainImagesNeverTouchesInUse guards the serving image even under a tiny
// keep budget.
func TestRetainImagesNeverTouchesInUse(t *testing.T) {
	res := "res_b"
	tag := func(s string) string { return imageTagPrefix(res) + s }
	fi := &fakeImages{imgs: []ImageSummary{
		{ID: "i2", RepoTags: []string{tag("new")}, Created: 2},
		{ID: "i1", RepoTags: []string{tag("old")}, Created: 1},
	}}
	retainImages(context.Background(), fi, res, 1, map[string]bool{tag("old"): true}, func(string, ...any) {})
	for _, r := range fi.removed {
		if r == tag("old") {
			t.Fatalf("retention removed the in-use image %s", r)
		}
	}
}
