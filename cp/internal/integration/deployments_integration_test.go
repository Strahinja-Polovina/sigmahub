package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
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
	_ = st.AppendDeployLog(ctx, serverID, dep.ID, "build", "step 1/5 : FROM nginx")
	_ = st.AppendDeployLog(ctx, serverID, dep.ID, "build", "step 2/5 : COPY . .")
	logs, err := st.DeployLogsSince(ctx, dep.ID, 0, 100)
	if err != nil || len(logs) != 2 {
		t.Fatalf("deploy logs = %+v (err %v)", logs, err)
	}
	logs2, _ := st.DeployLogsSince(ctx, dep.ID, logs[0].ID, 100)
	if len(logs2) != 1 || logs2[0].Line != "step 2/5 : COPY . ." {
		t.Fatalf("log cursor = %+v", logs2)
	}
	// Batched append (SIGMA-252): the agent ships hundreds of lines per request
	// and they go in as ONE statement. Every line must land, in the order it was
	// produced — deploy_logs.id is the SSE cursor, so insert order is the order
	// the operator reads the build in.
	batch := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		batch = append(batch, "batched line "+strconv.Itoa(i))
	}
	if err := st.AppendDeployLogs(ctx, serverID, dep.ID, "build", batch); err != nil {
		t.Fatal(err)
	}
	after, err := st.DeployLogsSince(ctx, dep.ID, logs[1].ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(batch) {
		t.Fatalf("batched append wrote %d lines, want %d", len(after), len(batch))
	}
	for i, l := range after {
		if l.Line != batch[i] {
			t.Fatalf("batched line %d = %q, want %q (order not preserved)", i, l.Line, batch[i])
		}
	}
	// The BOLA guard survives batching: a server with no part in this deployment
	// writes nothing, even when it posts a whole batch.
	if err := st.AppendDeployLogs(ctx, "srv_not_involved", dep.ID, "build", []string{"forged"}); err != nil {
		t.Fatal(err)
	}
	if forged, _ := st.DeployLogsSince(ctx, dep.ID, after[len(after)-1].ID, 100); len(forged) != 0 {
		t.Fatalf("a foreign server forged %d lines into the deploy log", len(forged))
	}

	// GetDeployment is org-scoped (BOLA): the right org resolves it; a foreign
	// org gets ErrNotFound.
	one, err := st.GetDeployment(ctx, orgID, dep.ID)
	if err != nil || one.ID != dep.ID {
		t.Fatalf("get deployment = %+v (err %v)", one, err)
	}
	if _, err := st.GetDeployment(ctx, "org_other", dep.ID); err != store.ErrNotFound {
		t.Fatalf("cross-org GetDeployment must be ErrNotFound, got %v", err)
	}

	// Rebuild-free rollback: rolling back to the successful release reuses its
	// image (trigger=rollback, rollback_of set, image carried forward, status
	// queued) and targets the same server so the reconciler renders only rollout.
	rb, rbServer, err := st.CreateRollback(ctx, orgID, res.ID, dep.ID, "operator")
	if err != nil {
		t.Fatalf("create rollback: %v", err)
	}
	if rb.Trigger != "rollback" || rb.RollbackOf != dep.ID || rb.ImageDigest != "sha256:abc" || rb.Status != "queued" || rbServer != serverID {
		t.Fatalf("rollback = %+v (server %q)", rb, rbServer)
	}

	// A failed deployment is not a valid rollback target.
	if _, _, err := st.CreateRollback(ctx, orgID, res.ID, failDep.ID, "operator"); err == nil {
		t.Fatal("rollback to a failed release must be rejected")
	}
}

