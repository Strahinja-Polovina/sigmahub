package integration

// SIGMA-335: the org switcher's server count, against real Postgres.
//
// The dashboard layout renders one integer per org the user belongs to, and it
// used to get each one by fetching that org's full server list and calling
// .length on it — which makes the store build serverSelect's whole dashboard
// projection (every column, the facts jsonb blob and a correlated readiness
// subquery per row) so the web can throw all of it away.
//
// The replacement is a COUNT, and the property that matters about it is not
// the arithmetic but the VISIBILITY rule: it has to agree with ListServers
// exactly, or the switcher starts contradicting the page it links to. A
// tombstoned server is the case where the two can drift — soft delete keeps the
// row (and its mesh IP) forever — so it is the case this test is built around,
// and it can only be exercised against the real schema.

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestCountServersMatchesListServers(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	const orgID = "org_count"
	const otherOrgID = "org_count_other"
	var ids []string
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("count-host-%d", i)
		tok, _, _, err := st.IssueBootstrapToken(ctx, orgID, name, "general", "", "", "test", time.Hour)
		if err != nil {
			t.Fatalf("bootstrap token %d: %v", i, err)
		}
		res, err := st.RegisterServer(ctx, tok, name, "0.1.0", json.RawMessage(`{}`), "")
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		ids = append(ids, res.Server.ID)
	}
	// A neighbour's fleet, so a count that forgot its org_id predicate shows up
	// as a wrong number rather than as a coincidence.
	otherTok, _, _, err := st.IssueBootstrapToken(ctx, otherOrgID, "rival-host", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterServer(ctx, otherTok, "rival-host", "0.1.0", json.RawMessage(`{}`), ""); err != nil {
		t.Fatal(err)
	}

	assertAgrees := func(when string) {
		t.Helper()
		list, err := st.ListServers(ctx, orgID)
		if err != nil {
			t.Fatalf("%s: list: %v", when, err)
		}
		n, err := st.CountServers(ctx, orgID)
		if err != nil {
			t.Fatalf("%s: count: %v", when, err)
		}
		if n != len(list) {
			t.Fatalf("%s: count = %d, list = %d — the switcher and the page disagree", when, n, len(list))
		}
	}

	assertAgrees("after registration")
	if n, _ := st.CountServers(ctx, orgID); n != 4 {
		t.Fatalf("count = %d, want 4", n)
	}

	// Force-disconnect one host: the row survives as a tombstone, and neither
	// reader may count it.
	if err := st.DeleteServer(ctx, orgID, ids[0], "test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertAgrees("after a force disconnect")
	if n, _ := st.CountServers(ctx, orgID); n != 3 {
		t.Fatalf("count after tombstone = %d, want 3", n)
	}

	// An org with nothing in it answers 0, not an error.
	if n, err := st.CountServers(ctx, "org_count_empty"); err != nil || n != 0 {
		t.Fatalf("empty org count = %d, err = %v", n, err)
	}
}
