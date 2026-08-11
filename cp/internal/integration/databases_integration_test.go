package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// dbTestFixture creates org → project → env → registered server → attachment
// and returns the ids the database tests need.
func dbTestFixture(t *testing.T, st *store.Store, orgID string, production bool, serverType string) (envID, serverID string) {
	t.Helper()
	ctx := context.Background()
	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", production, "test")
	if err != nil {
		t.Fatal(err)
	}
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "dbhost", serverType, "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "dbhost", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AttachServer(ctx, orgID, env.ID, reg.Server.ID, "test"); err != nil {
		t.Fatal(err)
	}
	return env.ID, reg.Server.ID
}

// TestDatabaseProvisioning exercises the P1-10 store path against a real
// Postgres: create → credentials + backup-policy rows in one transaction →
// member metadata → audited reveal → agent resolve returning the same secret →
// render targets → per-server port allocation.
func TestDatabaseProvisioning(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_db"
	envID, serverID := dbTestFixture(t, st, orgID, true, "database")

	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "Shop DB", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatalf("create postgres resource: %v", err)
	}

	// Member-visible metadata: engine identity, mesh host, allocated port, and
	// the production-derived backup policy — but never a password.
	info, err := st.GetDatabaseInfo(ctx, orgID, res.ID)
	if err != nil {
		t.Fatalf("get database info: %v", err)
	}
	if info.Engine != "postgres" || info.Username != "sigma" || info.Database != "shop_db" {
		t.Fatalf("info = %+v", info)
	}
	if info.Port != 15000 || info.Host == "" || !strings.HasPrefix(info.Host, "10.8.") {
		t.Fatalf("mesh binding = %s:%d, want mesh IP and base port", info.Host, info.Port)
	}
	if info.Backup == nil || info.Backup.KeepDaily != 30 || info.Backup.KeepWeekly != 0 {
		t.Fatalf("production backup policy = %+v, want daily/30", info.Backup)
	}
	if info.Backup.TargetID != nil {
		t.Fatal("new policy must have no backup target (P1-11 owns targets)")
	}

	// Audited reveal returns the decrypted credential and canonical URL.
	conn, err := st.RevealDatabaseConnection(ctx, orgID, res.ID, "admin@test")
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if len(conn.Password) < 16 {
		t.Fatalf("weak/missing password %q", conn.Password)
	}
	wantURL := "postgresql://sigma:" + conn.Password + "@" + info.Host + ":15000/shop_db"
	if conn.URL != wantURL {
		t.Fatalf("url = %q, want %q", conn.URL, wantURL)
	}
	audits, err := st.ListAudit(ctx, orgID, 50)
	if err != nil {
		t.Fatal(err)
	}
	foundReveal := false
	for _, a := range audits {
		if a.Action == "DB credentials revealed" && a.Target == res.ID && a.Actor == "admin@test" {
			foundReveal = true
		}
	}
	if !foundReveal {
		t.Fatal("reveal must write an audit row")
	}

	// The agent resolve channel returns the SAME password as an env-mode secret.
	resolved, err := st.ResolveSecretsForResource(ctx, orgID, serverID, res.ID, "agent:"+serverID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	var pgPass string
	for _, s := range resolved {
		if s.Name == "POSTGRES_PASSWORD" && s.EnvVar {
			pgPass = s.Value
		}
	}
	if pgPass != conn.Password {
		t.Fatalf("agent-resolved password %q != revealed %q", pgPass, conn.Password)
	}
	// BOLA guard holds for db credentials too: another server cannot drain them.
	if _, err := st.ResolveSecretsForResource(ctx, orgID, "srv_other", res.ID, "agent:other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign server resolve must 404, got %v", err)
	}

	// Render targets feed the reconciler; the server type drives tuning.
	targets, err := st.DBTargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	tgt, ok := targets[res.ID]
	if !ok || tgt.Engine != "postgres" || tgt.Port != 15000 || tgt.ServerType != "database" {
		t.Fatalf("db target = %+v", tgt)
	}

	// A second database on the same server gets the next port.
	res2, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "cache", Kind: "redis", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	info2, err := st.GetDatabaseInfo(ctx, orgID, res2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info2.Port != 15001 {
		t.Fatalf("second db port = %d, want 15001", info2.Port)
	}

	// Deleting the resource cascades its credentials and policy.
	if _, err := st.DeleteResource(ctx, orgID, res2.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDatabaseInfo(ctx, orgID, res2.ID); !errors.Is(err, store.ErrNotDatabase) {
		t.Fatalf("deleted db info err = %v, want ErrNotDatabase", err)
	}
}

// TestDatabaseEngineGate verifies the Postgres-only fallback is a pure
// configuration cut: disabled engines refuse creation with a typed domain
// error while enabled ones keep working.
func TestDatabaseEngineGate(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_gate"
	envID, serverID := dbTestFixture(t, st, orgID, false, "database")

	st.SetEnabledDBEngines([]string{"postgres"})
	defer st.SetEnabledDBEngines([]string{"postgres", "mysql", "redis", "mongodb"})

	_, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "m", Kind: "mysql", Spec: json.RawMessage(`{}`),
	}, "test")
	var inv store.ErrInvalid
	if !errors.As(err, &inv) || !strings.Contains(inv.Msg, "not enabled") {
		t.Fatalf("disabled engine create err = %v, want typed not-enabled", err)
	}

	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "pg", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatalf("postgres-only build must still provision postgres: %v", err)
	}
	info, err := st.GetDatabaseInfo(ctx, orgID, res.ID)
	if err != nil || info.Engine != "postgres" {
		t.Fatalf("info = %+v err = %v", info, err)
	}
	// Non-production environment gets the GFS 7/4/6 default.
	if info.Backup == nil || info.Backup.KeepDaily != 7 || info.Backup.KeepWeekly != 4 || info.Backup.KeepMonthly != 6 {
		t.Fatalf("gfs default = %+v", info.Backup)
	}
}

