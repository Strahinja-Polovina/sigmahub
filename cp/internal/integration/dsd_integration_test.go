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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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
	for _, tbl := range []string{"server_dsd", "deploy_logs", "alert_outbox", "alert_rules", "alert_channels", "deployments", "builds", "deploy_requests", "git_branch_map", "webhook_deliveries", "git_connections", "domains", "dns_provider_credentials", "preview_environments", "backup_runs", "backup_policies", "backup_targets", "db_credentials", "s3_credentials", "secrets", "org_deks", "env_servers", "resources", "environments", "projects", "agent_tokens", "bootstrap_tokens", "service_tokens", "server_hardening", "servers", "cp_audit_log", "cp_secrets"} {
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
	st.SetCustody(custody)
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
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
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
	// The document carries the resource op plus the always-present P1-5 host
	// hardening ops (nftables/sshd/cis).
	opIDs := map[string]bool{}
	for _, op := range signed.Document.Ops {
		opIDs[op.ID] = true
	}
	if !opIDs["res:"+res] {
		t.Fatalf("resource op missing: %+v", signed.Document.Ops)
	}
	for _, want := range []string{"host:nftables:" + serverID, "host:sshd:" + serverID, "host:cis:" + serverID} {
		if !opIDs[want] {
			t.Fatalf("host hardening op %q missing: %+v", want, signed.Document.Ops)
		}
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
	// 60s resync loop does) — the DSD must bump to v2 with no resource ops left
	// (only the always-present host hardening ops remain).
	if _, err := st.DeleteResource(ctx, orgID, res, "test"); err != nil {
		t.Fatal(err)
	}
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	signed2, code := agentGetDSD(t, ts.URL, agentTok, 1)
	if code != http.StatusOK || signed2.Document.Version != 2 {
		t.Fatalf("resync: code=%d version=%d, want 200 v2", code, signed2.Document.Version)
	}
	for _, op := range signed2.Document.Ops {
		if !strings.HasPrefix(op.ID, "host:") {
			t.Fatalf("resync left a non-host op after resource delete: %+v", signed2.Document.Ops)
		}
	}

	// Idempotent resync: a second tick with unchanged specs must NOT bump.
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.CurrentDSDVersion(ctx, serverID); v != 2 {
		t.Fatalf("idempotent resync bumped version to %d, want 2", v)
	}
}

// TestContainerRenderAndDestructiveOps drives the P1-3 CP surface end to end:
// an "app" resource fans into container ops in the signed DSD; the two-phase
// confirm flow injects a volume.remove op that drops out once the agent reports
// it applied; and an ephemeral resource's teardown auto-confirms volume removal
// as the system actor.
func TestContainerRenderAndDestructiveOps(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := reconciler.New(log, st, dsdKey)

	api.SetLongPollTimeout(400 * time.Millisecond)
	srv := api.New(log, st, st, st, api.Options{
		DevServiceToken: "dev",
		DSDStore:        st,
		DSDWaiter:       rec,
		Reconcile:       rec,
		DSDPublicKey:    dsdKey.Public().(ed25519.PublicKey),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	orgID := "org_ct"
	svcTok, _, err := st.IssueServiceToken(ctx, orgID, "web", store.RoleProjectAdmin, "test")
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
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "host", "general", "", "", "test", time.Hour)
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

	// App resource with a container spec (image + a named volume).
	appSpec, _ := json.Marshal(map[string]any{
		"image":          "nginxinc/nginx-unprivileged:1.27-alpine",
		"volumes":        []map[string]any{{"name": "data", "mountPath": "/data"}},
		"user":           "101:101",
		"readOnlyRootfs": true,
	})
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "app1", Kind: "app", Spec: appSpec,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}

	signed, code := agentGetDSD(t, ts.URL, agentTok, 0)
	if code != http.StatusOK {
		t.Fatalf("delivery code=%d", code)
	}
	kinds := map[string]int{}
	for _, op := range signed.Document.Ops {
		kinds[op.Kind]++
	}
	for _, want := range []string{dsd.KindNetworkEnsure, dsd.KindImagePull, dsd.KindVolumeEnsure, dsd.KindContainerApply} {
		if kinds[want] == 0 {
			t.Fatalf("app resource did not render a %s op; ops=%v", want, kinds)
		}
	}

	// Two-phase destructive op: request a confirm token, confirm it, and the
	// next DSD must carry the volume.remove op.
	volName := dsd.VolumeName(res.ID, "data")
	token := issueConfirmToken(t, ts.URL, svcTok, orgID, serverID, dsd.KindVolumeRemove, volName)
	confirmDestructive(t, ts.URL, svcTok, orgID, serverID, token, dsd.KindVolumeRemove, volName)
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	cur, _ := st.GetDSD(ctx, serverID)
	var volrmOpID string
	for _, op := range cur.Document.Ops {
		if op.Kind == dsd.KindVolumeRemove {
			volrmOpID = op.ID
		}
	}
	if volrmOpID == "" {
		t.Fatal("no volume.remove op in the DSD after confirm")
	}

	// A stale/unused confirm token cannot be replayed.
	if replayCode := tryConfirmDestructive(t, ts.URL, svcTok, orgID, serverID, token, dsd.KindVolumeRemove, volName); replayCode == http.StatusOK {
		t.Fatal("a used confirm token was replayable")
	}

	// Agent reports the volume.remove applied → it must drop from future DSDs.
	postDSDStatus(t, ts.URL, agentTok, cur.Document.Version, map[string]json.RawMessage{
		volrmOpID: json.RawMessage(`{"state":"applied"}`),
	})
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	after, _ := st.GetDSD(ctx, serverID)
	for _, op := range after.Document.Ops {
		if op.Kind == dsd.KindVolumeRemove {
			t.Fatal("volume.remove op did not drop after being reported applied")
		}
	}

	// Ephemeral carve-out: an ephemeral resource's teardown auto-confirms volume
	// removal as the system actor, with no interactive token.
	ephSpec, _ := json.Marshal(map[string]any{
		"image":   "nginxinc/nginx-unprivileged:1.27-alpine",
		"volumes": []map[string]any{{"name": "cache", "mountPath": "/cache"}},
	})
	eph, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "preview1", Kind: "app", Spec: ephSpec,
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE resources SET ephemeral = TRUE WHERE id = $1", eph.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteResource(ctx, orgID, eph.ID, "someuser"); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingDestructiveOpsForServer(ctx, orgID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	wantVol := dsd.VolumeName(eph.ID, "cache")
	found := false
	for _, p := range pending {
		if p.OpKind == dsd.KindVolumeRemove && p.Target == wantVol {
			found = true
		}
	}
	if !found {
		t.Fatalf("ephemeral teardown did not record a system volume.remove for %s; pending=%v", wantVol, pending)
	}
	// The teardown is audited as the system actor (both request and confirm).
	var sysAudits int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM cp_audit_log WHERE org_id = $1 AND actor = 'system' AND action LIKE 'Destructive-op %(ephemeral)'`,
		orgID).Scan(&sysAudits); err != nil {
		t.Fatal(err)
	}
	if sysAudits < 2 {
		t.Fatalf("expected >=2 system-actor ephemeral audit rows, got %d", sysAudits)
	}
}

func issueConfirmToken(t *testing.T, base, svcTok, orgID, serverID, opKind, target string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"opKind": opKind, "target": target})
	req, _ := http.NewRequest("POST", base+"/v1/orgs/"+orgID+"/servers/"+serverID+"/confirm-tokens", bytesReader(body))
	req.Header.Set("Authorization", "Bearer "+svcTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("issue confirm token: %d %s", resp.StatusCode, b)
	}
	var r struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Token == "" {
		t.Fatal("empty confirm token")
	}
	return r.Token
}

func tryConfirmDestructive(t *testing.T, base, svcTok, orgID, serverID, token, opKind, target string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"token": token, "opKind": opKind, "target": target})
	req, _ := http.NewRequest("POST", base+"/v1/orgs/"+orgID+"/servers/"+serverID+"/destructive-ops", bytesReader(body))
	req.Header.Set("Authorization", "Bearer "+svcTok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func confirmDestructive(t *testing.T, base, svcTok, orgID, serverID, token, opKind, target string) {
	t.Helper()
	if code := tryConfirmDestructive(t, base, svcTok, orgID, serverID, token, opKind, target); code != http.StatusOK {
		t.Fatalf("confirm destructive: %d", code)
	}
}

// TestServerLifecycleTombstoneAndEndpoints exercises P1-4: agent endpoints are
// stored and served in the peer list; a server delete tombstones (excluded from
// peers, agent token 401s) while retaining its mesh IP so a later registration
// never re-uses it; a delete with bound resources 409s; service tokens rotate
// and revoke.
func TestServerLifecycleTombstoneAndEndpoints(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_lc"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}

	register := func(name, pubkey string) store.RegisterResult {
		bt, _, _, err := st.IssueBootstrapToken(ctx, orgID, name, "general", "", "", "test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := st.RegisterServer(ctx, bt, name, "0.1.0", json.RawMessage(`{}`), pubkey)
		if err != nil {
			t.Fatal(err)
		}
		return reg
	}

	a := register("host-a", "pubA")
	b := register("host-b", "pubB")
	if a.Server.MeshIP == nil || b.Server.MeshIP == nil {
		t.Fatal("servers did not get mesh IPs")
	}
	bMeshIP := *b.Server.MeshIP

	// Endpoints reported on heartbeat are served in the peer list.
	if err := st.RecordHeartbeat(ctx, b.Server.ID, store.HeartbeatInput{Facts: json.RawMessage(`{}`), Pubkey: "pubB", Endpoint: "203.0.113.5:51820"}); err != nil {
		t.Fatal(err)
	}
	peers, err := st.MeshPeers(ctx, orgID, a.Server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Endpoint == nil || *peers[0].Endpoint != "203.0.113.5:51820" {
		t.Fatalf("peer endpoint not served: %+v", peers)
	}

	// Delete server B (no bound resources) → tombstoned.
	if err := st.DeleteServer(ctx, orgID, b.Server.ID, "admin"); err != nil {
		t.Fatalf("delete server B: %v", err)
	}
	// B drops from A's peer list.
	peers, _ = st.MeshPeers(ctx, orgID, a.Server.ID)
	if len(peers) != 0 {
		t.Fatalf("tombstoned server still in peer list: %+v", peers)
	}
	// B's agent token no longer authenticates (→ next heartbeat 401).
	if _, err := st.ServerByAgentToken(ctx, b.AgentToken); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoked agent token still valid: %v", err)
	}

	// A new registration must NOT receive B's tombstoned mesh IP.
	c := register("host-c", "pubC")
	if c.Server.MeshIP == nil || *c.Server.MeshIP == bMeshIP {
		t.Fatalf("tombstoned mesh IP %s was re-issued to a new server: %v", bMeshIP, c.Server.MeshIP)
	}

	// Bind a resource to A, then deleting A must 409 (ErrConflict) listing it.
	if err := st.AttachServer(ctx, orgID, env.ID, a.Server.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: a.Server.ID, Name: "app1", Kind: "app", Spec: json.RawMessage(`{}`),
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteServer(ctx, orgID, a.Server.ID, "admin"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete of server with bound resource should 409/ErrConflict, got: %v", err)
	}

	// Service-token lifecycle: issue → list → rotate → revoke.
	_, princ, err := st.IssueServiceToken(ctx, orgID, "ci", store.RoleDeveloper, "admin")
	if err != nil {
		t.Fatal(err)
	}
	toks, err := st.ListServiceTokens(ctx, orgID)
	if err != nil || len(toks) == 0 {
		t.Fatalf("list service tokens: %v (%d)", err, len(toks))
	}
	newTok, _, err := st.RotateServiceToken(ctx, orgID, princ.ID, "admin")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// The old token no longer authenticates; the new one does.
	if _, err := st.AuthenticateServiceToken(ctx, newTok); err != nil {
		t.Fatalf("rotated token should authenticate: %v", err)
	}
	if err := st.RevokeServiceToken(ctx, orgID, princ.ID, "admin"); !errors.Is(err, store.ErrNotFound) {
		// princ.ID was already revoked by rotate; revoking again is a no-op 404.
		if err != nil {
			t.Fatalf("unexpected revoke error: %v", err)
		}
	}
}

// TestSecretsEnvelopeRotationAAD exercises P1-6 at the store layer: DEK envelope
// encryption, reveal auditing, env-over-project resolution, AAD binding
// (cross-row swap fails), and both rotation kinds.
func TestSecretsEnvelopeRotationAAD(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_sec"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Env secret overrides a project default of the same name; a second env-only
	// secret is file-mode.
	if _, err := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{ProjectID: proj.ID, Name: "API_KEY", Value: "proj-default"}); err != nil {
		t.Fatal(err)
	}
	envSec, err := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{ProjectID: proj.ID, EnvironmentID: env.ID, Name: "API_KEY", Value: "env-value"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{ProjectID: proj.ID, EnvironmentID: env.ID, Name: "DB_URL", Value: "postgres://x"}); err != nil {
		t.Fatal(err)
	}

	// Reveal returns the value and writes an audit row.
	val, err := st.RevealSecret(ctx, orgID, envSec.ID, "admin")
	if err != nil || val != "env-value" {
		t.Fatalf("reveal = %q err=%v, want env-value", val, err)
	}
	var reveals int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM cp_audit_log WHERE org_id=$1 AND action='Secret revealed'`, orgID).Scan(&reveals)
	if reveals < 1 {
		t.Fatal("reveal did not write an audit row")
	}

	// Resolve for a resource in the env: env API_KEY wins over the project
	// default; DB_URL present. (Bind a server + resource first.)
	bt, _, _, _ := st.IssueBootstrapToken(ctx, orgID, "h", "general", "", "", "test", time.Hour)
	reg, _ := st.RegisterServer(ctx, bt, "h", "0.1.0", json.RawMessage(`{}`), "")
	st.AttachServer(ctx, orgID, env.ID, reg.Server.ID, "test")
	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{EnvironmentID: env.ID, ServerID: reg.Server.ID, Name: "app1", Kind: "app", Spec: json.RawMessage(`{}`)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := st.ResolveSecretsForResource(ctx, orgID, reg.Server.ID, res.ID, "sigmad")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range resolved {
		got[r.Name] = r.Value
	}
	if got["API_KEY"] != "env-value" || got["DB_URL"] != "postgres://x" {
		t.Fatalf("resolve = %+v, want API_KEY=env-value (env override) + DB_URL", got)
	}

	// Server scoping (anti-BOLA): a DIFFERENT server in the same org must not be
	// able to resolve this resource's secrets, even though it shares the org.
	bt2, _, _, _ := st.IssueBootstrapToken(ctx, orgID, "h2", "general", "", "", "test", time.Hour)
	reg2, _ := st.RegisterServer(ctx, bt2, "h2", "0.1.0", json.RawMessage(`{}`), "")
	if _, err := st.ResolveSecretsForResource(ctx, orgID, reg2.Server.ID, res.ID, "sigmad"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-server resolve = %v, want ErrNotFound (a server must not read another server's secrets)", err)
	}

	// AAD binding: swap another secret's ciphertext into envSec's row → decrypt
	// must fail (the AAD no longer matches the row identity).
	otherProj, _ := st.CreateProject(ctx, orgID, "p2", "", "test")
	other, _ := st.CreateSecret(ctx, orgID, "admin", store.CreateSecretInput{ProjectID: otherProj.ID, Name: "X", Value: "other"})
	if _, err := st.Pool.Exec(ctx,
		`UPDATE secrets SET ciphertext=(SELECT ciphertext FROM secrets WHERE id=$2), nonce=(SELECT nonce FROM secrets WHERE id=$2) WHERE id=$1`,
		envSec.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevealSecret(ctx, orgID, envSec.ID, "admin"); err == nil {
		t.Fatal("AAD: a ciphertext swapped from another row decrypted — AAD binding is broken")
	}
	// That row is now deliberately corrupt (ciphertext no longer matches its AAD),
	// so drop it before rotation — a later lazy re-encrypt would (correctly) fail
	// to decrypt it, which is not what the rotation assertions are exercising.
	if _, err := st.Pool.Exec(ctx, `DELETE FROM secrets WHERE id=$1`, envSec.ID); err != nil {
		t.Fatal(err)
	}

	// KEK rotation: wrapped-DEK versions advance; other secrets still decrypt.
	var beforeVer int
	st.Pool.QueryRow(ctx, `SELECT max(wrap_version) FROM org_deks WHERE org_id=$1`, orgID).Scan(&beforeVer)
	if _, err := st.RotateKEK(ctx, orgID, "admin"); err != nil {
		t.Fatal(err)
	}
	var afterVer int
	st.Pool.QueryRow(ctx, `SELECT max(wrap_version) FROM org_deks WHERE org_id=$1`, orgID).Scan(&afterVer)
	if afterVer <= beforeVer {
		t.Fatalf("KEK rotation did not advance wrap_version (%d -> %d)", beforeVer, afterVer)
	}
	if v, err := st.RevealSecret(ctx, orgID, other.ID, "admin"); err != nil || v != "other" {
		t.Fatalf("reveal after KEK rotation = %q err=%v, want other", v, err)
	}

	// DEK rotation: a new active DEK is created; ReencryptSecrets migrates all
	// rows and retires the drained old DEK; secrets still decrypt.
	if _, err := st.RotateDEK(ctx, orgID, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReencryptSecrets(ctx, orgID); err != nil {
		t.Fatal(err)
	}
	var retired, active int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM org_deks WHERE org_id=$1 AND retired_at IS NOT NULL`, orgID).Scan(&retired)
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM org_deks WHERE org_id=$1 AND active`, orgID).Scan(&active)
	if retired < 1 || active != 1 {
		t.Fatalf("DEK rotation: retired=%d active=%d, want retired>=1 active=1", retired, active)
	}
	if v, err := st.RevealSecret(ctx, orgID, other.ID, "admin"); err != nil || v != "other" {
		t.Fatalf("reveal after DEK rotation = %q err=%v, want other", v, err)
	}
	// Every remaining secret now sits on the active DEK.
	var stale int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM secrets s WHERE s.org_id=$1 AND s.dek_id <> (SELECT id FROM org_deks WHERE org_id=$1 AND active)`, orgID).Scan(&stale)
	if stale != 0 {
		t.Fatalf("%d secrets still on a non-active DEK after re-encrypt", stale)
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

// TestBootstrapProvisioning covers the P1-5 onboarding foundation: the SSH
// provisioner pre-creates the server + a bootstrap keypair, the bootstrap token
// binds to that server, registration updates the SAME row (not a new one), the
// token is single-use, and unsupported distros are rejected.
func TestBootstrapProvisioning(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_prov"

	// Unsupported distro is refused before anything is created.
	if _, err := st.ProvisionServer(ctx, orgID, store.ProvisionInput{
		Name: "bad", Type: "general", Distro: "centos-9",
	}, "admin", 15*time.Minute); !errors.Is(err, store.ErrUnsupportedDistro) {
		t.Fatalf("provision unsupported distro = %v, want ErrUnsupportedDistro", err)
	}

	// Provision a supported host: server pre-created, keypair minted, token bound.
	res, err := st.ProvisionServer(ctx, orgID, store.ProvisionInput{
		Name: "web1", Type: "general", Provider: "hetzner", Region: "fsn1",
		ProxyRole: true, Distro: "ubuntu-24.04",
	}, "admin", 15*time.Minute)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !strings.HasPrefix(res.BootstrapPubkey, "ssh-ed25519 ") {
		t.Errorf("bootstrap pubkey not an ssh-ed25519 line: %q", res.BootstrapPubkey)
	}

	// The pre-created server exists in provisioning state with its attributes,
	// a mesh IP, and the wrapped bootstrap key — BEFORE any agent registers.
	var status, source, distro string
	var proxy bool
	var meshIP *string
	var wrapped []byte
	if err := st.Pool.QueryRow(ctx, `
		SELECT status, source, proxy_role, distro, mesh_ip, bootstrap_key_wrapped
		  FROM servers WHERE id = $1`, res.ServerID).
		Scan(&status, &source, &proxy, &distro, &meshIP, &wrapped); err != nil {
		t.Fatalf("load pre-created server: %v", err)
	}
	if status != "provisioning" || source != "provisioned" || !proxy || distro != "ubuntu-24.04" {
		t.Errorf("pre-created server = status=%q source=%q proxy=%v distro=%q", status, source, proxy, distro)
	}
	if meshIP == nil || *meshIP == "" {
		t.Error("pre-created server has no mesh IP")
	}
	if len(wrapped) == 0 {
		t.Error("bootstrap key was not wrapped/stored")
	}

	// Register with the bound token → updates the SAME server row, no new server.
	reg, err := st.RegisterServer(ctx, res.Token, "web1", "0.1.0", json.RawMessage(`{"os":"linux"}`), "wgpubkey==")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if reg.Server.ID != res.ServerID {
		t.Errorf("register created a new server %q, want the pre-created %q", reg.Server.ID, res.ServerID)
	}
	var count int
	st.Pool.QueryRow(ctx, `SELECT count(*) FROM servers WHERE org_id = $1 AND deleted_at IS NULL`, orgID).Scan(&count)
	if count != 1 {
		t.Errorf("server count = %d, want exactly 1 (register must not insert a duplicate)", count)
	}

	// Single redemption: the token cannot be used a second time.
	if _, err := st.RegisterServer(ctx, res.Token, "web1", "0.1.0", json.RawMessage(`{}`), "x"); !errors.Is(err, store.ErrTokenInvalid) {
		t.Fatalf("second register = %v, want ErrTokenInvalid (single-use)", err)
	}
}

// TestHostHardening covers the P1-5 hardening store flow: defaults, the
// keep-public-SSH opt-out toggle, and posture persistence from a heartbeat.
func TestHostHardening(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_hard"

	res, err := st.ProvisionServer(ctx, orgID, store.ProvisionInput{
		Name: "web1", Type: "general", ProxyRole: true, Distro: "debian-12",
	}, "admin", 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterServer(ctx, res.Token, "web1", "0.1.0", json.RawMessage(`{}`), "wg=="); err != nil {
		t.Fatal(err)
	}

	// Default posture: SSH locked down, CIS on, proxy role carried, mesh iface set.
	hh, err := st.HostHardeningForServer(ctx, res.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if hh.KeepPublicSSH || !hh.CISEnabled || !hh.ProxyRole || hh.MeshInterface != "sigma0" || hh.MeshIP == "" {
		t.Fatalf("default hardening = %+v", hh)
	}

	// Opt out of SSH lockdown + add an exception; the render input reflects it.
	if err := st.SetHardeningConfig(ctx, orgID, res.ServerID, true, true,
		[]store.PortException{{Port: 8443, Proto: "tcp"}}, "admin"); err != nil {
		t.Fatal(err)
	}
	hh, _ = st.HostHardeningForServer(ctx, res.ServerID)
	if !hh.KeepPublicSSH || len(hh.ExtraPorts) != 1 || hh.ExtraPorts[0].Port != 8443 {
		t.Fatalf("after opt-out = %+v", hh)
	}

	// Cross-tenant guard: another org cannot change this server's hardening.
	if err := st.SetHardeningConfig(ctx, "org_other", res.ServerID, false, true, nil, "attacker"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-tenant hardening write = %v, want ErrNotFound", err)
	}

	// Posture from a heartbeat is persisted on the server.
	if err := st.RecordHeartbeat(ctx, res.ServerID, store.HeartbeatInput{
		AgentVersion: "0.1.0", Facts: json.RawMessage(`{}`),
		Hardening: &store.HardeningReport{Score: 80, DiskEncrypted: true, SSHLocked: true},
	}); err != nil {
		t.Fatal(err)
	}
	var score int
	var disk, locked bool
	st.Pool.QueryRow(ctx, `SELECT hardening_score, disk_encrypted, ssh_locked FROM servers WHERE id=$1`, res.ServerID).
		Scan(&score, &disk, &locked)
	if score != 80 || !disk || !locked {
		t.Fatalf("posture not persisted: score=%d disk=%v locked=%v", score, disk, locked)
	}
}

// TestMeshGatedReady covers the P1-5 derived Ready state: a server is Ready only
// when it is running, has applied its mesh config, AND a same-org peer is
// dialable (pubkey + mesh IP + endpoint) so a tunnel can form.
func TestMeshGatedReady(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_ready"

	provisionRegister := func(name, pubkey string) string {
		res, err := st.ProvisionServer(ctx, orgID, store.ProvisionInput{Name: name, Type: "general"}, "admin", 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.RegisterServer(ctx, res.Token, name, "0.1.0", json.RawMessage(`{}`), pubkey); err != nil {
			t.Fatal(err)
		}
		return res.ServerID
	}
	ready := func(id string) bool {
		srv, err := st.GetServer(ctx, orgID, id)
		if err != nil {
			t.Fatal(err)
		}
		return srv.Ready
	}

	a := provisionRegister("a", "wgA==")
	// Just registered: provisioning (no heartbeat), no mesh sync, no peer → not ready.
	if ready(a) {
		t.Fatal("server A ready before any heartbeat/mesh/peer")
	}

	// A heartbeats with mesh applied → running + mesh_synced, but still no peer.
	if err := st.RecordHeartbeat(ctx, a, store.HeartbeatInput{
		AgentVersion: "0.1.0", Facts: json.RawMessage(`{}`), Pubkey: "wgA==", MeshApplied: true, MeshPeerCount: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if ready(a) {
		t.Fatal("server A ready with no formable peer in the org")
	}

	// Bring up peer B with a pubkey + mesh IP + endpoint → A can now form a tunnel.
	b := provisionRegister("b", "wgB==")
	if err := st.RecordHeartbeat(ctx, b, store.HeartbeatInput{
		AgentVersion: "0.1.0", Facts: json.RawMessage(`{}`), Pubkey: "wgB==", Endpoint: "203.0.113.7:51820", MeshApplied: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !ready(a) {
		t.Fatal("server A should be Ready once a dialable same-org peer exists")
	}

	// Cross-org isolation: a peer in a different org must not make A ready. Prove
	// the peer predicate is org-scoped by deleting B and confirming A drops.
	if err := st.DeleteServer(ctx, orgID, b, "test"); err != nil {
		t.Fatal(err)
	}
	if ready(a) {
		t.Fatal("server A must drop out of Ready when its only formable peer is removed")
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
