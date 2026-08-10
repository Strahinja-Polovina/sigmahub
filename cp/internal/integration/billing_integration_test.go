package integration

// P2-4 billing integration: the server-hours meter, the usage+charge summary
// (free tier, billable count), subscription webhook idempotency + past_due
// alert, and honest-off.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// SIGMA-293: an event with no custom_data is still correlatable — either
	// stored Paddle id identifies the org, and an unknown id resolves to no org
	// rather than to some other tenant's row.
	for _, tc := range []struct{ sub, ctm, want string }{
		{"sub_1", "", orgID},
		{"", "ctm_1", orgID},
		{"sub_unknown", "ctm_1", orgID}, // falls through to the customer id
		{"sub_unknown", "ctm_unknown", ""},
		{"", "", ""},
	} {
		found, err := st.OrgForPaddleIDs(ctx, tc.sub, tc.ctm)
		if err != nil {
			t.Fatal(err)
		}
		if found != tc.want {
			t.Fatalf("OrgForPaddleIDs(%q, %q) = %q, want %q", tc.sub, tc.ctm, found, tc.want)
		}
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

// TestPastDueOrgIsCappedAfterGracePeriod is the SIGMA-295 guard. A past_due
// transition enqueued exactly one alert and nothing else ever happened: no
// repeat, no canceled alert, no grace-period expiry, and no code path anywhere
// consulted org_billing.status before creating a server. A customer running a
// 40-unit fleet could stop paying and keep every capability indefinitely.
func TestPastDueOrgIsCappedAfterGracePeriod(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// A paying org that stopped paying, with a fleet already running.
	orgID := "org_delinquent"
	connectServer(t, st, orgID, "existing-1")
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_d", SubscriptionID: "sub_d", Status: "past_due", Quantity: 5,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}

	// Inside the grace period nothing changes: the whole promise of the
	// past_due alert is "your servers keep running, nothing is paused".
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "grace-ok", "general", "", "", "admin", time.Hour); err != nil {
		t.Fatalf("a past_due org inside its grace period must keep working: %v", err)
	}

	// Now push the past_due transition beyond the grace window.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE org_billing SET status_since = $2 WHERE org_id = $1`,
		orgID, now.Add(-store.BillingGracePeriod-24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := st.IssueBootstrapToken(ctx, orgID, "new-server", "general", "", "", "admin", time.Hour)
	if err == nil {
		t.Fatal("an org past_due beyond the grace period was allowed to add a server")
	}
	var capped store.ErrBillingCapped
	if !errors.As(err, &capped) {
		t.Fatalf("refusal must be a billing refusal the API can explain, got %T: %v", err, err)
	}

	// The existing fleet is untouched: this is a cap on GROWTH, not a shutdown.
	if _, _, units, uerr := st.ConnectedServerUnits(ctx, orgID); uerr != nil || units == 0 {
		t.Fatalf("existing fleet must keep running: units=%d err=%v", units, uerr)
	}

	// A paying org is unaffected.
	paying := "org_paying"
	if err := st.UpsertSubscription(ctx, paying, store.BillingStatus{
		OrgID: paying, CustomerID: "ctm_p", SubscriptionID: "sub_p", Status: "active", Quantity: 5,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.IssueBootstrapToken(ctx, paying, "fine", "general", "", "", "admin", time.Hour); err != nil {
		t.Fatalf("an active subscription must not be capped: %v", err)
	}
	// So is an org that never subscribed — the free tier is a product, not a
	// delinquency.
	if _, _, _, err := st.IssueBootstrapToken(ctx, "org_free", "free", "general", "", "", "admin", time.Hour); err != nil {
		t.Fatalf("a free-tier org must not be capped: %v", err)
	}

	// The operator can see it, and the org is told again rather than once.
	del, err := st.DelinquentOrgs(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.DelinquentOrg
	for i := range del {
		if del[i].OrgID == orgID {
			found = &del[i]
		}
	}
	if found == nil {
		t.Fatalf("delinquent org missing from the operator view: %+v", del)
	}
	if !found.Capped || found.Status != "past_due" {
		t.Fatalf("delinquent row = %+v, want capped past_due", *found)
	}
}

// TestBillingDunningRepeatsAndAlertsOnCancel covers the rest of SIGMA-295: one
// alert, ever, was the entire dunning sequence, and a cancellation produced no
// alert at all.
func TestBillingDunningRepeatsAndAlertsOnCancel(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	orgID := "org_dunning"
	ch, err := st.CreateAlertChannel(ctx, orgID, "admin", store.CreateAlertChannelInput{
		// SIGMA-259 pins Slack channels to the hooks.slack.com prefix at create
		// time, so a placeholder host never becomes the row this test alerts on.
		Kind: "slack", Name: "ops", Secret: "https://hooks.slack.com/services/T000/B000/dunning",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAlertRules(ctx, orgID, ch.ID, []string{store.AlertPaymentFailed}, "admin"); err != nil {
		t.Fatal(err)
	}

	countAlerts := func() int {
		var n int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*) FROM alert_outbox WHERE org_id = $1 AND event = $2`,
			orgID, store.AlertPaymentFailed).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_x", SubscriptionID: "sub_x", Status: "past_due", Quantity: 4,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	if got := countAlerts(); got != 1 {
		t.Fatalf("transition alerts = %d, want 1", got)
	}

	// A sweep run immediately must NOT re-alert — the reminder is on a schedule,
	// not on every 10-minute pass.
	if _, err := st.SweepBillingDunning(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got := countAlerts(); got != 1 {
		t.Fatalf("immediate re-sweep alerted again: %d", got)
	}

	// A sweep one dunning interval later must remind them.
	if _, err := st.SweepBillingDunning(ctx, now.Add(store.BillingDunningInterval+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := countAlerts(); got != 2 {
		t.Fatalf("dunning reminder never fired: alerts = %d, want 2", got)
	}

	// A cancellation is an event the org must hear about too.
	cancelOrg := "org_canceled"
	cch, err := st.CreateAlertChannel(ctx, cancelOrg, "admin", store.CreateAlertChannelInput{
		Kind: "slack", Name: "ops", Secret: "https://hooks.slack.com/services/T000/B000/cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetAlertRules(ctx, cancelOrg, cch.ID, []string{store.AlertPaymentFailed}, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSubscription(ctx, cancelOrg, store.BillingStatus{
		OrgID: cancelOrg, CustomerID: "ctm_c", SubscriptionID: "sub_c", Status: "active", Quantity: 4,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertSubscription(ctx, cancelOrg, store.BillingStatus{
		OrgID: cancelOrg, CustomerID: "ctm_c", SubscriptionID: "sub_c", Status: "canceled", Quantity: 4,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_outbox WHERE org_id = $1 AND event = $2`,
		cancelOrg, store.AlertPaymentFailed).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cancellation alerts = %d, want 1", n)
	}
}

// TestBillableQuantityAgreesAcrossSummaryAndSync is the SIGMA-292 guard: the
// number the Billing page shows, the number handleBillingCheckout puts on the
// Paddle transaction and the number the drift sweep PATCHes onto the
// subscription must be the SAME number. They used to be computed by two
// different formulas over two different windows with two different floors, so
// an org that shrank in the morning approved one quantity at checkout and was
// invoiced another within ten minutes.
func TestBillableQuantityAgreesAcrossSummaryAndSync(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// (a) A fleet that shrank this morning. 20 general servers ran overnight and
	// were metered; 15 were deleted before the org subscribed in the afternoon.
	orgID := "org_shrunk"
	var ids []string
	for i := 0; i < 20; i++ {
		ids = append(ids, connectServer(t, st, orgID, fmt.Sprintf("shrunk-%d", i)))
	}
	if _, err := st.SweepServerHours(ctx, now.Add(-6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[:15] {
		if err := st.DeleteServer(ctx, orgID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}

	summary, err := st.BillingSummaryForOrg(ctx, orgID, now, true)
	if err != nil {
		t.Fatal(err)
	}
	// The customer subscribes at exactly the quantity checkout showed them.
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_shrunk", SubscriptionID: "sub_shrunk",
		Status: "active", Quantity: summary.BillableUnits,
	}, "checkout"); err != nil {
		t.Fatal(err)
	}

	drift, err := st.SubscriptionsNeedingQuantitySync(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range drift {
		if d.OrgID != orgID {
			continue
		}
		t.Fatalf("sweep wants to re-price a subscription the customer just approved: "+
			"summary/checkout showed %d billable units, sweep wants %d",
			summary.BillableUnits, d.Want)
	}

	// (b) An org that shrank BELOW the free tier. The page must not say "nothing
	// due" while the sweep keeps the subscription at a quantity Paddle invoices.
	small := "org_shrunk_small"
	smallIDs := []string{
		connectServer(t, st, small, "small-1"),
		connectServer(t, st, small, "small-2"),
		connectServer(t, st, small, "small-3"),
		connectServer(t, st, small, "small-4"),
	}
	if _, err := st.SweepServerHours(ctx, now.Add(-30*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, id := range smallIDs[:3] {
		if err := st.DeleteServer(ctx, small, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertSubscription(ctx, small, store.BillingStatus{
		OrgID: small, CustomerID: "ctm_small", SubscriptionID: "sub_small",
		Status: "active", Quantity: 1,
	}, "checkout"); err != nil {
		t.Fatal(err)
	}
	smallSummary, err := st.BillingSummaryForOrg(ctx, small, now, true)
	if err != nil {
		t.Fatal(err)
	}
	smallDrift, err := st.SubscriptionsNeedingQuantitySync(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	want := 1 // nothing drifted → Paddle keeps invoicing the stored quantity
	for _, d := range smallDrift {
		if d.OrgID == small {
			want = d.Want
		}
	}
	if smallSummary.BillableUnits != want {
		t.Fatalf("below the free tier the page shows %d billable units (%d %s due) "+
			"while the subscription is billed for %d",
			smallSummary.BillableUnits, smallSummary.Amount, smallSummary.Currency, want)
	}
}
