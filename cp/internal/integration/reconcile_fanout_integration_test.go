package integration

// SIGMA-288: an org-wide re-render must not consume the entire CP connection
// pool.
//
// A single Reconcile needs TWO pool connections at once: LockServerReconcile
// checks one out and holds it for the whole reconcile (the advisory lock is
// session-scoped, so it has to), and every read after that asks the pool for
// another. With an unbounded fan-out — reconcileOrg spawns one ReconcileAsync
// per server, and the backup scheduler fans out the same way — an org with
// more servers than the pool has connections wedges the process: MaxConns
// goroutines win a connection for their lock, the rest queue for one, and the
// winners then queue for the second connection they need to read anything.
// Nothing can make progress until the per-goroutine 10s timeout unwinds it,
// and every other HTTP handler (heartbeats, DSD long-polls, dashboard reads)
// is starved of connections meanwhile.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
)

func TestReconcileOrgFanOutDoesNotExhaustPool(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := reconciler.New(log, st, dsdKey)

	// Every completed render bumps the counter. A render that never happens is
	// exactly what pool exhaustion looks like from outside: no error surfaces to
	// the operator, the documents simply never converge.
	var rendered atomic.Int64
	rec.SetObservers(nil, func(time.Duration) { rendered.Add(1) })

	orgID := "org_fanout"
	// 25 servers against a pool floor of 20 (store.Open) — more servers than
	// connections is the whole point.
	const servers = 25
	for i := 0; i < servers; i++ {
		name := fmt.Sprintf("host-%02d", i)
		bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, name, "general", "", "", "test", time.Hour)
		if err != nil {
			t.Fatalf("bootstrap token %d: %v", i, err)
		}
		if _, err := st.RegisterServer(ctx, bootTok, name, "0.1.0", json.RawMessage(`{}`), ""); err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
	}

	// The fan-out reconcileOrg performs when the org's image registry changes.
	list, err := st.ListServers(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != servers {
		t.Fatalf("servers = %d, want %d", len(list), servers)
	}
	start := time.Now()
	for _, sv := range list {
		rec.ReconcileAsync(orgID, sv.ID)
	}

	// Generous: a bounded fan-out finishes 25 renders in well under a second.
	// The bug shows up as renders that never happen at all — the deadlocked
	// goroutines are unwound by their own 10s timeout and give up.
	deadline := time.Now().Add(30 * time.Second)
	for rendered.Load() < servers && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := rendered.Load(); n != servers {
		t.Fatalf("rendered %d/%d documents after %s — the fan-out exhausted the connection pool", n, servers, time.Since(start).Round(time.Millisecond))
	}
	t.Logf("fan-out of %d reconciles completed in %s", servers, time.Since(start).Round(time.Millisecond))
}
