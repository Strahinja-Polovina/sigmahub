package integration

// SIGMA-284: the control plane could create a tenant and never erase one.
//
// POST /v1/orgs mints an org; from there the only deletes were per-object. A
// design partner ending a trial, or a GDPR Art. 17 erasure request reaching the
// CP's own personal data — cp_audit_log's actor display names, alert_channels'
// SMTP credentials and recipient addresses, dns_provider_credentials' API
// tokens for the customer's own DNS account — had no code path at all. The
// operator's alternative was a hand-written DELETE tour of ~40 tables in
// foreign-key order, in a schema that gains tables every migration.
//
// The test is written against the SCHEMA rather than against a table list,
// because a list is the defect one migration later: it asserts that NO table
// carrying an org_id has a row left for the purged org. A table added next
// quarter fails this test if PurgeOrg misses it.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// seedTenant builds an org with enough of the product in it that the purge has
// to cross most of the foreign-key graph: a registered server, a project with
// an environment and a resource, a deployment with build-log lines, a cluster,
// a git connection, secrets (which mint the org DEK), an alert channel, DNS
// provider credentials and audit rows.
func seedTenant(t *testing.T, st *store.Store, orgID string) string {
	t.Helper()
	ctx := context.Background()

	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host-"+orgID, "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host-"+orgID, "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID := reg.Server.ID

	proj, err := st.CreateProject(ctx, orgID, "shop", "", "test")
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
	// A secret, so the org's DEK exists — org_deks is referenced with NO ACTION
	// from half a dozen tables and is the one row the purge can only delete last.
	if _, err := st.CreateSecret(ctx, orgID, "test", store.CreateSecretInput{
		ProjectID: proj.ID, EnvironmentID: env.ID, Name: "DB_PASSWORD", Value: "hunter2",
	}); err != nil {
		t.Fatal(err)
	}
	// An alert channel: the CP's own PII, a recipient address plus a credential.
	if _, err := st.CreateAlertChannel(ctx, orgID, "test", store.CreateAlertChannelInput{
		// A real hooks.slack.com address, not a placeholder host: SIGMA-259 pinned
		// the Slack prefix at create time, so an example.com stand-in is rejected
		// before the row this test needs to purge ever exists.
		Kind: "slack", Name: "ops", Secret: "https://hooks.slack.com/services/T000/B000/xxxx",
	}); err != nil {
		t.Fatal(err)
	}

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	seed(`INSERT INTO deploy_logs (deployment_id, stream, line) VALUES ($1, 'build', 'step 1/3')`, dep.ID)
	seed(`INSERT INTO dns_provider_credentials (id, org_id, provider, token_wrapped, created_by)
	      VALUES ($1, $2, 'cloudflare', '\x00', 'test')`, "dns_"+orgID, orgID)
	seed(`INSERT INTO cp_audit_log (org_id, actor, action, target)
	      VALUES ($1, 'Dana Reeve', 'Secret revealed', 'DB_PASSWORD')`, orgID)
	seed(`INSERT INTO github_installations (installation_id, org_id) VALUES ($1, $2)`, "inst_"+orgID, orgID)
	seed(`INSERT INTO server_hours (org_id, server_id, hour)
	      VALUES ($1, $2, date_trunc('hour', now()))`, orgID, serverID)
	seed(`INSERT INTO org_billing (org_id, paddle_customer_id, status)
	      VALUES ($1, 'ctm_x', 'active')`, orgID)
	seed(`INSERT INTO org_registries (org_id, host, namespace, username, password_wrapped, created_by)
	      VALUES ($1, 'ghcr.io', 'acme', 'bot', '\x00', 'test')`, orgID)
	if _, _, err := st.IssueServiceToken(ctx, orgID, "web", store.RoleOrgAdmin, "provisioner"); err != nil {
		t.Fatal(err)
	}
	return serverID
}

// orgScopedTableNames is the test's own copy of the discovery PurgeOrg does, so
// the assertion is independent of the implementation it is checking.
func orgScopedTableNames(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.Pool.Query(context.Background(), `
		SELECT c.table_name FROM information_schema.columns c
		  JOIN information_schema.tables tb
		    ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		 WHERE c.table_schema = current_schema() AND c.column_name = 'org_id'
		   AND tb.table_type = 'BASE TABLE'
		 ORDER BY c.table_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func TestPurgeOrgErasesEveryOrgScopedRow(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	const doomed = "org_partner"
	const keeper = "org_neighbour"
	seedTenant(t, st, doomed)
	seedTenant(t, st, keeper)

	tables := orgScopedTableNames(t, st)
	if len(tables) < 20 {
		t.Fatalf("discovered only %d org-scoped tables (%v); the schema query is wrong", len(tables), tables)
	}
	// The seed has to actually reach a broad slice of them, or "nothing left"
	// would be trivially true and the test would pass on an empty database.
	seeded := 0
	for _, tbl := range tables {
		if countRows(t, st, `SELECT count(*) FROM "`+tbl+`" WHERE org_id = $1`, doomed) > 0 {
			seeded++
		}
	}
	if seeded < 12 {
		t.Fatalf("fixture only populated %d of %d org-scoped tables; it is not exercising the purge", seeded, len(tables))
	}

	deleted, err := st.PurgeOrg(ctx, doomed)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(deleted) < 12 {
		t.Fatalf("purge reported deleting from %d tables, want at least 12: %v", len(deleted), deleted)
	}

	for _, tbl := range tables {
		if n := countRows(t, st, `SELECT count(*) FROM "`+tbl+`" WHERE org_id = $1`, doomed); n != 0 {
			t.Errorf("%s still holds %d rows for the purged org", tbl, n)
		}
	}

	// Cascade-reachable tables carry no org_id, so the loop above cannot see
	// them. An orphan here is a lost ON DELETE CASCADE — a partial erasure that
	// would otherwise report success.
	left, err := st.PurgeOrgLeftovers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("rows orphaned by the purge instead of cascaded: %v", left)
	}

	// The neighbouring tenant is untouched — a purge that takes the whole table
	// with it is the other way to fail this.
	neighbour := 0
	for _, tbl := range tables {
		neighbour += countRows(t, st, `SELECT count(*) FROM "`+tbl+`" WHERE org_id = $1`, keeper)
	}
	if neighbour < seeded {
		t.Errorf("the other tenant lost rows: %d remain across %d tables", neighbour, len(tables))
	}
	if n := countRows(t, st, `SELECT count(*) FROM cp_audit_log WHERE org_id = $1`, keeper); n == 0 {
		t.Error("the other tenant's audit log was erased too")
	}
}

func TestPurgeOrgRefusesEmptyOrg(t *testing.T) {
	st, _ := testStore(t)
	// An empty org id would match nothing and delete nothing, but a caller that
	// reached here has a bug and a purge is not the place to be forgiving.
	if _, err := st.PurgeOrg(context.Background(), ""); err == nil {
		t.Fatal("PurgeOrg(\"\") returned no error")
	}
}
