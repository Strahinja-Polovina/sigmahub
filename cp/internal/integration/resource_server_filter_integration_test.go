package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestListResourcesFiltersByServer is the SIGMA-328 regression.
//
// The server detail page needs one server's hosted resources. Before this fix
// the only filter ListResources understood was environment, so the page asked
// for every resource in the org — full `spec` jsonb and all — and threw away
// the ~98% that belonged to other servers. In an org with 2,000 resources
// across 40 servers that is megabytes over the wire per page view, and it grows
// every time anyone in the org creates a resource, including in projects the
// viewer cannot see.
//
// The rule this test keeps: a server-scoped list returns that server's
// resources and nothing else, and the environment filter still composes with
// it.
func TestListResourcesFiltersByServer(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_resfilter"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateEnvironment(ctx, orgID, proj.ID, "staging", false, "test")
	if err != nil {
		t.Fatal(err)
	}

	newServer := func(host string) string {
		t.Helper()
		bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, host, "general", "", "", "test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := st.RegisterServer(ctx, bootTok, host, "0.1.0", json.RawMessage(`{}`), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AttachServer(ctx, orgID, env.ID, reg.Server.ID, "test"); err != nil {
			t.Fatal(err)
		}
		if err := st.AttachServer(ctx, orgID, other.ID, reg.Server.ID, "test"); err != nil {
			t.Fatal(err)
		}
		return reg.Server.ID
	}

	serverA := newServer("host-a")
	serverB := newServer("host-b")

	appSpec, _ := json.Marshal(map[string]any{"image": "nginx"})
	mk := func(envID, serverID, name string) {
		t.Helper()
		if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
			EnvironmentID: envID, ServerID: serverID, Name: name, Kind: "app", Spec: appSpec,
		}, "test"); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	// Server A carries the crowd; server B carries the one row the page wants.
	for i := 0; i < 12; i++ {
		mk(env.ID, serverA, "a-"+string(rune('a'+i)))
	}
	mk(env.ID, serverB, "b-prod")
	mk(other.ID, serverB, "b-staging")

	// Server-scoped, no environment filter: exactly B's two resources.
	got, err := st.ListResources(ctx, orgID, "", serverB)
	if err != nil {
		t.Fatalf("list by server: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListResources(server=%s) returned %d rows, want 2: the org's other "+
			"servers' resources are still being shipped to the server detail page (SIGMA-328)",
			serverB, len(got))
	}
	for _, r := range got {
		if r.ServerID != serverB {
			t.Errorf("resource %s is bound to server %s, not %s", r.Name, r.ServerID, serverB)
		}
	}

	// The two filters compose: B's production resource only.
	got, err = st.ListResources(ctx, orgID, env.ID, serverB)
	if err != nil {
		t.Fatalf("list by env+server: %v", err)
	}
	if len(got) != 1 || got[0].Name != "b-prod" {
		t.Fatalf("ListResources(env=prod, server=B) = %d rows %+v, want just b-prod", len(got), got)
	}

	// No server filter still means the whole org, as every other caller expects.
	all, err := st.ListResources(ctx, orgID, "", "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 14 {
		t.Fatalf("unfiltered ListResources = %d rows, want 14", len(all))
	}
}