// TestComposeDeploymentPerServiceStatus pins the multi-service rollup: a
// deployment with 2 services flips to success only once BOTH service rollouts
// report, and to failed the moment one service fails.
func TestComposeDeploymentPerServiceStatus(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_compose"

	proj, _ := st.CreateProject(ctx, orgID, "p", "", "test")
	env, _ := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	bootTok, _, _, _ := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	reg, _ := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	serverID := reg.Server.ID
	_ = st.AttachServer(ctx, orgID, env.ID, serverID, "test")
	// A compose resource spec with two services.
	composeSpec, _ := json.Marshal(map[string]any{
		"compose": map[string]any{"services": []map[string]any{{"name": "web"}, {"name": "db"}}},
	})
	res, _ := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "app", Kind: "app", Spec: composeSpec,
	}, "test")

	// A git deploy of the compose app records service_count = 2 (via the drain
	// path's composeServiceCount; here we create it directly with a matching spec).
	dep, _ := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID, Trigger: "git", GitSHA: "sha1",
	}, "test")
	// CreateDeployment doesn't parse compose; set service_count as the drain path would.
	if _, err := st.Pool.Exec(ctx, `UPDATE deployments SET service_count = 2 WHERE id = $1`, dep.ID); err != nil {
		t.Fatal(err)
	}

	// web's rollout succeeds — the deployment is still deploying (db pending).
	if err := st.AdvanceDeploymentService(ctx, serverID, res.ID, "web", "rollout", true, "", 0); err != nil {
		t.Fatal(err)
	}
	d, _ := st.GetDeployment(ctx, orgID, dep.ID)
	if d.Status != "deploying" {
		t.Fatalf("one-of-two services done should still be deploying, got %s", d.Status)
	}

	// db's rollout succeeds — now ALL services are done → success.
	if err := st.AdvanceDeploymentService(ctx, serverID, res.ID, "db", "rollout", true, "", 0); err != nil {
		t.Fatal(err)
	}
	d, _ = st.GetDeployment(ctx, orgID, dep.ID)
	if d.Status != "success" {
		t.Fatalf("all services done should be success, got %s", d.Status)
	}

	// A second compose deploy where one service fails → the deployment fails.
	dep2, _ := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID, Trigger: "git", GitSHA: "sha2",
	}, "test")
	_, _ = st.Pool.Exec(ctx, `UPDATE deployments SET service_count = 2 WHERE id = $1`, dep2.ID)
	if err := st.AdvanceDeploymentService(ctx, serverID, res.ID, "web", "rollout", true, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceDeploymentService(ctx, serverID, res.ID, "db", "rollout", false, "db image build failed", 0); err != nil {
		t.Fatal(err)
	}
	d2, _ := st.GetDeployment(ctx, orgID, dep2.ID)
	if d2.Status != "failed" {
		t.Fatalf("a failed service must fail the deployment, got %s", d2.Status)
	}
}

// TestDeploymentSupersedeAndAdvance pins the review fixes: a newer deployment
// supersedes the in-flight one (single-in-flight invariant), status advances
// monotonically with started_at stamped on the first move, a rollout success
// records the image_digest that makes it a rollback target, and a stale report
// after terminal is a no-op.
func TestDeploymentSupersedeAndAdvance(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_sup"

	proj, _ := st.CreateProject(ctx, orgID, "p", "", "test")
	env, _ := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	bootTok, _, _, _ := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	reg, _ := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	serverID := reg.Server.ID
	_ = st.AttachServer(ctx, orgID, env.ID, serverID, "test")
	appSpec, _ := json.Marshal(map[string]any{"image": "nginx"})
	res, _ := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "web", Kind: "app", Spec: appSpec,
	}, "test")

	// First deploy, advance one step so it is in flight with started_at stamped.
	dep1, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID, Trigger: "git", GitSHA: "abcdef1", ConfigHash: "cfg",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceDeploymentForResource(ctx, serverID, res.ID, "clone", true, "", 0); err != nil {
		t.Fatal(err)
	}
	d1, _ := st.GetDeployment(ctx, orgID, dep1.ID)
	if d1.Status != "building" || d1.StartedAt == nil {
		t.Fatalf("clone advance = %s started=%v", d1.Status, d1.StartedAt)
	}

	// A manual redeploy supersedes the in-flight dep1 and becomes the sole in-flight.
	dep2, srv, err := st.CreateManualRedeploy(ctx, orgID, res.ID, "operator")
	if err != nil {
		t.Fatalf("manual redeploy: %v", err)
	}
	if srv != serverID {
		t.Fatalf("redeploy server = %q", srv)
	}
	d1, _ = st.GetDeployment(ctx, orgID, dep1.ID)
	if d1.Status != "superseded" {
		t.Fatalf("dep1 should be superseded, got %s", d1.Status)
	}

	// Out-of-order status report (rollout before clone/build) still advances the
	// single in-flight deployment monotonically to success and records the image.
	if err := st.AdvanceDeploymentForResource(ctx, serverID, res.ID, "rollout", true, "", 0); err != nil {
		t.Fatal(err)
	}
	// A late 'clone' report must NOT regress a terminal success.
	if err := st.AdvanceDeploymentForResource(ctx, serverID, res.ID, "clone", true, "", 0); err != nil {
		t.Fatal(err)
	}
	d2, _ := st.GetDeployment(ctx, orgID, dep2.ID)
	if d2.Status != "success" {
		t.Fatalf("dep2 status = %s, want success", d2.Status)
	}
	if d2.ImageDigest == "" {
		t.Fatal("a successful git deployment must record image_digest (rollback target)")
	}

	// It is now a rebuild-free rollback target.
	targets, _ := st.RollbackTargets(ctx, orgID, res.ID, 10)
	found := false
	for _, tgt := range targets {
		if tgt.ID == dep2.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the successful deployment should be a rollback target")
	}
}

