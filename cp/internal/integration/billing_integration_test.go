package integration

// P2-4 billing integration: the server-hours meter, the usage+charge summary
// (free tier, billable count), subscription webhook idempotency + past_due
// alert, and honest-off.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func connectServer(t *testing.T, st *store.Store, orgID, name string) string {
	t.Helper()
	return connectTypedServer(t, st, orgID, name, "general")
}

// connectTypedServer enrolls a server of an explicit type — the billing weight
// keys off it, so unit tests need to vary it.
func connectTypedServer(t *testing.T, st *store.Store, orgID, name, serverType string) string {
	t.Helper()
	ctx := context.Background()
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, name, serverType, "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, name, "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	// RegisterServer leaves the server provisioning; a heartbeat flips it to
	// running (the billable "connected" state).
	if err := st.RecordHeartbeat(ctx, reg.Server.ID, store.HeartbeatInput{}); err != nil {
		t.Fatal(err)
	}
	return reg.Server.ID
}

func TestBillingMeterAndSummary(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_bill"

	// 4 connected servers: 3 free, 1 billable.
	for i, n := range []string{"s1", "s2", "s3", "s4"} {
		connectServer(t, st, orgID, n)
		_ = i
	}
	// The meter records one row per connected server for this hour.
	n, err := st.SweepServerHours(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("server-hours swept = %d, want 4", n)
	}
	// Idempotent within the hour.
	again, err := st.SweepServerHours(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("re-sweep same hour = %d, want 0", again)
	}

	summary, err := st.BillingSummaryForOrg(ctx, orgID, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	// All four are general servers → 4 units, 3 free, 1 billable. An all-general
	// fleet must price exactly as it did before units existed.
	if summary.Connected != 4 || summary.Units != 4 || summary.BillableUnits != 1 {
		t.Fatalf("summary counts = connected %d units %d billable %d",
			summary.Connected, summary.Units, summary.BillableUnits)
	}
	if summary.Amount != store.BillingUnitPrice || summary.Currency != "EUR" {
		t.Fatalf("summary charge = %d %s", summary.Amount, summary.Currency)
	}
	if summary.ServerHoursThisMonth != 4 {
		t.Fatalf("month server-hours = %d, want 4", summary.ServerHoursThisMonth)
	}
	if !summary.Configured {
		t.Fatal("configured flag must pass through")
	}
	// Honest-off: configured=false surfaces even with usage present.
	off, _ := st.BillingSummaryForOrg(ctx, orgID, time.Now(), false)
	if off.Configured {
		t.Fatal("configured=false must surface")
	}

	// Within the free tier → nothing billable.
	orgSmall := "org_small"
	connectServer(t, st, orgSmall, "only1")
	small, _ := st.BillingSummaryForOrg(ctx, orgSmall, time.Now(), true)
	if small.BillableUnits != 0 || small.Amount != 0 {
		t.Fatalf("free-tier org billed: %+v", small)
	}
}

// TestBillingUnitWeights covers the unit model end to end: a GPU server weighs
// four units, so one GPU box alone leaves the free tier while three ordinary
// servers do not, and the breakdown explains the total per type.
func TestBillingUnitWeights(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_units"

	// 2 general + 1 GPU = 2 + 4 = 6 units → 3 billable after the free tier.
	connectServer(t, st, orgID, "app-1")
	connectServer(t, st, orgID, "app-2")
	connectTypedServer(t, st, orgID, "gpu-1", "gpu")

	lines, servers, units, err := st.ConnectedServerUnits(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if servers != 3 || units != 6 {
		t.Fatalf("units = %d servers over %d units, want 3/6", servers, units)
	}
	byType := map[string]store.ServerUnitLine{}
	for _, l := range lines {
		byType[l.Type] = l
	}
	if g := byType["general"]; g.Count != 2 || g.Weight != 1 || g.Units != 2 {
		t.Fatalf("general line = %+v", g)
	}
	if g := byType["gpu"]; g.Count != 1 || g.Weight != 4 || g.Units != 4 {
		t.Fatalf("gpu line = %+v", g)
	}

	summary, err := st.BillingSummaryForOrg(ctx, orgID, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Connected != 3 || summary.Units != 6 || summary.BillableUnits != 3 {
		t.Fatalf("summary = connected %d units %d billable %d",
			summary.Connected, summary.Units, summary.BillableUnits)
	}
	if summary.Amount != 3*store.BillingUnitPrice {
		t.Fatalf("amount = %d, want %d", summary.Amount, 3*store.BillingUnitPrice)
	}

	// A lone GPU server already exceeds the 3-unit free tier — the most valuable
	// use case is never given away, which is the whole point of the weights.
	orgGPU := "org_gpu_only"
	connectTypedServer(t, st, orgGPU, "h100", "gpu")
	gpuOnly, err := st.BillingSummaryForOrg(ctx, orgGPU, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if gpuOnly.Connected != 1 || gpuOnly.Units != 4 || gpuOnly.BillableUnits != 1 {
		t.Fatalf("gpu-only summary = %+v", gpuOnly)
	}
}

func TestBillingSubscriptionWebhookFlow(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_sub"

	// A payment-failed alert channel to prove the past_due enqueue.
	ch, err := st.CreateAlertChannel(ctx, orgID, "admin", store.CreateAlertChannelInput{
		Kind: "slack", Name: "ops", Secret: "https://hooks.slack.com/services/T000/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = ch

	// Idempotency: the same delivery id applies once.
	first, err := st.WebhookSeen(ctx, "evt_abc", "paddle", "subscription.updated")
	if err != nil {
		t.Fatal(err)
	}
	if first {
		t.Fatal("first delivery must not be a duplicate")
	}
	dup, err := st.WebhookSeen(ctx, "evt_abc", "paddle", "subscription.updated")
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Fatal("redelivered id must be a duplicate")
	}

	// Active subscription.
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_1", SubscriptionID: "sub_1", Status: "active", Quantity: 2,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBillingStatus(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || got.SubscriptionID != "sub_1" || got.Quantity != 2 {
		t.Fatalf("subscription = %+v", got)
	}

	// past_due transition enqueues a payment_failed alert exactly once.
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_1", SubscriptionID: "sub_1", Status: "past_due", Quantity: 2,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	var alerts int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_outbox WHERE org_id = $1 AND event = 'payment_failed'`, orgID).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("payment_failed alerts = %d, want 1", alerts)
	}
	// Staying past_due doesn't re-alert (dedup key + no-transition guard).
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_1", SubscriptionID: "sub_1", Status: "past_due", Quantity: 2,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_outbox WHERE org_id = $1 AND event = 'payment_failed'`, orgID).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 {
		t.Fatalf("re-past_due must not re-alert, got %d", alerts)
	}
}

// TestBillingIgnoresOutOfOrderEvent is the SIGMA-99 guard: a delayed/retried
// OLDER Paddle delivery (distinct id, so not deduped) must not clobber newer
// subscription state.
func TestBillingIgnoresOutOfOrderEvent(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_order"
	t2 := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	t1 := t2.Add(-time.Hour)

	applied, err := st.ApplyPaddleWebhook(ctx, "evt_new", "paddle", "subscription.activated", orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm", SubscriptionID: "sub", Status: "active", Quantity: 1,
	}, "paddle-webhook", t2)
	if err != nil || !applied {
		t.Fatalf("apply newer event: applied=%v err=%v", applied, err)
	}
	// Older event arrives last — acknowledged (not a dup) but must NOT change state.
	if _, err := st.ApplyPaddleWebhook(ctx, "evt_old", "paddle", "subscription.past_due", orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm", SubscriptionID: "sub", Status: "past_due", Quantity: 1,
	}, "paddle-webhook", t1); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetBillingStatus(ctx, orgID)
	if got.Status != "active" {
		t.Fatalf("out-of-order older event clobbered state: status=%q, want active", got.Status)
	}
}

