package reconciler

// SIGMA-320: the fleet resync must be BOUNDED, and it must say how long it took.
//
// SIGMA-288 bounded the fan-out from API handlers, but the resync itself walked
// the fleet one server at a time in a single goroutine — it took a slot,
// reconciled, released, and only then looked at the next server. So the pass
// cost N times one reconcile no matter how much headroom the semaphore had, and
// nothing anywhere measured it. The agent advertises a 60s drift-repair SLO and
// the CP's own re-drive logic is written assuming "the 60s resync re-runs
// anything missed"; both quietly stop being true once the fleet is big enough
// that a pass outlasts the tick, and there was no signal that it had.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestResyncPassIsBounded(t *testing.T) {
	const servers = 500
	// concurrencyStore's spec read sleeps for this long, standing in for the
	// ~15 sequential round trips a real reconcile makes.
	const perServer = 20 * time.Millisecond

	list := make([]struct{ ServerID, OrgID string }, servers)
	for i := range list {
		list[i] = struct{ ServerID, OrgID string }{fmt.Sprintf("srv_%03d", i), "org_1"}
	}
	st := &concurrencyStore{panicStore: panicStore{servers: list}}
	rec := New(slog.New(slog.NewTextHandler(io.Discard, nil)), st, nil)

	var mu sync.Mutex
	var passes int
	var passDur time.Duration
	rec.SetResyncPassObserver(func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		passes++
		if passDur == 0 {
			passDur = d
		}
	})

	// A short tick so the first pass starts at once; the pass duration, not the
	// interval, is what is under test.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx, 20*time.Millisecond)

	// Serially this pass is 500 x 20ms = 10s. Bounded by the reconcile
	// semaphore it is that divided by the number of concurrent reconciles, and
	// the budget below leaves 100% slack on top of that — so it fails on a
	// serial pass and passes on a bounded one without being timing-flaky.
	serial := servers * perServer
	budget := 2 * (serial / reconcileConcurrency)
	deadline := time.Now().Add(serial + 5*time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := passes
		mu.Unlock()
		if done > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	mu.Lock()
	got, n := passDur, passes
	mu.Unlock()
	if n == 0 {
		t.Fatalf("no resync pass completed within %s — the pass is not being timed, or it is far slower than serial", serial+5*time.Second)
	}
	if got > budget {
		t.Fatalf("resync pass over %d servers took %s, want <= %s (serial would be %s; the pass is not using the %d reconcile slots)",
			servers, got, budget, serial, reconcileConcurrency)
	}

	// Bounded, not unbounded: the pass must never exceed the semaphore that
	// exists to keep reconciles from eating the whole connection pool
	// (SIGMA-288). Parallelising the resync must not be a way around it.
	peak, _ := st.counters()
	if peak > reconcileConcurrency {
		t.Fatalf("peak concurrent reconciles during the pass = %d, want <= %d", peak, reconcileConcurrency)
	}
	if peak < 2 {
		t.Fatalf("peak concurrent reconciles = %d — the pass is still serial", peak)
	}
}
