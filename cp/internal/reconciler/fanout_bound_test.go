package reconciler

// SIGMA-288: the fan-out bound, without a database.
//
// The integration half of this (cp/internal/integration) proves the real
// symptom — 25 concurrent ReconcileAsync calls against a pool of 20 wedge
// every connection and nothing converges. This is the cheap regression guard:
// it counts how many reconciles are inside the store at the same moment and
// asserts the semaphore holds, so removing the bound fails a test that needs
// no Postgres to run.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// concurrencyStore is panicStore that never panics, plus a high-water mark of
// concurrent reconciles. Reads block briefly so overlap is observable at all.
type concurrencyStore struct {
	panicStore
	mu   sync.Mutex
	cur  int
	peak int
	done int
}

func (c *concurrencyStore) ResourceSpecsForServer(_ context.Context, _ string) ([]store.ResourceSpec, error) {
	c.mu.Lock()
	c.cur++
	if c.cur > c.peak {
		c.peak = c.cur
	}
	c.mu.Unlock()
	// Stand in for the ~18 round trips a real reconcile makes; without it the
	// renders finish so fast they would never overlap and the test would pass
	// even unbounded.
	time.Sleep(20 * time.Millisecond)
	c.mu.Lock()
	c.cur--
	c.done++
	c.mu.Unlock()
	return nil, nil
}

func (c *concurrencyStore) counters() (peak, done int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak, c.done
}

var _ Store = (*concurrencyStore)(nil)

func TestReconcileAsyncFanOutIsBounded(t *testing.T) {
	// The bound is only useful if it leaves the pool room to serve requests: a
	// reconcile pins TWO connections and store.Open floors the pool at 20, so
	// anything above 8 puts the CP back within reach of the deadlock.
	if reconcileConcurrency < 1 || reconcileConcurrency > 8 {
		t.Fatalf("reconcileConcurrency = %d, want 1..8 (two connections each, pool floor 20)", reconcileConcurrency)
	}
	st := &concurrencyStore{}
	rec := New(slog.New(slog.NewTextHandler(io.Discard, nil)), st, nil)

	const servers = 25
	for i := 0; i < servers; i++ {
		rec.ReconcileAsync("org_1", "srv_"+string(rune('a'+i%26)))
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		peak, done := st.counters()
		if done >= servers {
			if peak > reconcileConcurrency {
				t.Fatalf("peak concurrent reconciles = %d, want <= %d", peak, reconcileConcurrency)
			}
			return
		}
		if peak > reconcileConcurrency {
			t.Fatalf("peak concurrent reconciles = %d, want <= %d", peak, reconcileConcurrency)
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d reconciles finished", done, servers)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
