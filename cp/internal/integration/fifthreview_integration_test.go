package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestManagedEngineAvoidsAppHostPort is the mesh-side half of SIGMA-355. The
// SIGMA-352 fix made the app resolver treat mesh ports as taken, but the mesh
// allocator (allocateMeshPort, shared by databases/object-store/LLM) still built
// its used-set from only the three mesh tables — blind to the explicit host ports
// an app publishes in spec.ports[]. Both share the range [MeshPortBase, 65535], so
// an app that claimed a mesh-range port, created first, would have that exact port
// handed to a database created afterward, and both bind it → the second container
// dies at deploy, the very collision SIGMA-352 set out to end.
func TestManagedEngineAvoidsAppHostPort(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_meshapp"
	envID, serverID := dbTestFixture(t, st, orgID, true, "general")

	// An app claims exactly MeshPortBase — the lowest port allocateMeshPort hands
	// out — before any managed engine exists, so it keeps it.
	appPort := int(store.MeshPortBase)
	spec := json.RawMessage(`{"image":"nginx","ports":[{"container":80,"host":` +
		fmt.Sprintf("%d", appPort) + `}]}`)
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "web", Kind: "app", Spec: spec,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var appHost int
	if err := st.Pool.QueryRow(ctx,
		`SELECT (spec->'ports'->0->>'host')::int FROM resources WHERE id = $1`, app.ID).Scan(&appHost); err != nil {
		t.Fatal(err)
	}
	if appHost != appPort {
		t.Fatalf("app host port = %d, want the requested %d on an empty server", appHost, appPort)
	}

	// A database created afterward must NOT be handed the port the app publishes.
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "db", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	var collision bool
	if err := st.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM db_credentials WHERE server_id = $1 AND port = $2)`,
		serverID, appPort).Scan(&collision); err != nil {
		t.Fatal(err)
	}
	if collision {
		t.Fatalf("a managed engine was allocated mesh port %d already published by the app — "+
			"bind collision at deploy (SIGMA-355)", appPort)
	}
}

// TestPreviewEnvironmentGetsAReachableURL is SIGMA-357. ensurePreviewTx inserted
// the ephemeral app row without a public_label, so it defaulted to '' — the
// preview URL was always empty and the reconciler rendered no Traefik router, so
// the preview deployed green and was reachable by nobody, the exact gap SIGMA-351
// set out to close (left open on the one path that never went through
// CreateResource).
func TestPreviewEnvironmentGetsAReachableURL(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_prev_url"
	st.SetAppsDomain("apps.example.com")
	envID, serverID := dbTestFixture(t, st, orgID, false, "general")

	var projectID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id FROM environments WHERE id = $1`, envID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: projectID, RepoFullName: "acme/shop",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetConnectionPreviews(ctx, orgID, conn.ID, true, serverID, "test"); err != nil {
		t.Fatal(err)
	}
	appSpec, _ := json.Marshal(map[string]any{"image": "", "ports": []map[string]any{{"container": 3000}}})
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "web", Kind: "app", Spec: appSpec,
	}, "test"); err != nil {
		t.Fatal(err)
	}

	if _, err := st.HandleGitWebhook(ctx, store.GitWebhookEvent{
		DeliveryID: "d-pr9-open", EventType: "pull_request", RepoFullName: "acme/shop",
		Action: "opened", PRNumber: 9, Branch: "feat/y", SHA: "aaa999",
	}); err != nil {
		t.Fatal(err)
	}

	previews, err := st.ListPreviewEnvironments(ctx, orgID, conn.ID)
	if err != nil || len(previews) != 1 {
		t.Fatalf("previews = %+v err = %v", previews, err)
	}

	// The preview resource must carry a public_label of its own.
	var label string
	if err := st.Pool.QueryRow(ctx,
		`SELECT public_label FROM resources WHERE id = $1`, *previews[0].ResourceID).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label == "" {
		t.Fatal("preview resource has no public_label — its URL is empty and it renders no router (SIGMA-357)")
	}
	// And the surfaced URL resolves under the configured wildcard.
	if got := previews[0].URL; got == "" ||
		!strings.HasPrefix(got, "https://pr-9-") || !strings.HasSuffix(got, ".apps.example.com") {
		t.Fatalf("preview URL = %q, want https://pr-9-<id>.apps.example.com", got)
	}
}
