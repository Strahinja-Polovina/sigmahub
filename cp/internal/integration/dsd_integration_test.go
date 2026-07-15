// Package integration is the repo's first CP+Postgres integration harness
// (P1-2). It drives the real store, reconciler and API surface against a live
// Postgres and acts as the agent over HTTP, asserting DSD delivery, status
// ingest, replay rejection and resync convergence.
//
// It runs only when CP_TEST_DATABASE_URL points at a throwaway Postgres (CI
// provides one); otherwise it skips, so `go test ./...` stays green locally.
package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

func testStore(t *testing.T) (*store.Store, ed25519.PrivateKey) {
	t.Helper()
	dsn := os.Getenv("CP_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("CP_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Fresh, isolated state per run. cp_secrets is cleared too so the pepper
	// and DSD key are re-wrapped under this run's throwaway KMS key.
	for _, tbl := range []string{"server_dsd", "env_servers", "resources", "environments", "projects", "agent_tokens", "bootstrap_tokens", "service_tokens", "servers", "cp_audit_log", "cp_secrets"} {
		if _, err := st.Pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	custody, err := kms.LoadOrCreateFileCustody(t.TempDir()+"/kms.key", st.AuditUnwrapSink())
	if err != nil {
		t.Fatal(err)
	}
	pepper, err := st.LoadTokenPepper(ctx, custody)
	if err != nil {
		t.Fatal(err)
	}
	st.SetPepper(pepper)
	dsdKey, err := st.LoadDSDSigningKey(ctx, custody)
	if err != nil {
		t.Fatal(err)
	}
	return st, dsdKey
}

// agentGetDSD acts as the agent's long-poll client.
func agentGetDSD(t *testing.T, base, token string, after int64) (dsd.Signed, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/v1/agent/dsd?after="+strconv.FormatInt(after, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get dsd: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return dsd.Signed{}, resp.StatusCode
	}
	var signed dsd.Signed
	if err := json.NewDecoder(resp.Body).Decode(&signed); err != nil {
		t.Fatalf("decode dsd: %v", err)
	}
	return signed, resp.StatusCode
}

func TestDSDDeliveryApplyReplayResync(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := reconciler.New(log, st, dsdKey)

	api.SetLongPollTimeout(400 * time.Millisecond) // keep the no-replay probe fast
	srv := api.New(log, st, st, st, api.Options{
		DevServiceToken: "dev",
		DSDStore:        st,
		DSDWaiter:       rec,
		Reconcile:       rec,
		DSDPublicKey:    dsdKey.Public().(ed25519.PublicKey),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Provision org token, project, env.
	orgID := "org_it"
	svcTok, _, err := st.IssueServiceToken(ctx, orgID, "web", store.RoleOrgAdmin, "test")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Register a server (bootstrap → agent token) and attach it.
	bootTok, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "host", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID, agentTok := reg.Server.ID, reg.AgentToken
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "test"); err != nil {
		t.Fatal(err)
	}

	pubB64 := base64.StdEncoding.EncodeToString(dsdKey.Public().(ed25519.PublicKey))

	// No resources yet → the server has no DSD (version 0); GET ?after=0 must
	// time out to 204 rather than serve an empty document.
	if _, code := agentGetDSD(t, ts.URL, agentTok, 0); code != http.StatusNoContent {
		t.Fatalf("empty server DSD: code=%d, want 204", code)
	}

	// Create a resource via the API (as the agent would see it) → reconcile.
	res := createResourceViaAPI(t, ts.URL, svcTok, orgID, env.ID, serverID, "app1", "app")

	// Delivery: the agent long-polls after=0 and gets v1 within the window.
	signed, code := agentGetDSD(t, ts.URL, agentTok, 0)
	if code != http.StatusOK || signed.Document.Version != 1 {
		t.Fatalf("delivery: code=%d version=%d, want 200 v1", code, signed.Document.Version)
	}
	if err := dsd.Verify(dsdKey.Public().(ed25519.PublicKey), signed); err != nil {
		t.Fatalf("agent could not verify delivered DSD: %v", err)
	}
	if len(signed.Document.Ops) != 1 || signed.Document.Ops[0].ID != "res:"+res {
		t.Fatalf("unexpected ops: %+v", signed.Document.Ops)
	}
	_ = pubB64

	// Agent reports status → CP writes resources.status.
	statusOps := map[string]json.RawMessage{
		"res:" + res: json.RawMessage(`{"state":"applied"}`),
	}
	postDSDStatus(t, ts.URL, agentTok, signed.Document.Version, statusOps)
	if state := resourceStatusState(t, st, res); state != "applied" {
		t.Fatalf("resource status = %q, want applied", state)
	}

	// Replay/no-downgrade: GET ?after=1 must NOT return v1 again (204 within
	// the shortened window).
	if _, code := agentGetDSD(t, ts.URL, agentTok, 1); code != http.StatusNoContent {
		t.Fatalf("replay probe: code=%d, want 204 (no re-delivery of current)", code)
	}

	// Superseded status is ignored: reporting v0 after v1 was accepted must
	// not regress applied_version.
	postDSDStatus(t, ts.URL, agentTok, 0, statusOps)

	// Bogus version rejection: the reported version is agent-supplied, so a
	// report claiming a version above the DSD the CP actually issued must NOT
	// poison applied_version — otherwise every honest report afterwards would
	// read as superseded and permanently freeze this server's status.
	postDSDStatus(t, ts.URL, agentTok, 1<<40, statusOps)
	if av := appliedVersion(t, st, serverID); av != 1 {
		t.Fatalf("over-large status report poisoned applied_version to %d, want 1", av)
	}

	// Resync convergence: delete the resource directly in the store (bypassing
	// the API's event-driven reconcile), then run a reconcile tick (what the
	// 60s resync loop does) — the DSD must bump to v2 with zero ops.
	if _, err := st.DeleteResource(ctx, orgID, res, "test"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	signed2, code := agentGetDSD(t, ts.URL, agentTok, 1)
	if code != http.StatusOK || signed2.Document.Version != 2 || len(signed2.Document.Ops) != 0 {
		t.Fatalf("resync: code=%d version=%d ops=%d, want 200 v2 0-ops", code, signed2.Document.Version, len(signed2.Document.Ops))
	}

	// Idempotent resync: a second tick with unchanged specs must NOT bump.
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.CurrentDSDVersion(ctx, serverID); v != 2 {
		t.Fatalf("idempotent resync bumped version to %d, want 2", v)
	}
}

func createResourceViaAPI(t *testing.T, base, svcTok, orgID, envID, serverID, name, kind string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"environmentId": envID, "serverId": serverID, "name": name, "kind": kind})
	req, _ := http.NewRequest("POST", base+"/v1/orgs/"+orgID+"/resources", io.NopCloser(bytesReader(body)))
	req.Header.Set("Authorization", "Bearer "+svcTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create resource: %d %s", resp.StatusCode, b)
	}
	var r struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	// Give the fire-and-forget ReconcileAsync a moment to render the DSD.
	time.Sleep(200 * time.Millisecond)
	return r.ID
}

func postDSDStatus(t *testing.T, base, agentTok string, version int64, ops map[string]json.RawMessage) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"version": version, "ops": ops})
	req, _ := http.NewRequest("POST", base+"/v1/agent/dsd/status", bytesReader(body))
	req.Header.Set("Authorization", "Bearer "+agentTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("post status: %d", resp.StatusCode)
	}
}

func appliedVersion(t *testing.T, st *store.Store, serverID string) int64 {
	t.Helper()
	var v int64
	if err := st.Pool.QueryRow(context.Background(),
		"SELECT applied_version FROM server_dsd WHERE server_id = $1", serverID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func resourceStatusState(t *testing.T, st *store.Store, resourceID string) string {
	t.Helper()
	var raw []byte
	if err := st.Pool.QueryRow(context.Background(),
		"SELECT status FROM resources WHERE id = $1", resourceID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var s struct {
		State string `json:"state"`
	}
	json.Unmarshal(raw, &s)
	return s.State
}
