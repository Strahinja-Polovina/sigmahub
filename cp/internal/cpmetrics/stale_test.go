package cpmetrics

import (
	"testing"
	"time"
)

// SIGMA-365. The last-success clocks exist so a wedged loop is visible; until
// /livez read them, the only consumer was a Prometheus nobody had installed.
// These pin the judgement /livez makes.
func TestStaleLoops(t *testing.T) {
	r := New()

	// A control plane that just booted is not wedged, even though no loop has
	// reported a pass yet: every budget is measured from process start.
	r.startedAt = time.Now()
	if stale := r.StaleLoops(time.Now()); len(stale) != 0 {
		t.Fatalf("a freshly booted CP is not wedged, got %v", stale)
	}
	// Still not wedged a couple of minutes in — the slowest loop has not even
	// reached its first tick.
	if stale := r.StaleLoops(time.Now().Add(2 * time.Minute)); len(stale) != 0 {
		t.Fatalf("2m after boot nothing is over budget, got %v", stale)
	}

	// Two hours up with no loop having ever reported: every loop is over budget.
	r.startedAt = time.Now().Add(-2 * time.Hour)
	if stale := r.StaleLoops(time.Now()); len(stale) != len(Loops) {
		t.Fatalf("stale = %v, want all %d loops", stale, len(Loops))
	}

	// A loop that reports a pass clears itself, and only itself.
	r.Loop(LoopReconcilerResync).Pass()
	stale := r.StaleLoops(time.Now())
	for _, name := range stale {
		if name == LoopReconcilerResync {
			t.Fatalf("a loop that just passed must not be stale: %v", stale)
		}
	}
	if len(stale) != len(Loops)-1 {
		t.Fatalf("stale = %v, want every loop but the one that passed", stale)
	}

	// ...and it goes stale again once its budget elapses without another pass.
	if stale := r.StaleLoops(time.Now().Add(2 * time.Hour)); len(stale) != len(Loops) {
		t.Fatalf("a loop that last passed 2h ago is stale again; got %v", stale)
	}

	// A loop that is TICKING AND FAILING is alive, and must not be reported
	// wedged (SIGMA-365). Killing the control plane does not fix the bad row or
	// the declined card that made the pass fail, and pass verdicts are the first
	// failure across the whole fleet — so treating "errored" as "stopped" turned
	// one tenant's condition into a permanent 503 and a fleet-wide restart loop
	// that no restart could clear. The failing case is alerted on separately, off
	// the error counter.
	r.Loop(LoopSweeper).Fail()
	for _, name := range r.StaleLoops(time.Now()) {
		if name == LoopSweeper {
			t.Fatal("a loop that ticks and fails is alive; only a STOPPED loop is wedged")
		}
	}
	// ...but once it stops reporting at all, it is wedged.
	if stale := r.StaleLoops(time.Now().Add(2 * time.Hour)); len(stale) != len(Loops) {
		t.Fatalf("a loop that stopped reporting must be wedged; got %v", stale)
	}
}

// The budget table has to cover every loop the catalog names, or a loop silently
// opts out of liveness by existing.
func TestEveryLoopHasAStaleBudget(t *testing.T) {
	for _, name := range Loops {
		if _, ok := staleBudgets[name]; !ok {
			t.Errorf("loop %q has no stale budget, so /livez can never report it wedged", name)
		}
	}
}
