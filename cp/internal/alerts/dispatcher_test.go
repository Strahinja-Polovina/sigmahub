package alerts

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// fakeOutbox is an in-memory Store for the dispatcher. Every method must be safe
// for concurrent use: the drain sends deliveries in parallel, so the store slice
// is hit from several goroutines at once.
type fakeOutbox struct {
	mu       sync.Mutex
	due      []store.AlertDelivery
	channel  store.AlertChannelSend
	results  map[int64]bool
	renewals int
	// notHeld is the set of delivery IDs whose lease renewal reports "someone
	// else owns this row now", which must skip the send entirely.
	notHeld map[int64]bool
	// sent records deliveries that actually reached the transport.
	sent map[int64]bool
}

func (f *fakeOutbox) DueAlertDeliveries(_ context.Context, limit int) ([]store.AlertDelivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit > len(f.due) {
		limit = len(f.due)
	}
	out := f.due[:limit]
	f.due = f.due[limit:]
	return out, nil
}

func (f *fakeOutbox) AlertChannelForSend(_ context.Context, _, _ string) (store.AlertChannelSend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.channel, nil
}

func (f *fakeOutbox) RenewAlertDeliveryLease(_ context.Context, id int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewals++
	return !f.notHeld[id], nil
}

func (f *fakeOutbox) SetAlertDeliveryResult(_ context.Context, id int64, ok bool, _ string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.results == nil {
		f.results = map[int64]bool{}
	}
	f.results[id] = ok
	return nil
}

func (f *fakeOutbox) EnqueueCertExpiringAlerts(context.Context, time.Duration) error { return nil }

func (f *fakeOutbox) markSent(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent == nil {
		f.sent = map[int64]bool{}
	}
	f.sent[id] = true
}

// TestDrain_DeliversConcurrently pins the throughput property the outbox needs
// during a correlated incident (SIGMA-255). A partition that makes a few hundred
// servers miss heartbeats enqueues a burst of outbox rows in a single sweeper
// pass; if the drain sends them strictly one at a time, a slow-but-working
// endpoint stretches the operator's notification over hours. Fifty deliveries
// against a 200ms endpoint take ~10s serially, which is why the bound here is
// far below that: it only passes if sends overlap.
func TestDrain_DeliversConcurrently(t *testing.T) {
	const n = 50
	const perSend = 200 * time.Millisecond

	// A slow-but-healthy Slack endpoint. Also tracks peak concurrency so a
	// regression to serial sending is reported as such, not just as "slow".
	var mu sync.Mutex
	var inFlight, peak int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(perSend)
		mu.Lock()
		inFlight--
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	due := make([]store.AlertDelivery, 0, n)
	for i := 1; i <= n; i++ {
		due = append(due, store.AlertDelivery{ID: int64(i), OrgID: "org", ChannelID: "ch", Event: "server_unreachable", Title: "Server down", Body: "srv"})
	}
	st := &fakeOutbox{
		due:     due,
		channel: store.AlertChannelSend{Kind: "slack", Secret: srv.URL},
	}

	cfg := Config{BatchSize: n}
	cfg.defaults()

	start := time.Now()
	drain(context.Background(), slog.New(slog.DiscardHandler), st, NewSender(), cfg)
	elapsed := time.Since(start)

	if len(st.results) != n {
		t.Fatalf("recorded %d results, want %d", len(st.results), n)
	}
	for id, ok := range st.results {
		if !ok {
			t.Fatalf("delivery %d failed, want success", id)
		}
	}
	mu.Lock()
	gotPeak := peak
	mu.Unlock()
	if gotPeak < 2 {
		t.Fatalf("peak in-flight sends = %d: deliveries were sent serially", gotPeak)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("drain of %d x %v deliveries took %v; serial drain is ~%v, concurrent drain must be far below it",
			n, perSend, elapsed, time.Duration(n)*perSend)
	}
}

// TestDrain_SkipsWhenLeaseLost keeps the concurrency change honest about the
// duplicate-delivery guard from SIGMA-130: a row whose lease renewal reports it
// is no longer ours must not be sent, and must not be finalized either.
func TestDrain_SkipsWhenLeaseLost(t *testing.T) {
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer srv.Close()

	st := &fakeOutbox{
		due: []store.AlertDelivery{
			{ID: 1, OrgID: "org", ChannelID: "ch"},
			{ID: 2, OrgID: "org", ChannelID: "ch"},
		},
		channel: store.AlertChannelSend{Kind: "slack", Secret: srv.URL},
		notHeld: map[int64]bool{2: true},
	}
	cfg := Config{}
	cfg.defaults()
	drain(context.Background(), slog.New(slog.DiscardHandler), st, NewSender(), cfg)

	mu.Lock()
	gotHits := hits
	mu.Unlock()
	if gotHits != 1 {
		t.Fatalf("transport hit %d times, want 1 (the row whose lease was lost must not be sent)", gotHits)
	}
	if _, ok := st.results[2]; ok {
		t.Fatal("delivery 2 was finalized despite a lost lease")
	}
	if !st.results[1] {
		t.Fatalf("delivery 1 result = %v, want success", st.results[1])
	}
}
