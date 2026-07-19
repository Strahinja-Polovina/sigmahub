package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestDSDReDrivesOnOpFailure covers SIGMA-104: a DSD whose ops the agent reported
// as FAILED must be re-issued by the resync (same ops, a new version) so the
// failed op is retried, rather than being treated as converged forever because
// the rendered doc hash is unchanged.
func TestDSDReDrivesOnOpFailure(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	rec := reconciler.New(slog.New(slog.NewTextHandler(io.Discard, nil)), st, dsdKey)

	orgID := "org_redrive"
	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID := reg.Server.ID
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "test"); err != nil {
		t.Fatal(err)
	}
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "app1", Kind: "app", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.CurrentDSDVersion(ctx, serverID); v != 1 {
		t.Fatalf("first reconcile version = %d, want 1", v)
	}

	// The agent reports the resource op FAILED for v1.
	failed := map[string]json.RawMessage{"res:" + res.ID: json.RawMessage(`{"state":"failed","error":"registry unreachable"}`)}
	if ok, err := st.ApplyDSDStatus(ctx, serverID, 1, failed); err != nil || !ok {
		t.Fatalf("apply failed status: ok=%v err=%v", ok, err)
	}

	// A resync with UNCHANGED specs must RE-ISSUE (v2): a failed op is not
	// converged, so the same ops are delivered again for the agent to retry.
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.CurrentDSDVersion(ctx, serverID); v != 2 {
		t.Fatalf("re-drive version = %d, want 2 (a failed op must be retried)", v)
	}

	// The agent now reports success for v2 → converged; a further resync with the
	// same specs must NOT bump the version.
	okStatus := map[string]json.RawMessage{"res:" + res.ID: json.RawMessage(`{"state":"applied"}`)}
	if ok, err := st.ApplyDSDStatus(ctx, serverID, 2, okStatus); err != nil || !ok {
		t.Fatalf("apply ok status: ok=%v err=%v", ok, err)
	}
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.CurrentDSDVersion(ctx, serverID); v != 2 {
		t.Fatalf("converged resync bumped version to %d, want 2", v)
	}
}

// TestSweepAndResyncIgnoreDeletedServers covers SIGMA-107 and SIGMA-112: a
// soft-deleted (tombstoned) server must be excluded from the periodic resync's
// server list AND must never be flipped to 'unreachable' by the staleness
// sweeper (which would fire a spurious alert + audit for a server the operator
// already deleted).
func TestSweepAndResyncIgnoreDeletedServers(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_del"

	reganon := func(name string) store.RegisterResult {
		bt, _, _, err := st.IssueBootstrapToken(ctx, orgID, name, "general", "", "", "test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := st.RegisterServer(ctx, bt, name, "0.1.0", json.RawMessage(`{}`), "")
		if err != nil {
			t.Fatal(err)
		}
		return reg
	}
	live := reganon("live").Server.ID
	dead := reganon("dead").Server.ID

	// Precondition both as running-but-stale, then tombstone `dead`.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE servers SET status='running', last_seen_at = now() - interval '1 hour' WHERE id = ANY($1)`,
		[]string{live, dead}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServer(ctx, orgID, dead, "admin"); err != nil {
		t.Fatalf("delete server: %v", err)
	}

	// The staleness sweeper must flip only the live server.
	flipped, err := st.MarkStaleUnreachable(ctx, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if flipped != 1 {
		t.Fatalf("flipped = %d, want 1 (only the live server)", flipped)
	}
	var deadStatus string
	if err := st.Pool.QueryRow(ctx, `SELECT status FROM servers WHERE id = $1`, dead).Scan(&deadStatus); err != nil {
		t.Fatal(err)
	}
	if deadStatus == "unreachable" {
		t.Fatal("tombstoned server was flipped to unreachable (spurious alert would fire)")
	}
	var alertsForDead int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_outbox WHERE dedup_key = 'srv:' || $1 || ':unreachable'`, dead).Scan(&alertsForDead); err != nil {
		t.Fatal(err)
	}
	if alertsForDead != 0 {
		t.Fatalf("tombstoned server produced %d unreachable alerts, want 0", alertsForDead)
	}

	// The resync server list must exclude the tombstoned server.
	ids, err := st.AllServerIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sawLive, sawDead := false, false
	for _, s := range ids {
		if s.ServerID == live {
			sawLive = true
		}
		if s.ServerID == dead {
			sawDead = true
		}
	}
	if !sawLive {
		t.Fatal("AllServerIDs dropped the live server")
	}
	if sawDead {
		t.Fatal("AllServerIDs returned a tombstoned server (resync would churn on it)")
	}
}

// TestBillingPreservesSubscriptionIDOnEmpty covers the SIGMA-103 store-side guard:
// an upsert carrying an empty subscription id (as a transaction.* event would,
// once the API maps it correctly) must NOT blank the stored subscription id.
func TestBillingPreservesSubscriptionIDOnEmpty(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_bill_subid"

	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_1", SubscriptionID: "sub_123", Status: "active", Quantity: 3,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	// A later event with an empty subscription id (e.g. a transaction without a
	// resolvable subscription_id) updates status but must keep paddle_subscription_id.
	if err := st.UpsertSubscription(ctx, orgID, store.BillingStatus{
		OrgID: orgID, CustomerID: "ctm_1", SubscriptionID: "", Status: "past_due", Quantity: 3,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBillingStatus(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SubscriptionID != "sub_123" {
		t.Fatalf("subscription id = %q, want sub_123 (empty id must not blank it)", got.SubscriptionID)
	}
	if got.Status != "past_due" {
		t.Fatalf("status = %q, want past_due", got.Status)
	}
}
