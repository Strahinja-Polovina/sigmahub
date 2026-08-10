package apply

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

func testJournal(t *testing.T) *Journal {
	t.Helper()
	j, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestApplyDependencyOrderAndSkip(t *testing.T) {
	j := testJournal(t)
	reg := NewRegistry()
	var applied []string
	reg.Register("ok", func(_ context.Context, op dsd.Op) error {
		applied = append(applied, op.ID)
		return nil
	})
	reg.Register("boom", func(context.Context, dsd.Op) error { return errors.New("boom") })

	// c depends on b, b depends on a(boom) → b skipped-not-run? No: a fails,
	// b (depends a) skipped, c (depends b) skipped.
	doc := dsd.Document{Version: 1, Ops: []dsd.Op{
		{ID: "a", Kind: "boom"},
		{ID: "b", Kind: "ok", DependsOn: []string{"a"}},
		{ID: "c", Kind: "ok", DependsOn: []string{"b"}},
		{ID: "d", Kind: "ok"},
	}}
	results, err := j.applyWith(reg, doc)
	if err != nil {
		t.Fatal(err)
	}
	if results["a"].State != "failed" {
		t.Fatalf("a: %+v", results["a"])
	}
	if results["b"].State != "skipped" || results["c"].State != "skipped" {
		t.Fatalf("b/c not skipped: %+v %+v", results["b"], results["c"])
	}
	if results["d"].State != "applied" {
		t.Fatalf("d should apply independently: %+v", results["d"])
	}
	// Only d's handler ran (a errored, b/c skipped before handler).
	if len(applied) != 1 || applied[0] != "d" {
		t.Fatalf("handlers run = %v, want [d]", applied)
	}
}

func TestApplyResumeSkipsCompletedOps(t *testing.T) {
	j := testJournal(t)
	reg := NewRegistry()
	runs := map[string]int{}
	reg.Register("count", func(_ context.Context, op dsd.Op) error {
		runs[op.ID]++
		return nil
	})
	doc := dsd.Document{Version: 5, Ops: []dsd.Op{{ID: "x", Kind: "count"}, {ID: "y", Kind: "count"}}}

	if _, err := j.applyWith(reg, doc); err != nil {
		t.Fatal(err)
	}
	// Re-apply the SAME version (simulating a crash-resume re-fetch): handlers
	// must not run again.
	if _, err := j.applyWith(reg, doc); err != nil {
		t.Fatal(err)
	}
	if runs["x"] != 1 || runs["y"] != 1 {
		t.Fatalf("resume re-ran ops: %v", runs)
	}
}

func TestUnknownKindFails(t *testing.T) {
	j := testJournal(t)
	reg := NewRegistry()
	doc := dsd.Document{Version: 1, Ops: []dsd.Op{{ID: "a", Kind: "run.shell"}}}
	results, err := j.applyWith(reg, doc)
	if err != nil {
		t.Fatal(err)
	}
	if results["a"].State != "failed" {
		t.Fatalf("unknown kind should fail: %+v", results["a"])
	}
}

// applyWith is a tiny test helper mirroring the production Apply call.
func (j *Journal) applyWith(reg *Registry, doc dsd.Document) (map[string]OpResult, error) {
	return reg.Apply(context.Background(), quietLog(), j, doc)
}

func TestLastAppliedVersionAdvances(t *testing.T) {
	j := testJournal(t)
	reg := NewRegistry()
	reg.Register("ok", func(context.Context, dsd.Op) error { return nil })
	if v, _ := j.LastAppliedVersion(); v != 0 {
		t.Fatalf("initial version = %d, want 0", v)
	}
	if _, err := j.applyWith(reg, dsd.Document{Version: 7, Ops: []dsd.Op{{ID: "a", Kind: "ok"}}}); err != nil {
		t.Fatal(err)
	}
	if v, _ := j.LastAppliedVersion(); v != 7 {
		t.Fatalf("version = %d, want 7", v)
	}
}

// TestSkipCarriesPrerequisiteError pins SIGMA-301: when an op is skipped because
// a prerequisite failed, the skip must name WHICH prerequisite and carry ITS
// error. The dependent rollout op ("res:<id>") is the one the CP routes into the
// deployment's status, so the bare string "prerequisite failed" was literally all
// an operator got for a registry 401, a volume collision or a full disk — the
// failing op ("pull:<id>", "vol:<id>:<name>") maps to no deploy phase and its
// error reached nothing.
func TestSkipCarriesPrerequisiteError(t *testing.T) {
	j := testJournal(t)
	reg := NewRegistry()
	reg.Register("image.pull", func(context.Context, dsd.Op) error {
		return errors.New("pull ghcr.io/o/r:sha: denied: requested access to the resource is denied")
	})
	reg.Register("deploy.rollout", func(context.Context, dsd.Op) error { return nil })

	doc := dsd.Document{Version: 3, Ops: []dsd.Op{
		{ID: "pull:res_a", Kind: "image.pull"},
		{ID: "res:res_a", Kind: "deploy.rollout", DependsOn: []string{"pull:res_a"}},
	}}
	results, err := j.applyWith(reg, doc)
	if err != nil {
		t.Fatal(err)
	}
	if results["res:res_a"].State != "skipped" {
		t.Fatalf("rollout state = %+v, want skipped", results["res:res_a"])
	}
	got := results["res:res_a"].Err
	if !strings.Contains(got, "pull:res_a") {
		t.Errorf("skip error %q does not name the failing prerequisite", got)
	}
	if !strings.Contains(got, "denied: requested access to the resource is denied") {
		t.Errorf("skip error %q discards the prerequisite's own error", got)
	}
}
