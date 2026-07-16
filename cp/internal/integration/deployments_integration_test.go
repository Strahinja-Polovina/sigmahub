package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestDeploymentsLifecycle exercises the P1-9 deployments store against a real
// Postgres: queue → status transitions → terminal freeze (immutable history) →
// rollback targets (successful releases with an image) → build dedup.
func TestDeploymentsLifecycle(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_dep"

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
	appSpec, _ := json.Marshal(map[string]any{"image": "nginx"})
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "web", Kind: "app", Spec: appSpec,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Queue a git-triggered deploy.
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID,
		Trigger: "git", GitRef: "refs/heads/main", GitSHA: "sha1", ConfigHash: "cfg1",
	}, "test")
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	if dep.Status != "queued" || dep.ServerID != serverID {
		t.Fatalf("queued deploy = %+v", dep)
	}

	// Transition building → deploying → success, stamping the built image.
	bs := 12
	if err := st.SetDeploymentStatus(ctx, dep.ID, store.DeploymentStatusUpdate{Status: "building", MarkStarted: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentStatus(ctx, dep.ID, store.DeploymentStatusUpdate{Status: "deploying", ImageDigest: "sha256:abc", BuildSeconds: &bs}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentStatus(ctx, dep.ID, store.DeploymentStatusUpdate{Status: "success", MarkFinished: true}); err != nil {
		t.Fatal(err)
	}

	// Immutable history: a terminal row can't be transitioned again.
	if err := st.SetDeploymentStatus(ctx, dep.ID, store.DeploymentStatusUpdate{Status: "failed"}); err == nil {
		t.Fatal("a terminal deployment must not be re-transitioned")
	}

	got, err := st.ListDeployments(ctx, orgID, res.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != "success" || got[0].ImageDigest != "sha256:abc" {
		t.Fatalf("history = %+v", got)
	}
	if got[0].DurationSeconds == nil || got[0].BuildSeconds == nil || *got[0].BuildSeconds != 12 {
		t.Fatalf("timings not stamped: %+v", got[0])
	}

	// Rollback targets: only successful releases with an image.
	targets, err := st.RollbackTargets(ctx, orgID, res.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ImageDigest != "sha256:abc" {
		t.Fatalf("rollback targets = %+v", targets)
	}

	// A failed deploy is not a rollback target.
	failDep, _ := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{ResourceID: res.ID, ServerID: serverID, Trigger: "git", GitSHA: "sha2"}, "test")
	_ = st.SetDeploymentStatus(ctx, failDep.ID, store.DeploymentStatusUpdate{Status: "failed", Detail: "health check failed", MarkFinished: true})
	targets, _ = st.RollbackTargets(ctx, orgID, res.ID, 10)
	if len(targets) != 1 {
		t.Fatalf("failed deploy must not be a rollback target: %+v", targets)
	}

	// Build dedup: record a built image, then a lookup by the same key reuses it.
	dedup := "cfg1:sha1"
	if _, err := st.LookupBuild(ctx, res.ID, dedup); err == nil {
		t.Fatal("expected no build before recording")
	}
	if err := st.RecordBuildResult(ctx, orgID, res.ID, serverID, dedup, "sha1", "sigmahub/web:sha1", "sha256:abc", "built"); err != nil {
		t.Fatal(err)
	}
	b, err := st.LookupBuild(ctx, res.ID, dedup)
	if err != nil || b.Status != "built" || b.ImageDigest != "sha256:abc" {
		t.Fatalf("build dedup lookup = %+v (err %v)", b, err)
	}

	// Deploy-log streaming cursor.
	_ = st.AppendDeployLog(ctx, dep.ID, "build", "step 1/5 : FROM nginx")
	_ = st.AppendDeployLog(ctx, dep.ID, "build", "step 2/5 : COPY . .")
	logs, err := st.DeployLogsSince(ctx, dep.ID, 0, 100)
	if err != nil || len(logs) != 2 {
		t.Fatalf("deploy logs = %+v (err %v)", logs, err)
	}
	logs2, _ := st.DeployLogsSince(ctx, dep.ID, logs[0].ID, 100)
	if len(logs2) != 1 || logs2[0].Line != "step 2/5 : COPY . ." {
		t.Fatalf("log cursor = %+v", logs2)
	}
}
