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
