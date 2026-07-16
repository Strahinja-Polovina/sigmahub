package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestDatabaseProvisioning pins the P1-10 store path against real Postgres:
// creating a DB resource generates envelope-encrypted credentials and the
// backup-policy hook row in the same tx; the agent secret fetch carries the
// password as an env-mode secret; the reveal returns a working-shaped
// connection string with the mesh host and writes an audit row.
func TestDatabaseProvisioning(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_db"

	proj, _ := st.CreateProject(ctx, orgID, "p", "", "test")
	env, _ := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	bootTok, _, _, _ := st.IssueBootstrapToken(ctx, orgID, "dbhost", "database", "", "", "test", time.Hour)
	reg, err := st.RegisterServer(ctx, bootTok, "dbhost", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID := reg.Server.ID
	_ = st.AttachServer(ctx, orgID, env.ID, serverID, "test")

	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "maindb", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatalf("create postgres resource: %v", err)
	}

	// Credentials generated + decryptable; identity is deterministic.
	creds, err := st.DBCredentialsForResource(ctx, orgID, res.ID)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.Engine != "postgres" || creds.Database != "app" || creds.Password == "" {
		t.Fatalf("creds = %+v", creds)
	}

	// Production environment → daily backup policy with 14d retention.
	pol, err := st.BackupPolicyForResource(ctx, orgID, res.ID)
	if err != nil {
		t.Fatalf("backup policy: %v", err)
	}
	if pol.Schedule != "0 3 * * *" || pol.RetentionDays != 14 || !pol.Enabled {
		t.Fatalf("prod backup policy = %+v", pol)
	}

	// The agent's secret fetch (server-scoped) includes the password env ref.
	secrets, err := st.ResolveSecretsForResource(ctx, orgID, serverID, res.ID, "agent")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range secrets {
		if s.Name == "POSTGRES_PASSWORD" && s.EnvVar && s.Value == creds.Password {
			found = true
		}
	}
	if !found {
		t.Fatalf("agent secrets missing POSTGRES_PASSWORD: %+v", secrets)
	}

	// Reveal renders the engine connection string and audits.
	conn, err := st.RevealDBConnection(ctx, orgID, res.ID, "admin-user")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(conn, "postgresql://") || !strings.Contains(conn, creds.Password) {
		t.Fatalf("connection string = %q", conn)
	}
	if n := auditCount(t, st, orgID, "admin-user"); n == 0 {
		t.Fatal("reveal must write an audit row")
	}

	// A cross-org read fails closed.
	if _, err := st.DBCredentialsForResource(ctx, "org_other", res.ID); err != store.ErrNotFound {
		t.Fatalf("cross-org creds read must be ErrNotFound, got %v", err)
	}
}
