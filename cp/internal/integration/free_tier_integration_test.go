package integration

// SIGMA-363: the free tier is finite.
//
// The asymmetry this closes: an org that subscribed and then let its card lapse
// was capped after a grace period, while an org that never entered a card at all
// was capped by nothing at all — so a 40-unit fleet ran indefinitely for free and
// the rational move for every customer was to never subscribe. The meter, the
// dashboard's "Total due" and the quantity sweep all existed; nothing read them
// before handing out more capacity.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestFreeTierCapsGrowthWithoutASubscription(t *testing.T) {
	st, _ := testStore(t)
	st.SetBillingConfigured(true) // hosted deployment: there is a way to pay
	ctx := context.Background()
	orgID := "org_freetier"

	// The free tier is a product: every unit of it is handed out without a card.
	for i := 0; i < store.BillingFreeTier; i++ {
		connectServer(t, st, orgID, fmt.Sprintf("free%d", i))
	}

	// The unit after it is not. Growth is refused with the billing error the API
	// turns into a 402 naming the portal, not a generic failure.
	_, _, _, err := st.IssueBootstrapToken(ctx, orgID, "one-too-many", "general", "", "", "admin", time.Hour)
	var capped store.ErrBillingCapped
	if !errors.As(err, &capped) {
		t.Fatalf("a free-tier org at its ceiling must be refused growth, got %T: %v", err, err)
	}

	// Nothing running is touched — this is a cap on growth, exactly like the
	// delinquency cap. The fleet is still connected and still metered.
	_, _, units, uerr := st.ConnectedServerUnits(ctx, orgID)
	if uerr != nil || units != store.BillingFreeTier {
		t.Fatalf("the existing free fleet must keep running: units=%d err=%v", units, uerr)
	}

	// And resources on the servers it already has are unaffected: the free tier
	// gives away SERVER units, and gating apps on it would make it useless
	// rather than finite.
	envID, serverID := dbTestFixture(t, st, "org_freetier_res", true, "general")
	for i := 0; i < store.BillingFreeTier+3; i++ {
		if _, err := st.CreateResource(ctx, "org_freetier_res", store.CreateResourceInput{
			EnvironmentID: envID, ServerID: serverID,
			Name: fmt.Sprintf("app%d", i), Kind: "app",
			Spec: []byte(`{"image":"nginx"}`),
		}, "admin"); err != nil {
			t.Fatalf("free-tier orgs must still create resources on the servers they have: %v", err)
		}
	}
}

func TestSubscribingLiftsTheFreeTierCap(t *testing.T) {
	st, _ := testStore(t)
	st.SetBillingConfigured(true)
	ctx := context.Background()
	orgID := "org_freetier_paid"

	for i := 0; i < store.BillingFreeTier; i++ {
		connectServer(t, st, orgID, fmt.Sprintf("free%d", i))
	}
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "blocked", "general", "", "", "admin", time.Hour); err == nil {
		t.Fatal("precondition: the org should be at its free ceiling")
	}

	// A live subscription pays for growth, so the ceiling lifts immediately.
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_f", SubscriptionID: "sub_f", Status: "active", Quantity: 5,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "now-fine", "general", "", "", "admin", time.Hour); err != nil {
		t.Fatalf("an active subscription must lift the free-tier cap: %v", err)
	}
}

// The conversion funnel must not be a dead end (SIGMA-365). The first cut of the
// ceiling engaged at units >= 3 while checkout required units >= 4, so an org
// with exactly three general servers — the shape the free tier advertises, and
// where every organically growing org lands — was refused growth AND shown a
// disabled Subscribe button reading "Within free tier". It could neither grow nor
// pay, with no in-product escape, at the single moment the paywall exists to
// create a customer.
func TestFreeTierCapIsNotADeadEnd(t *testing.T) {
	st, _ := testStore(t)
	st.SetBillingConfigured(true)
	ctx := context.Background()
	orgID := "org_funnel"

	for i := 0; i < store.BillingFreeTier; i++ {
		connectServer(t, st, orgID, fmt.Sprintf("free%d", i))
	}
	// Capped, as intended.
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "blocked", "general", "", "", "admin", time.Hour); err == nil {
		t.Fatal("precondition: an org at the ceiling must be refused growth")
	}
	// And it can pay. The checkout opens a subscription at the minimum quantity —
	// the unit that takes the org past the free tier — so the 402's "start a
	// subscription from Settings → Billing" is a route that exists rather than a
	// sentence pointing at a disabled button.
	sum, err := st.BillingSummaryForOrg(ctx, orgID, time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if qty := store.BillableQuantity(sum.BilledUnits, true); qty < 1 {
		t.Fatalf("an org refused growth at the ceiling has nothing to buy "+
			"(checkout quantity=%d, billedUnits=%d): capped and unable to subscribe "+
			"is a dead end", qty, sum.BilledUnits)
	}

	// And once it has paid, it grows.
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_fn", SubscriptionID: "sub_fn", Status: "active", Quantity: 1,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "fourth", "general", "", "", "admin", time.Hour); err != nil {
		t.Fatalf("after subscribing, the refused server must be creatable: %v", err)
	}
}

