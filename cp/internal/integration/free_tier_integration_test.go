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
