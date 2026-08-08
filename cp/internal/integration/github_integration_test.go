package integration

// GitHub as an org-level integration: claim once, then derive per-project
// connections by selecting a repo.

import (
	"context"
	"errors"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestGitHubIntegrationClaimAndList(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	inst, err := st.ClaimInstallationWithMeta(ctx, "org_a", "12345", "acme-corp", "Organization", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if inst.AccountLogin != "acme-corp" || inst.AccountType != "Organization" {
		t.Fatalf("installation meta = %+v", inst)
	}

	list, err := st.ListOrgInstallations(ctx, "org_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].InstallationID != "12345" {
		t.Fatalf("installations = %+v", list)
	}

	// Re-claiming by the owner refreshes metadata rather than erroring — the
	// post-install callback can land more than once.
	again, err := st.ClaimInstallationWithMeta(ctx, "org_a", "12345", "acme-renamed", "Organization", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if again.AccountLogin != "acme-renamed" {
		t.Fatalf("re-claim did not refresh metadata: %+v", again)
	}

	// Another org cannot claim it, and gets an opaque not-found so the response
	// never confirms someone else's installation exists (SIGMA-87).
	if _, err := st.ClaimInstallationWithMeta(ctx, "org_b", "12345", "evil", "User", "attacker"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-org claim err = %v, want ErrNotFound", err)
	}
	// ...and the original binding is untouched.
	list, _ = st.ListOrgInstallations(ctx, "org_a")
	if len(list) != 1 || list[0].AccountLogin != "acme-renamed" {
		t.Fatalf("cross-org claim damaged the owner's row: %+v", list)
	}
	if other, _ := st.ListOrgInstallations(ctx, "org_b"); len(other) != 0 {
		t.Fatalf("org_b must own nothing, got %+v", other)
	}
}

func TestEnsureGitConnectionIsIdempotent(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_pick"

	if _, err := st.ClaimInstallationWithMeta(ctx, orgID, "999", "acme", "Organization", "admin"); err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Selecting a repo derives the connection, inheriting the org's single
	// installation without the caller naming it.
	first, err := st.EnsureGitConnection(ctx, orgID, store.EnsureGitConnectionInput{
		ProjectID:    proj.ID,
		RepoFullName: "acme/api",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if first.InstallationID != "999" {
		t.Fatalf("installation not inherited from the org integration: %+v", first)
	}
	if first.RepoFullName != "acme/api" {
		t.Fatalf("repo = %q", first.RepoFullName)
	}

	// A second resource on the same repo reuses the connection instead of
	// failing the once-per-org uniqueness rule.
	second, err := st.EnsureGitConnection(ctx, orgID, store.EnsureGitConnectionInput{
		ProjectID:    proj.ID,
		RepoFullName: "acme/api",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("selecting the same repo made a second connection: %s vs %s", second.ID, first.ID)
	}
}

func TestDisconnectIntegrationGuardsLiveConnections(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_disc"

	if _, err := st.ClaimInstallationWithMeta(ctx, orgID, "555", "acme", "Organization", "admin"); err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, orgID, "web", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureGitConnection(ctx, orgID, store.EnsureGitConnectionInput{
		ProjectID:    proj.ID,
		RepoFullName: "acme/api",
	}, "admin"); err != nil {
		t.Fatal(err)
	}

	// Refuses while repos still deploy through it — severing push-to-deploy for
	// the whole org must never be a silent side effect of one click.
	err = st.DeleteOrgInstallation(ctx, orgID, "555", "admin", false)
	var inUse store.ErrIntegrationInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("unforced disconnect err = %v, want ErrIntegrationInUse", err)
	}
	if inUse.Connections != 1 {
		t.Fatalf("in-use count = %d, want 1", inUse.Connections)
	}

	// Forced: the integration goes, the connection survives without a binding
	// (its repo simply stops auto-deploying) rather than being deleted.
	if err := st.DeleteOrgInstallation(ctx, orgID, "555", "admin", true); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListOrgInstallations(ctx, orgID); len(list) != 0 {
		t.Fatalf("installation survived a forced disconnect: %+v", list)
	}
	conns, err := st.ListGitConnections(ctx, orgID, proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 1 {
		t.Fatalf("connections = %d, want the connection kept", len(conns))
	}
	if conns[0].InstallationID != "" {
		t.Fatalf("connection kept a dangling installation: %+v", conns[0])
	}

	// Disconnecting something this org doesn't own is an opaque not-found.
	if err := st.DeleteOrgInstallation(ctx, orgID, "424242", "admin", true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown installation err = %v, want ErrNotFound", err)
	}
}