// A deleted server frees its capacity immediately. The first cut read the 24h
// billing high-water mark, which exists to stop a heartbeat blip re-pricing a
// subscription — correct for the invoice, wrong for admission: an org that
// deleted a server was told it "is using all 3 free units" while running two, and
// could not replace it for a day. That is the rebuild path and the fumbled-
// onboarding path, both common at launch.
func TestFreeTierFreesCapacityWhenAServerGoes(t *testing.T) {
	st, _ := testStore(t)
	st.SetBillingConfigured(true)
	ctx := context.Background()
	orgID := "org_replace"

	var ids []string
	for i := 0; i < store.BillingFreeTier; i++ {
		ids = append(ids, connectServer(t, st, orgID, fmt.Sprintf("s%d", i)))
	}
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "blocked", "general", "", "", "admin", time.Hour); err == nil {
		t.Fatal("precondition: full fleet is capped")
	}
	// Delete one (soft delete, as the product does) and replace it at once.
	if _, err := st.Pool.Exec(ctx, `UPDATE servers SET deleted_at = now() WHERE id = $1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "replacement", "general", "", "", "admin", time.Hour); err != nil {
		t.Fatalf("a freed unit must be reusable immediately, not in 24h: %v", err)
	}
}

// The gate weighs the server being ADDED. Without that an org at 2 units adds a
// 4-unit GPU and sits at 6 units free forever — the straddle that made the
// ceiling advisory rather than binding.
func TestFreeTierCountsTheWeightOfTheServerBeingAdded(t *testing.T) {
	st, _ := testStore(t)
	st.SetBillingConfigured(true)
	ctx := context.Background()
	orgID := "org_straddle"

	connectServer(t, st, orgID, "one")
	connectServer(t, st, orgID, "two") // 2 units of 3 used

	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "gpu-straddle", "gpu", "", "", "admin", time.Hour); err == nil {
		t.Fatal("a 4-unit GPU must not slip under a 3-unit ceiling from 2 units used")
	}
	// A 1-unit server still fits exactly, because landing ON the tier is what the
	// free tier promises.
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "third", "general", "", "", "admin", time.Hour); err != nil {
		t.Fatalf("the third free unit must still be free: %v", err)
	}
}

// A delinquent org has ONE owner: assertBillingNotCappedTx, which gives it a
// 14-day grace period SigmaHub emails the customer as a promise. The free-tier
// gate must not cap it too — that nullifies the grace and explains it with "has
// no active subscription", which is false on both counts.
func TestDelinquentOrgKeepsItsGracePeriod(t *testing.T) {
	st, _ := testStore(t)
	st.SetBillingConfigured(true)
	ctx := context.Background()
	orgID := "org_grace"

	for i := 0; i < store.BillingFreeTier; i++ {
		connectServer(t, st, orgID, fmt.Sprintf("g%d", i))
	}
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_g", SubscriptionID: "sub_g", Status: "past_due", Quantity: 1,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	// Inside the grace window: nothing is paused, exactly as the dunning email says.
	if _, _, _, err := st.IssueBootstrapToken(ctx, orgID, "during-grace", "general", "", "", "admin", time.Hour); err != nil {
		t.Fatalf("a past_due org inside its grace period must still grow: %v", err)
	}
}

func TestUnbilledOrgsSurfacesOverTierUsage(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	now := time.Now()

	// Over the tier with no subscription: invisible to DelinquentOrgs, which
	// filters to past_due/canceled, so this was the one shape of unpaid usage no
	// operator view showed at all.
	//
	// Reached the way an org can still drift over the ceiling now that growth is
	// capped: not by adding a server, but by one it already has getting heavier.
	// A `gpu` host weighs 4 units against `general`'s 1, so retyping a single
	// server of a full free fleet puts the org at 6 — no server created, no
	// subscription, well past the giveaway. (Legacy orgs from before the cap and
	// orgs that grew then cancelled land in the same report by the same query.)
	over := "org_unbilled"
	var overServers []string
	for i := 0; i < store.BillingFreeTier; i++ {
		overServers = append(overServers, connectServer(t, st, over, fmt.Sprintf("s%d", i)))
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE servers SET type = 'gpu' WHERE id = $1`, overServers[0]); err != nil {
		t.Fatal(err)
	}
	// At the tier: using the product as offered, not a report entry.
	under := "org_within_free"
	connectServer(t, st, under, "only")

	// Paying: also not a report entry, whatever its size. Subscribed first, then
	// grown — which is now the only way to get big, and the point of the change.
	paying := "org_paying_big"
	if err := st.UpsertSubscription(ctx, paying, store.BillingStatus{
		OrgID: paying, CustomerID: "ctm_b", SubscriptionID: "sub_b", Status: "active", Quantity: 4,
	}, "paddle-webhook"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < store.BillingFreeTier+2; i++ {
		connectServer(t, st, paying, fmt.Sprintf("p%d", i))
	}

	rows, err := st.UnbilledOrgs(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]store.DelinquentOrg{}
	for _, r := range rows {
		seen[r.OrgID] = r
	}
	got, ok := seen[over]
	if !ok {
		t.Fatalf("an org over the free tier with no subscription must be reported; got %+v", rows)
	}
	if got.Units <= store.BillingFreeTier || got.MonthlyValue <= 0 {
		t.Fatalf("the report must carry the revenue at stake: %+v", got)
	}
	if _, bad := seen[under]; bad {
		t.Error("an org inside the free tier is a customer using the product as offered, not unbilled usage")
	}
	if _, bad := seen[paying]; bad {
		t.Error("an org with an active subscription must never appear as unbilled")
	}
}
