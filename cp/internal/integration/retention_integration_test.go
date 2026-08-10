package integration

// SIGMA-249: the sweeper's only retention action was PruneMetrics on
// server_metrics. Every other growth table — deploy_logs above all, but also
// cp_audit_log, deploy_requests, webhook_deliveries, alert_outbox and
// idempotency_keys — was append-only with no cleanup at all, on a compose host
// with one disk.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/sweeper"
)

func countRows(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSweeperPrunesGrowthTables(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orgID := "org_retain"

	// A finished deployment from four months ago, with build-log lines against
	// it — the shape that dominates the table: 30 deploys a day at ~2,500 lines
	// each is 75,000 rows a day, on a table nothing reads beyond the last 500
	// lines of an IN-FLIGHT build.
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID := reg.Server.ID
	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
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
	oldDep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID, Trigger: "manual", GitSHA: "abc1234def",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	freshDep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID, Trigger: "manual", GitSHA: "def5678abc",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `
		UPDATE deployments SET status = 'success', finished_at = now() - interval '120 days',
		       created_at = now() - interval '120 days'
		 WHERE id = $1`, oldDep.ID); err != nil {
		t.Fatal(err)
	}
	for _, dep := range []string{oldDep.ID, freshDep.ID} {
		for i := 0; i < 5; i++ {
			if _, err := st.Pool.Exec(ctx,
				`INSERT INTO deploy_logs (deployment_id, stream, line, at)
				 VALUES ($1, 'build', 'step', now() - interval '120 days')`, dep); err != nil {
				t.Fatal(err)
			}
		}
	}

	// The other append-only tables, each with one old row and one recent row.
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	seed(`INSERT INTO cp_audit_log (org_id, actor, action, target, created_at)
	      VALUES ($1, 'reconciler', 'DSD issued', 'old', now() - interval '400 days'),
	             ($1, 'reconciler', 'DSD issued', 'new', now())`, orgID)
	seed(`INSERT INTO webhook_deliveries (delivery_id, provider, event_type, received_at)
	      VALUES ('wh_old', 'github', 'push', now() - interval '120 days'),
	             ('wh_new', 'github', 'push', now())`)
	seed(`INSERT INTO idempotency_keys (org_id, key, request_hash, status_code, response, created_at)
	      VALUES ($1, 'k_old', '\x00', 200, '{}'::jsonb, now() - interval '120 days'),
	             ($1, 'k_new', '\x00', 200, '{}'::jsonb, now())`, orgID)

	ch, err := st.CreateAlertChannel(ctx, orgID, "test", store.CreateAlertChannelInput{
		Kind: "slack", Name: "ops", Secret: "https://hooks.slack.example/T000/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	seed(`INSERT INTO alert_outbox (org_id, channel_id, event, dedup_key, title, body, status, created_at)
	      VALUES ($1, $2, 'deploy_failed', 'd_old', 't', 'b', 'sent',    now() - interval '120 days'),
	             ($1, $2, 'deploy_failed', 'd_stuck', 't', 'b', 'pending', now() - interval '120 days'),
	             ($1, $2, 'deploy_failed', 'd_new', 't', 'b', 'sent',    now())`, orgID, ch.ID)

	conn := "gc_retain"
	seed(`INSERT INTO git_connections (id, org_id, project_id, provider, installation_id, repo_full_name, created_by)
	      VALUES ($1, $2, $3, 'github', 'inst_1', 'acme/retain', 'test')`, conn, orgID, proj.ID)
	seed(`INSERT INTO deploy_requests (id, org_id, connection_id, environment_id, kind, ref, sha, status, created_at)
	      VALUES ('dr_old', $1, $2, $3, 'deploy', 'refs/heads/main', 'aaa', 'done',   now() - interval '120 days'),
	             ('dr_queued', $1, $2, $3, 'deploy', 'refs/heads/main', 'bbb', 'queued', now() - interval '120 days'),
	             ('dr_new', $1, $2, $3, 'deploy', 'refs/heads/main', 'ccc', 'done',   now())`, orgID, conn, env.ID)

	// One sweep, with the fleet-health knobs disabled so only retention runs.
	sctx, cancel := context.WithCancel(ctx)
	go sweeper.Run(sctx, log, st, sweeper.Config{
		Interval:   20 * time.Millisecond,
		StaleAfter: time.Hour,
		Retention:  24 * time.Hour,
		Retain: store.Retention{
			DeployLogs:        30 * 24 * time.Hour,
			Audit:             365 * 24 * time.Hour,
			DeployRequests:    30 * 24 * time.Hour,
			WebhookDeliveries: 30 * 24 * time.Hour,
			AlertOutbox:       30 * 24 * time.Hour,
			IdempotencyKeys:   7 * 24 * time.Hour,
		},
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countRows(t, st, `SELECT count(*) FROM deploy_logs WHERE deployment_id = $1`, oldDep.ID) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	// The logs of a deployment that finished four months ago are gone…
	if n := countRows(t, st, `SELECT count(*) FROM deploy_logs WHERE deployment_id = $1`, oldDep.ID); n != 0 {
		t.Fatalf("deploy_logs for a deployment finished 120 days ago = %d, want 0", n)
	}
	// …and the in-flight deployment's are untouched, however old the LINES look:
	// retention is keyed on the deployment's life, not on each line's timestamp,
	// because a build that has been streaming for an hour is still being read.
	if n := countRows(t, st, `SELECT count(*) FROM deploy_logs WHERE deployment_id = $1`, freshDep.ID); n != 5 {
		t.Fatalf("deploy_logs for an unfinished deployment = %d, want 5 (kept)", n)
	}

	if n := countRows(t, st, `SELECT count(*) FROM cp_audit_log WHERE target = 'old'`); n != 0 {
		t.Fatalf("cp_audit_log rows older than the audit retention = %d, want 0", n)
	}
	if n := countRows(t, st, `SELECT count(*) FROM cp_audit_log WHERE target = 'new'`); n != 1 {
		t.Fatal("a recent audit row was pruned")
	}
	if n := countRows(t, st, `SELECT count(*) FROM webhook_deliveries WHERE delivery_id = 'wh_old'`); n != 0 {
		t.Fatalf("stale webhook_deliveries = %d, want 0", n)
	}
	if n := countRows(t, st, `SELECT count(*) FROM webhook_deliveries WHERE delivery_id = 'wh_new'`); n != 1 {
		t.Fatal("a recent webhook delivery was pruned")
	}
	if n := countRows(t, st, `SELECT count(*) FROM idempotency_keys WHERE key = 'k_old'`); n != 0 {
		t.Fatalf("stale idempotency_keys = %d, want 0", n)
	}
	if n := countRows(t, st, `SELECT count(*) FROM idempotency_keys WHERE key = 'k_new'`); n != 1 {
		t.Fatal("a recent idempotency key was pruned")
	}
	if n := countRows(t, st, `SELECT count(*) FROM alert_outbox WHERE dedup_key = 'd_old'`); n != 0 {
		t.Fatalf("stale sent alert_outbox rows = %d, want 0", n)
	}
	// A row still pending is undelivered work, not history: pruning it would
	// silently drop an alert nobody ever received.
	if n := countRows(t, st, `SELECT count(*) FROM alert_outbox WHERE dedup_key = 'd_stuck'`); n != 1 {
		t.Fatal("an undelivered alert was pruned")
	}
	if n := countRows(t, st, `SELECT count(*) FROM alert_outbox WHERE dedup_key = 'd_new'`); n != 1 {
		t.Fatal("a recent alert row was pruned")
	}
	if n := countRows(t, st, `SELECT count(*) FROM deploy_requests WHERE id = 'dr_old'`); n != 0 {
		t.Fatalf("stale drained deploy_requests = %d, want 0", n)
	}
	// Same rule as the outbox: a request still queued is work the drain has not
	// done yet, however old it looks.
	if n := countRows(t, st, `SELECT count(*) FROM deploy_requests WHERE id = 'dr_queued'`); n != 1 {
		t.Fatal("an undrained deploy request was pruned")
	}
	if n := countRows(t, st, `SELECT count(*) FROM deploy_requests WHERE id = 'dr_new'`); n != 1 {
		t.Fatal("a recent deploy request was pruned")
	}
}
