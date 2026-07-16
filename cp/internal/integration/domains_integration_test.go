package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestDomainsLifecycle exercises the P1-8 domain store against a real Postgres:
// attach → server-scoped listing → cert-status ingest (idempotent) → BOLA scope
// → detach.
func TestDomainsLifecycle(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_dom"

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
	// A second server, to prove the cert-status ingest is server-scoped (BOLA).
	bootTok2, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host2", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg2, err := st.RegisterServer(ctx, bootTok2, "host2", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	otherServerID := reg2.Server.ID

	appSpec, _ := json.Marshal(map[string]any{"image": "nginx", "ports": []map[string]any{{"container": 8080}}})
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "web", Kind: "app", Spec: appSpec,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Only an app can take a domain.
	if _, _, err := st.AttachDomain(ctx, orgID, "res_nope", "x.example.com", "http", "test"); err == nil {
		t.Fatal("attaching to a missing resource must fail")
	}

	dom, srvID, err := st.AttachDomain(ctx, orgID, res.ID, "App.Example.COM", "http", "test")
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if srvID != serverID {
		t.Fatalf("attach returned server %q, want %q", srvID, serverID)
	}
	if dom.Domain != "app.example.com" {
		t.Errorf("domain not lowercased: %q", dom.Domain)
	}

	// Duplicate (any case) → conflict.
	if _, _, err := st.AttachDomain(ctx, orgID, res.ID, "app.example.com", "http", "test"); err == nil {
		t.Fatal("duplicate domain must conflict")
	}

	// DomainsForServer keys by resource.
	byServer, err := st.DomainsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if len(byServer[res.ID]) != 1 || byServer[res.ID][0].Domain != "app.example.com" {
		t.Fatalf("DomainsForServer = %+v", byServer)
	}

	// Cert-status ingest from the hosting server.
	exp := time.Now().Add(60 * 24 * time.Hour)
	if err := st.SetDomainCertStatus(ctx, serverID, "app.example.com", "issued", "SERIAL123", &exp, ""); err != nil {
		t.Fatalf("cert status: %v", err)
	}
	// Idempotent re-report of the same serial.
	if err := st.SetDomainCertStatus(ctx, serverID, "app.example.com", "issued", "SERIAL123", &exp, ""); err != nil {
		t.Fatalf("idempotent cert status: %v", err)
	}
	got, err := st.ListDomainsForResource(ctx, orgID, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CertStatus != "issued" || got[0].CertSerial != "SERIAL123" {
		t.Fatalf("domain after issue = %+v", got)
	}

	// BOLA: the OTHER server must not be able to write this domain's cert state.
	if err := st.SetDomainCertStatus(ctx, otherServerID, "app.example.com", "failed", "EVIL", nil, "boom"); err == nil {
		t.Fatal("a non-hosting server must not update cert status (BOLA)")
	}
	// Unchanged by the rejected write.
	got, _ = st.ListDomainsForResource(ctx, orgID, res.ID)
	if got[0].CertStatus != "issued" || got[0].CertSerial != "SERIAL123" {
		t.Fatalf("cert state changed by a non-hosting server: %+v", got)
	}

	// Detach returns the host server (for reconcile) and removes the row.
	dsrv, err := st.DetachDomain(ctx, orgID, dom.ID, "test")
	if err != nil {
		t.Fatalf("detach: %v", err)
	}
	if dsrv != serverID {
		t.Errorf("detach returned server %q, want %q", dsrv, serverID)
	}
	got, _ = st.ListDomainsForResource(ctx, orgID, res.ID)
	if len(got) != 0 {
		t.Fatalf("domain still present after detach: %+v", got)
	}
}
