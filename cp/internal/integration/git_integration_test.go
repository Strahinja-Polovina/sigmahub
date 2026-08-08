package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

const itWebhookSecret = "integration-secret"

func signWebhook(body string) string {
	mac := hmac.New(sha256.New, []byte(itWebhookSecret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// postDelivery sends a signed GitHub webhook and returns the status code.
func postDelivery(t *testing.T, base, event, delivery, body string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/v1/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", signWebhook(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post delivery: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func countKind(t *testing.T, st *store.Store, orgID, kind string) int {
	t.Helper()
	reqs, err := st.ListDeployRequests(context.Background(), orgID, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range reqs {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

func auditCount(t *testing.T, st *store.Store, orgID, actor string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM cp_audit_log WHERE org_id = $1 AND actor = $2`, orgID, actor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestGitWebhookRouting is the P1-7 acceptance harness: a signed push on an auto
// branch enqueues exactly one deploy (idempotent on redelivery); a manual branch
// enqueues nothing until promoted; a PR event records a hook but no deploy; a
// forged signature is rejected; every routed delivery writes an audit row.
func TestGitWebhookRouting(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := api.New(log, st, st, st, api.Options{
		DevServiceToken:     "dev",
		Git:                 st,
		GitHubWebhookSecret: itWebhookSecret,
		DSDPublicKey:        dsdKey.Public().(ed25519.PublicKey),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	orgID := "org_git"
	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	envProd, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	envStg, err := st.CreateEnvironment(ctx, orgID, proj.ID, "staging", false, "test")
	if err != nil {
		t.Fatal(err)
	}

	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, RepoFullName: "Acme/App", InstallationID: "42", Token: "ghs_secret",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	// main → prod (auto), release → staging (manual).
	if _, err := st.SetBranchMap(ctx, orgID, conn.ID, "main", envProd.ID, "auto", "", "test"); err != nil {
		t.Fatal(err)
	}
	manualMap, err := st.SetBranchMap(ctx, orgID, conn.ID, "release", envStg.ID, "manual", "", "test")
	if err != nil {
		t.Fatal(err)
	}

	pushBody := func(branch, sha string) string {
		return `{"ref":"refs/heads/` + branch + `","after":"` + sha + `","deleted":false,"repository":{"full_name":"Acme/App"}}`
	}

	// 1. Auto branch push → exactly one deploy.
	if code := postDelivery(t, ts.URL, "push", "d-auto-1", pushBody("main", "sha-main-1")); code != 200 {
		t.Fatalf("auto push status = %d, want 200", code)
	}
	if n := countKind(t, st, orgID, "deploy"); n != 1 {
		t.Fatalf("after auto push: deploy count = %d, want 1", n)
	}

	// 2. Idempotent redelivery (same delivery id) → still one deploy.
	if code := postDelivery(t, ts.URL, "push", "d-auto-1", pushBody("main", "sha-main-1")); code != 200 {
		t.Fatalf("redelivery status = %d, want 200", code)
	}
	if n := countKind(t, st, orgID, "deploy"); n != 1 {
		t.Fatalf("after redelivery: deploy count = %d, want 1 (idempotent)", n)
	}

	// 3. Manual branch push → records the commit, enqueues NO deploy.
	if code := postDelivery(t, ts.URL, "push", "d-manual-1", pushBody("release", "sha-rel-1")); code != 200 {
		t.Fatalf("manual push status = %d, want 200", code)
	}
	if n := countKind(t, st, orgID, "deploy"); n != 1 {
		t.Fatalf("after manual push: deploy count = %d, want 1 (unchanged)", n)
	}

	// 4. Promotion enqueues the remembered manual commit.
	dr, err := st.PromoteBranch(ctx, orgID, manualMap.ID, "test")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if dr.SHA != "sha-rel-1" || dr.EnvironmentID != envStg.ID {
		t.Fatalf("promoted deploy = %+v, want sha-rel-1 → staging", dr)
	}
	if n := countKind(t, st, orgID, "deploy"); n != 2 {
		t.Fatalf("after promote: deploy count = %d, want 2", n)
	}

	// 5. Push to an unmapped branch → no deploy.
	if code := postDelivery(t, ts.URL, "push", "d-unmapped", pushBody("feature-x", "sha-fx")); code != 200 {
		t.Fatalf("unmapped push status = %d", code)
	}
	if n := countKind(t, st, orgID, "deploy"); n != 2 {
		t.Fatalf("unmapped push must not deploy: count = %d, want 2", n)
	}

	// 6. Pull request → records a pr_hook, no deploy.
	prBody := `{"action":"opened","pull_request":{"head":{"ref":"feature-x","sha":"sha-pr"}},"repository":{"full_name":"Acme/App"}}`
	if code := postDelivery(t, ts.URL, "pull_request", "d-pr-1", prBody); code != 200 {
		t.Fatalf("pr status = %d", code)
	}
	if n := countKind(t, st, orgID, "pr_hook"); n != 1 {
		t.Fatalf("pr_hook count = %d, want 1", n)
	}
	if n := countKind(t, st, orgID, "deploy"); n != 2 {
		t.Fatalf("pr must not deploy: deploy count = %d, want 2", n)
	}

	// 7. Forged signature → 401, no side effect.
	forged, _ := http.NewRequest("POST", ts.URL+"/v1/webhooks/github", strings.NewReader(pushBody("main", "sha-forged")))
	forged.Header.Set("X-GitHub-Event", "push")
	forged.Header.Set("X-GitHub-Delivery", "d-forged")
	forged.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	fresp, err := http.DefaultClient.Do(forged)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, fresp.Body)
	fresp.Body.Close()
	if fresp.StatusCode != 401 {
		t.Fatalf("forged signature status = %d, want 401", fresp.StatusCode)
	}
	if n := countKind(t, st, orgID, "deploy"); n != 2 {
		t.Fatalf("forged delivery must not enqueue: deploy count = %d, want 2", n)
	}

	// 8. Unconnected repo → acknowledged, no deploy.
	otherBody := `{"ref":"refs/heads/main","after":"x","repository":{"full_name":"Other/Repo"}}`
	if code := postDelivery(t, ts.URL, "push", "d-other", otherBody); code != 200 {
		t.Fatalf("unconnected repo status = %d, want 200", code)
	}

	// Every routed delivery wrote a webhook-actor audit row (auto push, manual
	// push, unmapped push, PR — the unconnected + duplicate ones do not).
	if got := auditCount(t, st, orgID, store.WebhookActor); got < 4 {
		t.Fatalf("webhook audit rows = %d, want >= 4", got)
	}
}

// TestGitConnectionUniqueRepo proves a repo drives at most one connection.
// TestDeleteGitConnectionRefusesWhileDeployed pins SIGMA-159: disconnecting a
// repo that still has deployed resources must be refused. Allowing it nulls the
// deployments' connection_id, which removes their deploy targets, downgrades
// them to the resource.sync stub, and makes the agent's GC force-remove the
// running production containers — with no working redeploy path back.
func TestDeleteGitConnectionRefusesWhileDeployed(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_disconnect"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host-disc", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host-disc", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	srvID := reg.Server.ID
	if err := st.AttachServer(ctx, orgID, env.ID, srvID, "test"); err != nil {
		t.Fatal(err)
	}
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: srvID, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"image":"nginx:alpine"}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, RepoFullName: "Acme/Disc",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// With no deployments the disconnect is allowed.
	if err := st.DeleteGitConnection(ctx, orgID, conn.ID, "test"); err != nil {
		t.Fatalf("disconnect with no deployments: %v", err)
	}

	// Reconnect and deploy from it — now the disconnect must be refused.
	conn2, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, RepoFullName: "Acme/Disc",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: srvID,
		ConnectionID: conn2.ID, Trigger: "git", GitSHA: "abc1234",
	}, "test"); err != nil {
		t.Fatal(err)
	}
	err = st.DeleteGitConnection(ctx, orgID, conn2.ID, "test")
	if err == nil {
		t.Fatal("expected disconnect to be refused while a resource is deployed from the repo")
	}
	if !strings.Contains(err.Error(), "api") {
		t.Fatalf("refusal should name the blocking resource, got: %v", err)
	}
}

func TestGitConnectionUniqueRepo(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_uniq"
	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	in := store.CreateGitConnectionInput{ProjectID: proj.ID, RepoFullName: "Acme/Dup"}
	if _, err := st.CreateGitConnection(ctx, orgID, in, "test"); err != nil {
		t.Fatal(err)
	}
	// Same repo, different case → still a conflict (unique on lower(repo)).
	in2 := store.CreateGitConnectionInput{ProjectID: proj.ID, RepoFullName: "acme/dup"}
	_, err = st.CreateGitConnection(ctx, orgID, in2, "test")
	if err == nil {
		t.Fatal("expected conflict connecting the same repo twice")
	}

	// Uniqueness is ORG-scoped (SIGMA-174): another org connecting the same repo
	// is not blocked — the global slot was the squatting/denial vector.
	otherOrg := "org_uniq_b"
	projB, err := st.CreateProject(ctx, otherOrg, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateGitConnection(ctx, otherOrg, store.CreateGitConnectionInput{
		ProjectID: projB.ID, RepoFullName: "acme/dup",
	}, "test"); err != nil {
		t.Fatalf("cross-org connection of the same repo must succeed, got: %v", err)
	}
}

// TestWebhookDeliveryOrgScoped covers SIGMA-174's routing half: when two orgs
// hold the same repo, a delivery routes by its installation binding — never by
// repo name alone — and an unattributable delivery is dropped, not guessed.
func TestWebhookDeliveryOrgScoped(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	setup := func(orgID, installation string) store.GitConnection {
		t.Helper()
		proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
		if err != nil {
			t.Fatal(err)
		}
		env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
		if err != nil {
			t.Fatal(err)
		}
		if installation != "" {
			if err := st.ClaimInstallation(ctx, orgID, installation); err != nil {
				t.Fatal(err)
			}
		}
		conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
			ProjectID: proj.ID, RepoFullName: "Acme/Shared", InstallationID: installation,
		}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetBranchMap(ctx, orgID, conn.ID, "main", env.ID, "auto", "", "test"); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	setup("org_owner", "1001")
	setup("org_other", "2002")

	push := func(delivery, installation string) store.WebhookOutcome {
		t.Helper()
		out, err := st.HandleGitWebhook(ctx, store.GitWebhookEvent{
			DeliveryID: delivery, Provider: "github", EventType: "push",
			RepoFullName: "acme/shared", Ref: "refs/heads/main",
			SHA: strings.Repeat("a", 40), InstallationID: installation,
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}

	// A delivery carrying org_owner's installation routes to org_owner only.
	out := push("d-owner", "1001")
	if out.Connection == nil || out.Connection.OrgID != "org_owner" {
		t.Fatalf("delivery with installation 1001 must route to org_owner, got %+v", out.Connection)
	}
	if out.Enqueued == nil {
		t.Fatal("expected an auto-branch deploy for the owning org")
	}
	// And org_other's installation routes to org_other.
	out = push("d-other", "2002")
	if out.Connection == nil || out.Connection.OrgID != "org_other" {
		t.Fatalf("delivery with installation 2002 must route to org_other, got %+v", out.Connection)
	}
	// No installation and two candidate orgs: dropped, not guessed.
	out = push("d-ambiguous", "")
	if out.Connection != nil || !out.Ambiguous {
		t.Fatalf("unattributable delivery must be dropped as ambiguous, got %+v", out)
	}
}

// TestFirstDeployFromRepoHead is the cold start against a real database
// (SIGMA-177): a resource with a connected repo, a mapped branch and NO
// deployment history has to be deployable.
//
// The two halves that were missing: nothing joined a resource to the branch its
// environment is mapped to, and nothing could mint a deployment from a commit
// that was not already recorded on a previous one. Together they meant the only
// route to a first build was a git push — with the repo connected, the branch
// mapped, and the commit sitting one API call away the whole time.
func TestFirstDeployFromRepoHead(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_headdeploy"

	proj, err := st.CreateProject(ctx, orgID, "shop", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	server := connectServer(t, st, orgID, "runner")
	builder := connectServer(t, st, orgID, "builder")
	for _, id := range []string{server, builder} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/shop",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.SetBranchMap(ctx, orgID, conn.ID, "main", env.ID, "manual", builder, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: server, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"compose":{"services":[{"name":"web","build":"."},{"name":"db","image":"postgres:16"}]}}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}

	// Redeploy has nothing to replay — that is the state the Deploy button used
	// to dead-end in.
	if _, _, err := st.CreateManualRedeploy(ctx, orgID, app.ID, "admin"); err == nil {
		t.Fatal("a resource with no history must have nothing to redeploy")
	}

	origin, err := st.HeadDeployOriginForResource(ctx, orgID, app.ID)
	if err != nil {
		t.Fatalf("resolve deploy origin: %v", err)
	}
	if origin.RepoFullName != "acme/shop" || origin.Branch != "main" || origin.Ref != "refs/heads/main" {
		t.Fatalf("origin = %+v, want the repo and branch mapped to this resource's environment", origin)
	}
	if origin.ServerID != server || origin.BuildServerID != builder {
		t.Fatalf("origin server=%q build=%q, want %q/%q — the deploy must land where the resource lives and build where the map says",
			origin.ServerID, origin.BuildServerID, server, builder)
	}

	dep, target, err := st.CreateHeadDeployment(ctx, orgID, app.ID, "operator", origin, "cafebabedeadbeef")
	if err != nil {
		t.Fatalf("create head deployment: %v", err)
	}
	if target != server {
		t.Fatalf("re-render target = %q, want %q", target, server)
	}
	if dep.GitSHA != "cafebabedeadbeef" || dep.GitRef != "refs/heads/main" || dep.Trigger != "manual" || dep.Status != "queued" {
		t.Fatalf("deployment = %+v", dep)
	}
	// The per-service denominator has to be real, or a Compose deploy can never
	// complete: nothing else sets it on this path.
	if dep.ServiceCount != 2 {
		t.Fatalf("serviceCount = %d, want 2", dep.ServiceCount)
	}

	// It renders: the deploy target now has a deploy target, which is the whole
	// point of the button.
	targets, err := st.DeployTargetsForServer(ctx, server)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := targets[app.ID]
	if !ok {
		t.Fatalf("the deploy target did not reach the server's render: %+v", targets)
	}
	if got.SHA != "cafebabedeadbeef" || got.BuildServerID != builder {
		t.Fatalf("render target = %+v", got)
	}

	// And the branch map learned the commit, so Promote stops claiming there is
	// nothing to promote.
	maps, err := st.ListBranchMaps(ctx, orgID, conn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(maps) != 1 || maps[0].LastSHA != "cafebabedeadbeef" {
		t.Fatalf("branch maps = %+v, want the head recorded", maps)
	}
	if _, err := st.PromoteBranch(ctx, orgID, m.ID, "admin"); err != nil {
		t.Fatalf("promote after a head deploy: %v", err)
	}
}

// A resource that deploys from no repo resolves no origin, so the Deploy button
// falls through to the re-apply path instead of inventing a git deploy for a
// database.
func TestHeadDeployOriginRequiresAConnectedRepo(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_noorigin"

	proj, _ := st.CreateProject(ctx, orgID, "shop", "", "admin")
	env, _ := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	server := connectServer(t, st, orgID, "runner")
	if err := st.AttachServer(ctx, orgID, env.ID, server, "admin"); err != nil {
		t.Fatal(err)
	}

	// An app with no git connection at all.
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: server, Name: "api", Kind: "app",
		Spec: json.RawMessage(`{"image":"nginx:1"}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.HeadDeployOriginForResource(ctx, orgID, app.ID); err != store.ErrNotFound {
		t.Fatalf("origin for a repo-less app = %v, want ErrNotFound", err)
	}

	// A connected repo whose branch is mapped to a DIFFERENT environment is not
	// this resource's origin either — deploying it would ship another
	// environment's branch.
	conn, _ := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/shop",
	}, "admin")
	staging, _ := st.CreateEnvironment(ctx, orgID, proj.ID, "staging", false, "admin")
	if _, err := st.SetBranchMap(ctx, orgID, conn.ID, "develop", staging.ID, "auto", "", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.HeadDeployOriginForResource(ctx, orgID, app.ID); err != store.ErrNotFound {
		t.Fatalf("origin resolved across environments = %v, want ErrNotFound", err)
	}

	// A database is never a git deploy, even in an environment with a mapping.
	if _, err := st.SetBranchMap(ctx, orgID, conn.ID, "main", env.ID, "manual", "", "admin"); err != nil {
		t.Fatal(err)
	}
	db, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: server, Name: "pg", Kind: "postgres",
		Spec: json.RawMessage(`{"version":"16"}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.HeadDeployOriginForResource(ctx, orgID, db.ID); err != store.ErrNotFound {
		t.Fatalf("origin for a database = %v, want ErrNotFound", err)
	}
	// The app in the same environment now does resolve.
	if _, err := st.HeadDeployOriginForResource(ctx, orgID, app.ID); err != nil {
		t.Fatalf("origin for the mapped app: %v", err)
	}
}

// RecordBranchHead unblocks Promote on a freshly mapped manual branch, and a
// real push always outranks it.
func TestRecordBranchHeadFillsOnlyABlank(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_recordhead"

	proj, _ := st.CreateProject(ctx, orgID, "shop", "", "admin")
	env, _ := st.CreateEnvironment(ctx, orgID, proj.ID, "production", true, "admin")
	conn, _ := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/shop",
	}, "admin")
	m, err := st.SetBranchMap(ctx, orgID, conn.ID, "main", env.ID, "manual", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteBranch(ctx, orgID, m.ID, "admin"); err == nil {
		t.Fatal("a branch with no recorded commit has nothing to promote")
	}

	if err := st.RecordBranchHead(ctx, orgID, m.ID, "refs/heads/main", "aaaa111"); err != nil {
		t.Fatal(err)
	}
	maps, _ := st.ListBranchMaps(ctx, orgID, conn.ID)
	if len(maps) != 1 || maps[0].LastSHA != "aaaa111" {
		t.Fatalf("branch maps = %+v", maps)
	}
	if maps[0].LastPushedAt != nil {
		t.Error("recording a head is not a push and must not claim one")
	}

	// A second resolve must not overwrite: whatever is recorded — a real push,
	// most importantly — wins over asking the provider again.
	if err := st.RecordBranchHead(ctx, orgID, m.ID, "refs/heads/main", "bbbb222"); err != nil {
		t.Fatal(err)
	}
	maps, _ = st.ListBranchMaps(ctx, orgID, conn.ID)
	if maps[0].LastSHA != "aaaa111" {
		t.Fatalf("recorded head was overwritten: %+v", maps[0])
	}

	// Cross-org writes are refused (silently — the caller is best-effort).
	if err := st.RecordBranchHead(ctx, "org_other", m.ID, "refs/heads/main", "cccc333"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteBranch(ctx, orgID, m.ID, "admin"); err != nil {
		t.Fatalf("promote after recording a head: %v", err)
	}
}