// TestMeshPortsAreReclaimed is the SIGMA-352 guard.
//
// The allocator was MAX(port) + 1 across the three port-owning tables, so a
// port freed by deleting a resource was gone for the life of the server. A
// fleet that creates and destroys — a preview-heavy project, anyone iterating
// in the wizard — walked the number upward forever, and with no ceiling check
// it would eventually hand the agent a number above 65535 that cannot bind.
func TestMeshPortsAreReclaimed(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_ports"
	envID, serverID := dbTestFixture(t, st, orgID, true, "database")

	mk := func(name string) (string, int) {
		t.Helper()
		res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
			EnvironmentID: envID, ServerID: serverID, Name: name, Kind: "postgres",
			Spec: json.RawMessage(`{}`),
		}, "admin")
		if err != nil {
			t.Fatal(err)
		}
		var port int
		if err := st.Pool.QueryRow(ctx,
			`SELECT port FROM db_credentials WHERE resource_id = $1`, res.ID).Scan(&port); err != nil {
			t.Fatal(err)
		}
		return res.ID, port
	}

	// Three in a row: the range is dense from the base.
	_, p1 := mk("one")
	id2, p2 := mk("two")
	_, p3 := mk("three")
	if p1 != store.MeshPortBase || p2 != store.MeshPortBase+1 || p3 != store.MeshPortBase+2 {
		t.Fatalf("ports = %d, %d, %d; want %d, %d, %d",
			p1, p2, p3, store.MeshPortBase, store.MeshPortBase+1, store.MeshPortBase+2)
	}

	// Free the middle one. The next create must take the hole, not climb past
	// the high-water mark.
	if _, err := st.DeleteResource(ctx, orgID, id2, "admin"); err != nil {
		t.Fatal(err)
	}
	_, p4 := mk("four")
	if p4 != p2 {
		t.Fatalf("after freeing port %d the next allocation took %d — a deleted resource's port "+
			"must come back, or the range climbs until it runs off the end of 65535", p2, p4)
	}
}
