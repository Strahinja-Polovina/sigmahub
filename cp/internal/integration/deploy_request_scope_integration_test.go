package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestListDeployRequestsScopedToConnection is the SIGMA-330 regression.
//
// The project page's "Recent pushes" panel exists to answer one question — "I
// pushed, why is nothing happening?" (migration 0052 spells this out). It got
// its rows by asking for the org's deploy requests, unfiltered, which
// ListDeployRequests caps at 50, and then filtering that shared window down to
// this project's connections in the browser-facing render path.
//
// In an org with several active repositories that window belongs to whoever
// pushed most. Project A's CI pushes 60 times in an afternoon; an operator
// opens project B, which they pushed to twenty minutes ago, and the panel is
// empty — because B's row fell outside a window it has to share with A. The
// panel then answers the question with silence, which reads exactly like "the
// webhook never arrived".
//
// The rule this test keeps: a connection-scoped list is scoped in SQL, so a
// connection's own history is never hidden by another repository's volume.
func TestListDeployRequestsScopedToConnection(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_dpr_scope"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	connA, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, RepoFullName: "acme/busy",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	connB, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, RepoFullName: "acme/quiet",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// B pushed first; then A's CI ran 60 times, burying it well past the
	// org-wide 50-row window.
	insert := func(id, connID string, at time.Time) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, `
			INSERT INTO deploy_requests (id, org_id, connection_id, kind, ref, sha, branch, status, created_at)
			VALUES ($1,$2,$3,'deploy','refs/heads/main',$1,'main','queued',$4)`,
			id, orgID, connID, at); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	base := time.Now().Add(-2 * time.Hour)
	insert("dpr_b_0001", connB.ID, base)
	for i := 0; i < 60; i++ {
		insert(fmt.Sprintf("dpr_a_%04d", i), connA.ID, base.Add(time.Duration(i+1)*time.Minute))
	}

	scoped, err := st.ListDeployRequests(ctx, orgID, connB.ID, 5)
	if err != nil {
		t.Fatalf("list scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != "dpr_b_0001" {
		t.Fatalf("ListDeployRequests(conn=B) = %d rows %+v, want just dpr_b_0001: the "+
			"busy repo's 60 pushes are still hiding this project's push, and the "+
			"Recent pushes panel renders empty (SIGMA-330)", len(scoped), scoped)
	}

	// The unscoped call is unchanged: the org's newest 50, newest first.
	all, err := st.ListDeployRequests(ctx, orgID, "", 0)
	if err != nil {
		t.Fatalf("list org: %v", err)
	}
	if len(all) != 50 {
		t.Fatalf("unscoped ListDeployRequests = %d rows, want the 50-row default window", len(all))
	}
	if all[0].ID != "dpr_a_0059" {
		t.Fatalf("unscoped newest row = %s, want dpr_a_0059", all[0].ID)
	}
	for _, d := range all {
		if d.ConnectionID == connB.ID {
			t.Fatalf("B's push is inside the org window; the fixture no longer reproduces "+
				"the failure it exists to describe (got %+v)", d)
		}
	}
}
