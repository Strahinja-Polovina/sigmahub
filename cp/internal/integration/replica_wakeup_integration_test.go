package integration

// SIGMA-291: a DSD change made on one control-plane replica must wake the agent
// long-polling a DIFFERENT replica.
//
// Almost all shared state is already in Postgres — advisory locks, SKIP LOCKED
// leases, partial unique indexes — which is exactly what makes the long-poll
// waiter map easy to miss. It is a per-process map, and notify() closes only
// the channels registered in the process that rendered the change. Run two
// replicas behind a load balancer (a reasonable reading of a self-hostable PaaS
// with no documented instance-count limit) and roughly half of all DSD changes
// are delivered a full longPollTimeout late instead of immediately: deploys and
// config changes feel intermittently sluggish, with no error anywhere and
// nothing to grep for.
//
// This is the two-instance test: instance A serves the agent's long-poll,
// instance B renders the change. It asserts A's poll returns the new document
// promptly rather than waiting out its own timeout.

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"context"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// startChangeBus wires one simulated replica onto the shared LISTEN/NOTIFY bus
// and waits until its listener is actually connected — a NOTIFY published
// before the listener has issued its LISTEN is simply not delivered, so
// starting the goroutine is not the same as being subscribed.
func startChangeBus(t *testing.T, st *store.Store, rec *reconciler.Reconciler, log *slog.Logger) {
	t.Helper()
	rec.SetChangeBus(st)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	woken := make(chan string, 8)
	go st.SubscribeDSDChanges(ctx, log, func(serverID string) {
		rec.WakeServer(serverID)
		select {
		case woken <- serverID:
		default:
		}
	})

	// Probe until a published notification comes back, i.e. LISTEN is live.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := st.PublishDSDChange(context.Background(), "srv_bus_probe"); err != nil {
			t.Fatalf("publish probe: %v", err)
		}
		select {
		case <-woken:
			return
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("dsd change listener never came up")
		}
	}
}

func TestLongPollOnOneReplicaIsWokenByAReconcileOnAnother(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Two independent control-plane processes over one database. Separate
	// Reconcilers is exactly what two containers are.
	recA := reconciler.New(log, st, dsdKey)
	recB := reconciler.New(log, st, dsdKey)
	// Each "process" publishes changes onto the shared bus and listens on it,
	// which is exactly the two lines main.go runs at boot.
	startChangeBus(t, st, recA, log)
	startChangeBus(t, st, recB, log)

	// Long enough that "woken" and "timed out" are unmistakably different
	// outcomes, short enough that a failing run does not cost 25 seconds.
	prev := api.LongPollTimeout()
	api.SetLongPollTimeout(4 * time.Second)
	t.Cleanup(func() { api.SetLongPollTimeout(prev) })

	srvA := api.New(log, st, st, st, api.Options{
		DevServiceToken: "dev",
		DSDStore:        st,
		DSDWaiter:       recA,
		Reconcile:       recA,
		DSDPublicKey:    dsdKey.Public().(ed25519.PublicKey),
	})
	tsA := httptest.NewServer(srvA.Handler())
	defer tsA.Close()

	orgID := "org_replica"
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID, agentTok := reg.Server.ID, reg.AgentToken

	// Give the server a document to be at the current version of, so the poll
	// below has something to wait past rather than returning immediately.
	if err := recB.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	current, err := st.CurrentDSDVersion(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if current == 0 {
		t.Fatal("no DSD rendered for a freshly registered server")
	}

	// The agent long-polls instance A, asking for anything newer than what it
	// already has.
	type pollResult struct {
		signed  dsd.Signed
		code    int
		elapsed time.Duration
	}
	results := make(chan pollResult, 1)
	go func() {
		start := time.Now()
		signed, code := agentGetDSD(t, tsA.URL, agentTok, current)
		results <- pollResult{signed, code, time.Since(start)}
	}()

	// Let the poll get past its version read and into the wait. Without this
	// the test could pass on the read rather than on the wake-up.
	time.Sleep(400 * time.Millisecond)

	// The change lands on instance B: an operator upgrading this host from the
	// dashboard, served by the other replica.
	if err := st.SetDesiredAgentVersion(ctx, orgID, serverID, "v9.9.9", "test"); err != nil {
		t.Fatal(err)
	}
	if err := recB.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}

	got := <-results
	if got.code != http.StatusOK {
		t.Fatalf("long-poll on instance A returned %d after %s — a reconcile on instance B did not wake it, so the agent waited out the full long-poll window and every cross-replica change is delivered that late",
			got.code, got.elapsed.Round(time.Millisecond))
	}
	if got.signed.Document.Version <= current {
		t.Fatalf("long-poll returned version %d, want > %d", got.signed.Document.Version, current)
	}
	// Woken, not timed out: the whole point is that the delivery is prompt.
	if got.elapsed > 2*time.Second {
		t.Fatalf("long-poll took %s — that is the timeout expiring, not a wake-up", got.elapsed.Round(time.Millisecond))
	}
}