// TestIdempotencyClaimSemantics is the SIGMA-92 guard: a claim reserves the key
// before execution, a concurrent duplicate sees a pending (not-done) row, a
// finalized row replays, and a released pending row can be re-claimed.
func TestIdempotencyClaimSemantics(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	org, key := "org_idem", "k1"
	hashA := []byte("hashA")

	claimed, _, err := st.IdempotencyClaim(ctx, org, key, hashA)
	if err != nil || !claimed {
		t.Fatalf("first claim must win: claimed=%v err=%v", claimed, err)
	}
	// Concurrent duplicate loses and sees a PENDING row.
	claimed2, existing, err := st.IdempotencyClaim(ctx, org, key, hashA)
	if err != nil {
		t.Fatal(err)
	}
	if claimed2 || existing.Done {
		t.Fatalf("duplicate must lose and be pending: claimed=%v done=%v", claimed2, existing.Done)
	}
	// Finalize → a subsequent claim replays.
	if err := st.IdempotencyFinalize(ctx, org, key, 201, []byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	claimed3, done, err := st.IdempotencyClaim(ctx, org, key, hashA)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]bool
	// response is a JSONB column, so the bytes are re-canonicalised on read —
	// compare on the parsed value, not byte-for-byte.
	if claimed3 || !done.Done || done.StatusCode != 201 ||
		json.Unmarshal(done.Response, &payload) != nil || !payload["ok"] {
		t.Fatalf("after finalize expected replay of {ok:true}: claimed=%v resp=%s", claimed3, done.Response)
	}
	// Release must NOT delete a finalized row.
	if err := st.IdempotencyRelease(ctx, org, key); err != nil {
		t.Fatal(err)
	}
	if _, err := st.IdempotencyLookup(ctx, org, key); err != nil {
		t.Fatalf("release deleted a finalized row: %v", err)
	}
	// A pending claim that is released can be re-claimed (retry after a 5xx).
	org2, key2 := "org_idem2", "k2"
	if c, _, _ := st.IdempotencyClaim(ctx, org2, key2, hashA); !c {
		t.Fatal("claim k2")
	}
	if err := st.IdempotencyRelease(ctx, org2, key2); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := st.IdempotencyClaim(ctx, org2, key2, hashA); !c {
		t.Fatal("after release a retry must be able to re-claim")
	}
}
