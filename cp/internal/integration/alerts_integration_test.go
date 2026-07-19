package integration

// P2-6 alerting integration: channel CRUD with the DEK-enveloped secret,
// producers enqueueing inside the state-change transaction (server
// unreachable + flap cooldown, recovery, deploy failed, rules filtering),
// and the outbox delivery lifecycle (backoff, terminal failure, success).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func outboxRows(t *testing.T, st *store.Store, event string) int {
	t.Helper()
	var n int
	if err := st.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM alert_outbox WHERE event = $1`, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAlertingEndToEnd(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_alerts"

	// Channel CRUD: secret sealed under the org DEK, every event on by default.
	ch, err := st.CreateAlertChannel(ctx, orgID, "test", store.CreateAlertChannelInput{
		Kind: "slack", Name: "ops", Secret: "https://hooks.slack example/T000/secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Events) != len(store.AlertEvents) {
		t.Fatalf("default events = %v", ch.Events)
	}
	if _, err := st.CreateAlertChannel(ctx, orgID, "test", store.CreateAlertChannelInput{Kind: "slack", Name: "no-secret"}); err == nil {
		t.Fatal("slack channel without a secret must be rejected")
	}
	if _, err := st.CreateAlertChannel(ctx, orgID, "test", store.CreateAlertChannelInput{Kind: "telegram", Name: "tg", Secret: "tok", Config: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("telegram channel without chatId must be rejected")
	}

	listed, err := st.ListAlertChannels(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != ch.ID {
		t.Fatalf("listed = %+v", listed)
	}
	send, err := st.AlertChannelForSend(ctx, orgID, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if send.Secret != "https://hooks.slack example/T000/secret" {
		t.Fatalf("secret round-trip = %q", send.Secret)
	}
	if _, err := st.AlertChannelForSend(ctx, "org_other", ch.ID); err == nil {
		t.Fatal("cross-org channel read must 404")
	}

	// A server that goes silent: the sweeper flip enqueues server_unreachable
	// exactly once per cooldown window (flap suppression).
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID := reg.Server.ID
	goSilent := func() {
		if _, err := st.Pool.Exec(ctx, `
			UPDATE servers SET status = 'running', last_seen_at = now() - interval '10 minutes'
			 WHERE id = $1`, serverID); err != nil {
			t.Fatal(err)
		}
	}
	goSilent()
	flipped, err := st.MarkStaleUnreachable(ctx, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if flipped != 1 {
		t.Fatalf("flipped = %d, want 1", flipped)
	}
	if n := outboxRows(t, st, store.AlertServerUnreachable); n != 1 {
		t.Fatalf("unreachable alerts = %d, want 1", n)
	}
	// Flap: silent again inside the 30-minute cooldown → no second alert.
	goSilent()
	if _, err := st.MarkStaleUnreachable(ctx, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if n := outboxRows(t, st, store.AlertServerUnreachable); n != 1 {
		t.Fatalf("cooldown broken: unreachable alerts = %d, want 1", n)
	}

	// Recovery heartbeat pairs with the unreachable alert.
	if _, err := st.Pool.Exec(ctx, `UPDATE servers SET status = 'unreachable' WHERE id = $1`, serverID); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordHeartbeat(ctx, serverID, store.HeartbeatInput{}); err != nil {
		t.Fatal(err)
	}
	if n := outboxRows(t, st, store.AlertServerRecovered); n != 1 {
		t.Fatalf("recovered alerts = %d, want 1", n)
	}

	// Deploy failure alerts once per deployment, through the status choke point.
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
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: res.ID, EnvironmentID: env.ID, ServerID: serverID, Trigger: "manual", GitSHA: "abc1234def",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentStatus(ctx, dep.ID, store.DeploymentStatusUpdate{
		Status: "failed", Detail: "image build exploded", MarkFinished: true,
	}); err != nil {
		t.Fatal(err)
	}
	if n := outboxRows(t, st, store.AlertDeployFailed); n != 1 {
		t.Fatalf("deploy_failed alerts = %d, want 1", n)
	}
	var title, body string
	if err := st.Pool.QueryRow(ctx, `
		SELECT title, body FROM alert_outbox WHERE event = $1`, store.AlertDeployFailed).Scan(&title, &body); err != nil {
		t.Fatal(err)
	}
	if title != "Deploy of web failed (abc1234)" || body != "image build exploded" {
		t.Fatalf("deploy alert = %q / %q", title, body)
	}

	// Delivery lifecycle: due → failed attempt backs off → success marks the
	// row sent and the channel healthy.
	due, err := st.DueAlertDeliveries(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) < 3 {
		t.Fatalf("due deliveries = %d, want >= 3", len(due))
	}
	first := due[0]
	if err := st.SetAlertDeliveryResult(ctx, first.ID, false, "connection refused", 8); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempts int
	var nextAt time.Time
	if err := st.Pool.QueryRow(ctx, `
		SELECT status, attempts, next_attempt_at FROM alert_outbox WHERE id = $1`, first.ID).
		Scan(&status, &attempts, &nextAt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempts != 1 || !nextAt.After(time.Now()) {
		t.Fatalf("after failed attempt: %s attempts=%d next=%v", status, attempts, nextAt)
	}
	// A fresh drain must return NOTHING: DueAlertDeliveries now CLAIMS and leases
	// the rows it returns (SIGMA-106), so the backed-off row is not yet due and
	// the still-'sending' siblings from the first drain are leased out — a sibling
	// replica's drain has nothing to double-send.
	due2, err := st.DueAlertDeliveries(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(due2) != 0 {
		t.Fatalf("second drain re-surfaced %d already-claimed deliveries", len(due2))
	}
	// maxAttempts=1 → the next failure is terminal. Use another delivery the first
	// drain already claimed.
	term := due[1]
	if err := st.SetAlertDeliveryResult(ctx, term.ID, false, "410 gone", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, `SELECT status FROM alert_outbox WHERE id = $1`, term.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("terminal failure status = %q", status)
	}
	// Success path: a third claimed delivery marks the channel healthy.
	okID := due[2].ID
	if err := st.SetAlertDeliveryResult(ctx, okID, true, "", 8); err != nil {
		t.Fatal(err)
	}
	var lastOK *time.Time
	if err := st.Pool.QueryRow(ctx, `SELECT last_ok_at FROM alert_channels WHERE id = $1`, ch.ID).Scan(&lastOK); err != nil {
		t.Fatal(err)
	}
	if lastOK == nil {
		t.Fatal("successful delivery must mark the channel healthy")
	}

	// Rules are the off switch: restrict the channel to deploy_failed only,
	// then a fresh unreachable flip enqueues nothing new.
	if err := st.SetAlertRules(ctx, orgID, ch.ID, []string{store.AlertDeployFailed}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAlertRules(ctx, orgID, ch.ID, []string{"bogus_event"}, "test"); err == nil {
		t.Fatal("unknown event must be rejected")
	}
	if _, err := st.Pool.Exec(ctx, `DELETE FROM alert_outbox WHERE event = $1`, store.AlertServerUnreachable); err != nil {
		t.Fatal(err)
	}
	goSilent()
	if _, err := st.MarkStaleUnreachable(ctx, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	if n := outboxRows(t, st, store.AlertServerUnreachable); n != 0 {
		t.Fatalf("rule filtering broken: unreachable alerts = %d, want 0", n)
	}

	// Cert-expiry producer: an issued cert inside the window alerts once.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO domains (id, org_id, resource_id, domain, challenge_type, cert_status, cert_expires_at, created_by)
		VALUES ('dom_1', $1, $2, 'app.example.com', 'http-01', 'issued', now() + interval '5 days', 'test')`,
		orgID, res.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAlertRules(ctx, orgID, ch.ID, []string{store.AlertCertExpiring}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueCertExpiringAlerts(ctx, 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueCertExpiringAlerts(ctx, 14*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := outboxRows(t, st, store.AlertCertExpiring); n != 1 {
		t.Fatalf("cert_expiring alerts = %d, want exactly 1 across repeated scans", n)
	}

	// Deleting the channel cascades rules and queued deliveries.
	if err := st.DeleteAlertChannel(ctx, orgID, ch.ID, "test"); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM alert_outbox`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("outbox rows after channel delete = %d, want 0", remaining)
	}
}