// TestConfigDeployments pins SIGMA-166's store half: a secret/domain change on
// a resource whose standing target is a SUCCESSFUL release mints a 'config'
// deployment that copies the release's coordinates AND its image pin (so the
// render re-ships the running image under a fresh generation), returns the
// server to re-render, and skips resources that are in flight or undeployed.
func TestConfigDeployments(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_cfgdep"

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
	appSpec, _ := json.Marshal(map[string]any{"repo": "acme/app"})
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "web", Kind: "app", Spec: appSpec,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// An undeployed resource mints nothing.
	refs, err := st.CreateConfigDeployments(ctx, orgID, []string{res.ID}, "admin", "secret changed")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("undeployed resource must not mint a config deploy: %+v", refs)
	}

	// Deploy to success.
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID,
		Trigger: "git", GitRef: "refs/heads/main", GitSHA: "shacfg1", ConfigHash: "cfg1",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceDeploymentForResource(ctx, serverID, res.ID, "rollout", true, "", 0); err != nil {
		t.Fatal(err)
	}

	// The config deploy copies the release's sha + pin and returns the server.
	refs, err = st.CreateConfigDeployments(ctx, orgID, []string{res.ID}, "admin", "secret changed")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].ServerID != serverID || refs[0].OrgID != orgID {
		t.Fatalf("config deploy refs = %+v", refs)
	}
	var srcPin, cfgPin, cfgSHA, cfgStatus, cfgDigest string
	if err := st.Pool.QueryRow(ctx,
		`SELECT COALESCE(image_pin,'') FROM deployments WHERE id = $1`, dep.ID).Scan(&srcPin); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `
		SELECT COALESCE(image_pin,''), COALESCE(git_sha,''), status, COALESCE(image_digest,'')
		  FROM deployments WHERE org_id = $1 AND resource_id = $2 AND trigger = 'config'`,
		orgID, res.ID).Scan(&cfgPin, &cfgSHA, &cfgStatus, &cfgDigest); err != nil {
		t.Fatal(err)
	}
	if srcPin == "" || cfgPin != srcPin {
		t.Fatalf("config deploy must copy the source pin: src=%q cfg=%q", srcPin, cfgPin)
	}
	if cfgSHA != "shacfg1" || cfgStatus != "queued" {
		t.Fatalf("config row = sha %q status %q", cfgSHA, cfgStatus)
	}
	if cfgDigest == "" {
		t.Fatal("config deploy must copy the recorded image reference")
	}

	// With the config row in flight, a second change mints nothing new.
	refs, err = st.CreateConfigDeployments(ctx, orgID, []string{res.ID}, "admin", "secret changed")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM deployments WHERE org_id = $1 AND resource_id = $2 AND trigger = 'config'`,
		orgID, res.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 || n != 1 {
		t.Fatalf("in-flight config deploy must not stack another: refs=%+v count=%d", refs, n)
	}

	// A FAILED latest deployment must not swallow the change. This is the case
	// that matters most in practice — the user is editing an env var precisely
	// because the last rollout died — and it used to mint nothing at all while
	// the dashboard reported the secret saved.
	if err := st.AdvanceDeploymentForResource(ctx, serverID, res.ID, "rollout", false,
		"new version unhealthy, kept previous", 0); err != nil {
		t.Fatal(err)
	}
	var lastStatus string
	if err := st.Pool.QueryRow(ctx, `
		SELECT status FROM deployments WHERE org_id = $1 AND resource_id = $2
		 ORDER BY created_at DESC LIMIT 1`, orgID, res.ID).Scan(&lastStatus); err != nil {
		t.Fatal(err)
	}
	if lastStatus != "failed" {
		t.Fatalf("precondition: latest deployment should be failed, got %q", lastStatus)
	}

	refs, err = st.CreateConfigDeployments(ctx, orgID, []string{res.ID}, "admin", "secret changed")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("a failed latest deployment must still mint a config deploy, got refs=%+v", refs)
	}

	// And it re-ships the last SUCCESSFUL release, not the failed attempt's
	// image: a rollout that fails its health gate leaves its predecessor
	// serving, so that predecessor is what the new config belongs on.
	var newPin, newSHA, newStatus string
	if err := st.Pool.QueryRow(ctx, `
		SELECT COALESCE(image_pin,''), COALESCE(git_sha,''), status
		  FROM deployments WHERE org_id = $1 AND resource_id = $2 AND trigger = 'config'
		 ORDER BY created_at DESC LIMIT 1`, orgID, res.ID).Scan(&newPin, &newSHA, &newStatus); err != nil {
		t.Fatal(err)
	}
	if newStatus != "queued" {
		t.Fatalf("new config row status = %q", newStatus)
	}
	if newSHA != "shacfg1" {
		t.Fatalf("config deploy must carry the successful release's sha, got %q", newSHA)
	}
	if newPin != srcPin {
		t.Fatalf("config deploy must re-ship the successful release's pin: want %q got %q", srcPin, newPin)
	}
}

// TestCloneCredentialReachesEveryServerThatClones is SIGMA-228. The git.clone op
// is not rendered into the deploy TARGET's document when a dedicated build
// server exists — it lands in the BUILD server's — and for a cluster workload
// there is no deploy target at all (server_id is NULL). Scoping the credential
// release to `d.server_id = $1` therefore 404s exactly the agent that has to
// clone, and every private-repo deploy on those two shapes dies at clone with an
// auth-shaped error that blames the repo rather than the control plane.
func TestCloneCredentialReachesEveryServerThatClones(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_clone_cred_scope"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	target := connectServer(t, st, orgID, "srv-web")
	builder := connectServer(t, st, orgID, "srv-build")
	cpNode := connectServer(t, st, orgID, "srv-cp")
	stranger := connectServer(t, st, orgID, "srv-other")
	for _, id := range []string{target, builder, cpNode, stranger} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "test"); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, RepoFullName: "acme/api", Token: "ghp_pat",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Shape 1: a server-bound app with a dedicated build server.
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: target, Name: "web", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: env.ID, ServerID: target, ConnectionID: conn.ID,
		Trigger: "git", GitRef: "refs/heads/main", GitSHA: "abc1234567", ConfigHash: "cfg",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, dep.ID, builder); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.DeploymentCloneCredential(ctx, target, dep.ID); err != nil {
		t.Fatalf("deploy target must still resolve the credential: %v", err)
	}
	tok, repo, _, err := st.DeploymentCloneCredential(ctx, builder, dep.ID)
	if err != nil {
		t.Fatalf("the build server holds the clone op but cannot fetch its credential: %v", err)
	}
	if tok != "ghp_pat" || repo != "acme/api" {
		t.Fatalf("build-server credential = %q %q", tok, repo)
	}

	// Shape 2: a cluster workload — no deploy target at all.
	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: env.ID, Name: "prod", ControlPlaneID: cpNode,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	capp, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ClusterID: cluster.ID, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"ports":[{"container":8080}]}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	cdep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: capp.ID, EnvironmentID: env.ID, ConnectionID: conn.ID,
		Trigger: "git", GitRef: "refs/heads/main", GitSHA: "def4567890", ConfigHash: "cfg",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, cdep.ID, builder); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.DeploymentCloneCredential(ctx, builder, cdep.ID); err != nil {
		t.Fatalf("cluster build server cannot fetch its credential: %v", err)
	}
	if _, _, _, err := st.DeploymentCloneCredential(ctx, cpNode, cdep.ID); err != nil {
		t.Fatalf("a cluster node must resolve the credential for its own workload: %v", err)
	}

	// The BOLA guard has to survive the widening: a server that owns no part of
	// either deployment still gets nothing.
	if _, _, _, err := st.DeploymentCloneCredential(ctx, stranger, dep.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unrelated server got the credential: err = %v", err)
	}
	if _, _, _, err := st.DeploymentCloneCredential(ctx, stranger, cdep.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unrelated server got the cluster credential: err = %v", err)
	}
}
