package apply

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// TestApplyFailsHungOpAndContinues covers SIGMA-339: a handler that never
// returns (a pull against a black-holing registry, a build RUN hanging on an
// unreachable apt mirror) used to block the serial apply loop forever, holding
// every later DSD version for this host behind it with no signal anywhere. The
// per-op deadline must fail it and let the rest of the document run.
func TestApplyFailsHungOpAndContinues(t *testing.T) {
	j := testJournal(t)
	reg := NewRegistry()
	reg.SetTimeout("hang", 50*time.Millisecond)
	started := make(chan struct{})
	reg.Register("hang", func(ctx context.Context, _ dsd.Op) error {
		close(started)
		<-ctx.Done() // never returns on its own
		return ctx.Err()
	})
	var applied []string
	reg.Register("ok", func(_ context.Context, op dsd.Op) error {
		applied = append(applied, op.ID)
		return nil
	})

	doc := dsd.Document{Version: 1, Ops: []dsd.Op{
		{ID: "wedged", Kind: "hang"},
		{ID: "next", Kind: "ok"},
	}}

	done := make(chan struct{})
	var results map[string]OpResult
	var err error
	go func() {
		results, err = j.applyWith(reg, doc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Apply never returned: a hung handler wedged the apply loop")
	}
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if results["wedged"].State != "failed" {
		t.Fatalf("hung op = %+v, want failed", results["wedged"])
	}
	if !strings.Contains(results["wedged"].Err, "timed out") {
		t.Fatalf("hung op error = %q, want a timeout explanation", results["wedged"].Err)
	}
	// The whole point: the loop keeps converging the rest of the host.
	if len(applied) != 1 || applied[0] != "next" {
		t.Fatalf("ops run after the hung one = %v, want [next]", applied)
	}
	// And it is durable — a resync must not re-run the wedged op as if nothing
	// happened, it must show up as a failure in the journal.
	if prev, ok, jerr := j.Result(1, "wedged"); jerr != nil || !ok || prev.State != "failed" {
		t.Fatalf("journal result = %+v ok=%v err=%v", prev, ok, jerr)
	}
}

// TestOpTimeoutDefaults pins the shape of the deadline table: long-running
// kinds keep a generous ceiling (notably above backup's own 25m self-limit, so
// this deadline never pre-empts the handler's better error), everything else
// falls back to the default.
func TestOpTimeoutDefaults(t *testing.T) {
	reg := NewRegistry()
	if got := reg.timeoutFor("container.apply"); got != DefaultOpTimeout {
		t.Fatalf("container.apply timeout = %s, want default %s", got, DefaultOpTimeout)
	}
	if got := reg.timeoutFor("image.build"); got != 30*time.Minute {
		t.Fatalf("image.build timeout = %s, want 30m", got)
	}
	for _, kind := range []string{"backup.run", "backup.base", "backup.verify", "backup.restore", "backup.restore-pitr"} {
		if got := reg.timeoutFor(kind); got <= 25*time.Minute {
			t.Fatalf("%s timeout = %s, must exceed the handler's own 25m limit", kind, got)
		}
	}
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
