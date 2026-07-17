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
	ctx := context.Background()
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, name, "general", "", "", "test", time.Hour)
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
	if summary.Connected != 4 || summary.BillableServers != 1 {
		t.Fatalf("summary counts = connected %d billable %d", summary.Connected, summary.BillableServers)
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
	if small.BillableServers != 0 || small.Amount != 0 {
		t.Fatalf("free-tier org billed: %+v", small)
	}
}

func TestBillingSubscriptionWebhookFlow(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_sub"

	// A payment-failed alert channel to prove the past_due enqueue.
	ch, err := st.CreateAlertChannel(ctx, orgID, "admin", store.CreateAlertChannelInput{
		Kind: "slack", Name: "ops", Secret: "https://hooks.example/x",
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
