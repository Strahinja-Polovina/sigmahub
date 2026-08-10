package integration

// Graceful decommission, end to end (SIGMA-204, SIGMA-205).
//
// Disconnecting a server is not a function call, it is a conversation: the
// control plane marks the row, renders one op, and then WAITS — for an agent
// that may or may not still be there — before it writes the tombstone. Every
// property worth having is a property of that sequence, and none of them is
// visible to a unit test of either end:
//
//   - the machine is actually told. A dashboard that tombstones the row and
//     tells the operator "the agent tears down its WireGuard tunnel" while the
//     binary, the unit, the tunnel and every container stay exactly where they
//     are is the defect this replaces;
//   - the agent can still SPEAK when it reports. The ack is authenticated by a
//     token this very flow revokes, so the order of the two is the feature;
//   - it terminates either way. On the ack, or on the timeout, or on an
//     operator's force disconnect — never "eventually, maybe";
//   - it refuses before it starts, and says what is in the way by name;
//   - a decommissioning server is not billed and not scheduled onto.

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/api"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// decommissionAPI wires the full stack a decommission needs: the reconciler
// (which renders the uninstall op), the DSD endpoints (which deliver it) and
// the agent ack route. catalogAPI is not enough — without the reconciler the
// op is never rendered and the test would pass on a control plane that never
// tells the machine anything.
func decommissionAPI(t *testing.T, st *store.Store, key ed25519.PrivateKey) (*httptest.Server, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := reconciler.New(log, st, key)
	api.SetLongPollTimeout(500 * time.Millisecond)
	srv := api.New(log, st, st, st, api.Options{
		DevServiceToken: "dev",
		DSDStore:        st,
		DSDWaiter:       rec,
		Reconcile:       rec,
		DSDPublicKey:    key.Public().(ed25519.PublicKey),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, "dev"
}

// ackUninstall is the agent's dedicated report — the one it sends from INSIDE
// the uninstall handler, before it tears down the tunnel and deletes the data
// dir holding this token.
func ackUninstall(t *testing.T, ts *httptest.Server, agentToken string, ok bool, detail string) (int, map[string]any) {
	t.Helper()
	return postAs(t, ts, agentToken, "/v1/agent/uninstall-ack", map[string]any{"ok": ok, "detail": detail})
}

// The whole conversation, in order.
func TestGracefulDecommissionLifecycle(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_decom"

	serverID, agentToken := connectAgent(t, ts, token, orgID, "retiring", "general", noGPUHostFacts)
	heartbeat(t, ts, agentToken, noGPUHostFacts)
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusRunning {
		t.Fatalf("status = %q, want running before we start", srv.Status)
	}
	// Baseline: it IS billed while it is a member of the fleet.
	if _, servers, _, err := st.ConnectedServerUnits(ctx, orgID); err != nil || servers != 1 {
		t.Fatalf("connected servers = %d (err %v), want 1", servers, err)
	}

	// Phase 1. The operator disconnects; nothing is destroyed yet.
	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission",
		map[string]any{"purgeVolumes": false})
	if code != http.StatusOK {
		t.Fatalf("decommission → %d: %v", code, body)
	}
	if got, _ := body["status"].(string); got != store.ServerStatusDecommissioning {
		t.Fatalf("response status = %q, want %q", got, store.ServerStatusDecommissioning)
	}
	srv := getServer(t, st, orgID, serverID)
	if srv.Status != store.ServerStatusDecommissioning {
		t.Fatalf("stored status = %q, want %q", srv.Status, store.ServerStatusDecommissioning)
	}
	if srv.DecommissioningSince == nil {
		t.Fatal("no decommission timestamp stored — the timeout has no clock to run against")
	}

	// The row is NOT tombstoned yet: the machine has not been told, let alone
	// finished. Tombstoning here is the old behaviour.
	if _, err := st.GetServer(ctx, orgID, serverID); err != nil {
		t.Fatalf("server disappeared before the agent did anything: %v", err)
	}
	// And the agent can still authenticate — the ack it is about to send
	// depends on it.
	heartbeat(t, ts, agentToken, noGPUHostFacts)

	// A decommissioning server stops being billed the moment the operator asks
	// for it back. Both meters key on 'running', which is why the new status is
	// its own value and not a flavour of it.
	if _, servers, units, err := st.ConnectedServerUnits(ctx, orgID); err != nil {
		t.Fatal(err)
	} else if servers != 0 || units != 0 {
		t.Fatalf("a decommissioning server is still billed: %d server(s), %d unit(s)", servers, units)
	}
	if n, err := st.SweepServerHours(ctx, time.Now()); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("SweepServerHours wrote %d billable hour row(s) for a server being decommissioned", n)
	}

	// Phase 1b: the machine is told. The document is the uninstall op alone.
	signed, code := agentGetDSD(t, ts.URL, agentToken, 0)
	if code != http.StatusOK {
		t.Fatalf("agent DSD poll → %d; the host was never told to uninstall", code)
	}
	if err := dsd.Verify(key.Public().(ed25519.PublicKey), signed); err != nil {
		t.Fatalf("uninstall DSD does not verify: %v", err)
	}
	if len(signed.Document.Ops) != 1 || signed.Document.Ops[0].Kind != dsd.KindAgentUninstall {
		t.Fatalf("decommission document = %+v, want exactly one agent.uninstall op", signed.Document.Ops)
	}
	var spec struct {
		ServerID      string `json:"serverId"`
		PurgeVolumes  bool   `json:"purgeVolumes"`
		MeshInterface string `json:"meshInterface"`
	}
	if err := json.Unmarshal(signed.Document.Ops[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.ServerID != serverID || spec.MeshInterface == "" {
		t.Fatalf("uninstall spec = %+v", spec)
	}
	if spec.PurgeVolumes {
		t.Fatal("purgeVolumes reached the agent without the operator opting in")
	}

	// Phase 2: the agent acks — from inside its handler, while this token still
	// works, which is the whole reason the ack is not the op-status report.
	if code, body := ackUninstall(t, ts, agentToken, true, ""); code != http.StatusOK {
		t.Fatalf("uninstall ack → %d: %v", code, body)
	}

	// Now, and only now, the tombstone.
	if _, err := st.GetServer(ctx, orgID, serverID); err == nil {
		t.Fatal("server is still readable after the agent confirmed it removed itself")
	}
	// The token is revoked as part of completing: the agent's next call 401s,
	// which is what makes a leftover process exit.
	code, _ = postAs(t, ts, agentToken, "/v1/agent/heartbeat", map[string]any{"agentVersion": "0.9.0"})
	if code != http.StatusUnauthorized {
		t.Fatalf("heartbeat after decommission → %d, want 401 (token revoked)", code)
	}

	// The audit log tells the story in order: asked for, then completed.
	entries, err := st.ListAudit(ctx, orgID, 100)
	if err != nil {
		t.Fatal(err)
	}
	var sawStart, sawDone bool
	for _, e := range entries {
		switch e.Action {
		case "Server decommissioning":
			sawStart = true
		case "Server decommissioned":
			sawDone = true
			// Attributed to the operator who pressed the button, not to the
			// agent that happened to deliver the news.
			if e.Actor == "sigmad" {
				t.Errorf("completion attributed to %q, want the requesting operator", e.Actor)
			}
		}
	}
	if !sawStart || !sawDone {
		t.Fatalf("audit trail incomplete (start=%v done=%v): %+v", sawStart, sawDone, entries)
	}
}

// The volumes opt-in survives the whole path: dialog → API → row → op spec.
// Each hop is one assignment, and a break anywhere silently reverts to the safe
// default — which is the failure nobody notices until a customer's database
// volume is still on a machine they gave back, or worse, is not.
func TestPurgeVolumesOptInReachesTheAgent(t *testing.T) {
	st, key := testStore(t)
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_purge"

	serverID, agentToken := connectAgent(t, ts, token, orgID, "with-data", "general", noGPUHostFacts)
	if code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission",
		map[string]any{"purgeVolumes": true}); code != http.StatusOK {
		t.Fatalf("decommission → %d: %v", code, body)
	}
	if srv := getServer(t, st, orgID, serverID); !srv.PurgeVolumes {
		t.Fatal("the row does not record the opt-in, so a page reload would show the wrong promise")
	}
	signed, code := agentGetDSD(t, ts.URL, agentToken, 0)
	if code != http.StatusOK {
		t.Fatalf("DSD poll → %d", code)
	}
	var spec struct {
		PurgeVolumes bool `json:"purgeVolumes"`
	}
	if err := json.Unmarshal(signed.Document.Ops[0].Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if !spec.PurgeVolumes {
		t.Fatal("the operator opted into deleting application data and the agent was never told")
	}
}

// The first line of defence, and the thing SIGMA-205 asks it to say: WHICH
// resources. A 409 whose only content is a sentence forces the dialog to either
// parse prose or print a raw error at a customer.
func TestDecommissionRefusesWithBoundResourceNames(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_bound"

	serverID, _ := connectAgent(t, ts, token, orgID, "busy", "general", noGPUHostFacts)
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
	for _, name := range []string{"web", "api"} {
		if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
			EnvironmentID: env.ID, ServerID: serverID, Name: name, Kind: "app",
		}, "test"); err != nil {
			t.Fatal(err)
		}
	}

	code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission", map[string]any{})
	if code != http.StatusConflict {
		t.Fatalf("decommission with bound resources → %d: %v", code, body)
	}
	names, _ := body["boundResources"].([]any)
	if len(names) != 2 {
		t.Fatalf("boundResources = %v, want the two blocking resources as data the dialog can list", body)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n.(string)] = true
	}
	if !got["web"] || !got["api"] {
		t.Fatalf("boundResources = %v, want web and api", names)
	}
	// The refusal is complete: nothing was started, so the server is still an
	// ordinary running host rather than a half-decommissioned one.
	if srv := getServer(t, st, orgID, serverID); srv.Status == store.ServerStatusDecommissioning {
		t.Fatal("a refused decommission still moved the server out of service")
	}

	// The force path answers the same way, from the same store error — the
	// dialog offers both buttons and must not get two different shapes back.
	code, body = sendAs(t, ts, "DELETE", token, "/v1/orgs/"+orgID+"/servers/"+serverID, nil)
	if code != http.StatusConflict {
		t.Fatalf("force disconnect with bound resources → %d: %v", code, body)
	}
	if names, _ := body["boundResources"].([]any); len(names) != 2 {
		t.Fatalf("force path 409 carries no resource names: %v", body)
	}

	// Remove the blockers and the disconnect goes through.
	bound, err := st.ListResources(ctx, orgID, env.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range bound {
		if _, err := st.DeleteResource(ctx, orgID, r.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission",
		map[string]any{}); code != http.StatusOK {
		t.Fatalf("decommission after clearing the blockers → %d: %v", code, body)
	}
}

// Nothing may be scheduled onto a machine on its way out — otherwise a resource
// created mid-teardown re-arms the bound-resources guard and blocks the
// completion of a decommission already in flight.
func TestDecommissioningServerRefusesNewResources(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_noschedule"

	serverID, _ := connectAgent(t, ts, token, orgID, "leaving", "general", noGPUHostFacts)
	proj, _ := st.CreateProject(ctx, orgID, "p", "", "test")
	env, _ := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "test"); err != nil {
		t.Fatal(err)
	}
	if code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission",
		map[string]any{}); code != http.StatusOK {
		t.Fatalf("decommission → %d: %v", code, body)
	}
	_, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "late", Kind: "app",
	}, "test")
	if err == nil {
		t.Fatal("a resource was scheduled onto a server being decommissioned")
	}
	// And its type cannot be re-filed either: the type decides what renders and
	// what it bills as, and both are settled.
	if code, _ := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/type",
		map[string]any{"type": "storage"}); code != http.StatusConflict {
		t.Fatalf("re-filing a decommissioning server → %d, want 409", code)
	}
}

// The guard that keeps the teardown alive while it runs.
//
// One of the two documented exits from `incompatible` IS disconnecting, so the
// host most likely to be decommissioned is one whose facts fail its type — and
// the agent keeps heartbeating throughout the teardown. Without the terminal
// rule in compatibilityStatus, the gate writes `incompatible` back over
// `decommissioning`, the row drops out of the sweeper's timeout scan, and the
// operator's disconnect never completes at all.
func TestDecommissioningSurvivesAHeartbeatThatFailsTheGate(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_decom_gate"

	// A GPU host with no GPU: refused at enrollment, exactly the state an
	// operator reaches the disconnect button from.
	serverID, agentToken := connectAgent(t, ts, token, orgID, "misfiled", "gpu", noGPUHostFacts)
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusIncompatible {
		t.Fatalf("status = %q, want incompatible", srv.Status)
	}
	if code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission",
		map[string]any{}); code != http.StatusOK {
		t.Fatalf("decommission → %d: %v", code, body)
	}

	// The agent keeps checking in while it tears the host down, reporting the
	// same facts that failed the gate.
	heartbeat(t, ts, agentToken, noGPUHostFacts)
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusDecommissioning {
		t.Fatalf("status after a heartbeat mid-teardown = %q, want it to stay %q",
			srv.Status, store.ServerStatusDecommissioning)
	}
	// Still in flight, so still the sweeper's problem.
	timedOut, err := st.TimeoutStaleDecommissions(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(timedOut) != 1 || timedOut[0].ServerID != serverID {
		t.Fatalf("the sweeper cannot see the in-flight decommission: %+v", timedOut)
	}
}

// The other termination path: the machine never answers. A host powered off
// between the button and the op pickup would otherwise hold its row forever —
// 'decommissioning' is not 'running', so the staleness sweep never touches it.
func TestDecommissionTimeoutCompletesWithoutTheAgent(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_timeout"

	serverID, agentToken := connectAgent(t, ts, token, orgID, "silent", "general", noGPUHostFacts)
	if code, body := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission",
		map[string]any{}); code != http.StatusOK {
		t.Fatalf("decommission → %d: %v", code, body)
	}

	// A generous timeout must NOT fire on a teardown that just started.
	if timedOut, err := st.TimeoutStaleDecommissions(ctx, time.Hour); err != nil {
		t.Fatal(err)
	} else if len(timedOut) != 0 {
		t.Fatalf("the sweeper gave up on a fresh decommission: %+v", timedOut)
	}

	// Time passes (the sweeper's cutoff is computed in SQL, so age the row the
	// same way the database will read it).
	if _, err := st.Pool.Exec(ctx,
		`UPDATE servers SET decommission_started_at = now() - interval '20 minutes' WHERE id = $1`,
		serverID); err != nil {
		t.Fatal(err)
	}
	timedOut, err := st.TimeoutStaleDecommissions(ctx, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(timedOut) != 1 || timedOut[0].ServerID != serverID {
		t.Fatalf("timeout sweep = %+v, want the stale server", timedOut)
	}
	if _, err := st.GetServer(ctx, orgID, serverID); err == nil {
		t.Fatal("a timed-out decommission left the server row live")
	}
	// Same terminal state as the ack path: token revoked, so a machine that
	// wakes up later exits instead of resuming.
	if code, _ := postAs(t, ts, agentToken, "/v1/agent/heartbeat",
		map[string]any{"agentVersion": "0.9.0"}); code != http.StatusUnauthorized {
		t.Fatalf("heartbeat after a timed-out decommission → %d, want 401", code)
	}
	// The audit says which of the two it was: an operator debugging a machine
	// that still has our binary on it needs to know we gave up waiting.
	entries, err := st.ListAudit(ctx, orgID, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Action, "Server decommission timed out") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no timeout audit entry: %+v", entries)
	}
	// Idempotent: a second sweep has nothing left to do.
	if again, err := st.TimeoutStaleDecommissions(ctx, time.Minute); err != nil {
		t.Fatal(err)
	} else if len(again) != 0 {
		t.Fatalf("the sweep re-tombstoned an already-removed server: %+v", again)
	}
	// And a late ack from a machine that finally woke up is accepted and
	// discarded rather than erroring at an agent that did nothing wrong.
	if code, body := ackUninstall(t, ts, agentToken, true, ""); code != http.StatusUnauthorized {
		t.Fatalf("late ack on a revoked token → %d: %v (want 401 — the credential is gone)", code, body)
	}
}

// The force path, from the state it exists for: a host that stopped answering.
// This is today's tombstone+revoke, unchanged — the point is that it stays
// reachable, because a graceful teardown cannot be delivered to a machine that
// is not listening.
func TestForceDisconnectOnUnreachableServer(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_force"

	serverID, agentToken := connectAgent(t, ts, token, orgID, "dead-box", "general", noGPUHostFacts)
	heartbeat(t, ts, agentToken, noGPUHostFacts) // it was alive, once
	// The sweeper gives up on it (no heartbeat inside the window).
	if _, err := st.MarkStaleUnreachable(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusUnreachable {
		t.Fatalf("status = %q, want unreachable", srv.Status)
	}
	if code, body := sendAs(t, ts, "DELETE", token, "/v1/orgs/"+orgID+"/servers/"+serverID, nil); code != http.StatusOK {
		t.Fatalf("force disconnect → %d: %v", code, body)
	}
	if _, err := st.GetServer(ctx, orgID, serverID); err == nil {
		t.Fatal("force disconnect left the server readable")
	}
}

// The staleness sweep must leave a decommissioning server alone. It flips
// 'running' rows only — asserted here rather than assumed, because the two
// sweeps run in the same tick and an unreachable alert for a machine doing
// exactly what it was told is a page nobody should get.
func TestStalenessSweepIgnoresDecommissioningServers(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_sweep"

	serverID, _ := connectAgent(t, ts, token, orgID, "quiet", "general", noGPUHostFacts)
	if code, _ := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission",
		map[string]any{}); code != http.StatusOK {
		t.Fatal("decommission failed")
	}
	if _, err := st.MarkStaleUnreachable(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if srv := getServer(t, st, orgID, serverID); srv.Status != store.ServerStatusDecommissioning {
		t.Fatalf("status after the staleness sweep = %q, want %q", srv.Status, store.ServerStatusDecommissioning)
	}
}

// An ack for a server nobody asked to decommission is refused. This is the one
// message on the agent channel that destroys a server, so it may only ever
// finish work the control plane started.
func TestUnsolicitedAckIsRefused(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_unsolicited"

	serverID, agentToken := connectAgent(t, ts, token, orgID, "healthy", "general", noGPUHostFacts)
	if code, body := ackUninstall(t, ts, agentToken, true, ""); code != http.StatusConflict {
		t.Fatalf("unsolicited ack → %d: %v, want 409", code, body)
	}
	if _, err := st.GetServer(ctx, orgID, serverID); err != nil {
		t.Fatalf("an unsolicited ack removed a live server: %v", err)
	}
}

// A teardown the agent could not finish still completes the decommission, and
// still says so. Refusing to finish would leave the control plane holding a row
// for a machine that has already deleted its own credential — a hang with a
// tidy-looking cause.
func TestFailedTeardownStillCompletesAndIsAudited(t *testing.T) {
	st, key := testStore(t)
	ctx := context.Background()
	ts, token := decommissionAPI(t, st, key)
	orgID := "org_partial"

	serverID, agentToken := connectAgent(t, ts, token, orgID, "half-torn", "general", noGPUHostFacts)
	if code, _ := postAs(t, ts, token, "/v1/orgs/"+orgID+"/servers/"+serverID+"/decommission",
		map[string]any{}); code != http.StatusOK {
		t.Fatal("decommission failed")
	}
	if code, body := ackUninstall(t, ts, agentToken, false,
		"containers: docker daemon not reachable"); code != http.StatusOK {
		t.Fatalf("failed-teardown ack → %d: %v", code, body)
	}
	if _, err := st.GetServer(ctx, orgID, serverID); err == nil {
		t.Fatal("a failed teardown left the row open, so the operator waits on a machine that is gone")
	}
	entries, err := st.ListAudit(ctx, orgID, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Action, "Server decommissioned with errors") &&
			strings.Contains(e.Action, "docker daemon not reachable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failure detail never reached the audit log: %+v", entries)
	}
}

// A cluster node cannot be disconnected out from under its cluster.
//
// The bug this pins: the graceful path refused only on resources.server_id, and
// a cluster member's workloads are bound to the CLUSTER — so decommissioning a
// live cluster's control-plane node returned 200, and the agent then tore down
// its DOCKER objects only. k3s, /var/lib/rancher/k3s and every workload the
// scheduler had placed there kept running on a machine the dashboard had just
// removed, while the cluster went on reporting it as a node. That is exactly
// the "we said the host was clean and it was not" failure this lifecycle
// exists to end.
func TestDecommissionRefusesAClusterNode(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_decom_cluster"
	envID, cpServer, worker := clusterFixture(t, st, orgID)

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "production", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddClusterNode(ctx, orgID, cluster.ID, worker, "admin"); err != nil {
		t.Fatal(err)
	}

	for _, srv := range []struct{ name, id string }{
		{"control plane", cpServer},
		{"worker", worker},
	} {
		t.Run(srv.name, func(t *testing.T) {
			_, err := st.BeginDecommission(ctx, orgID, srv.id, false, "admin")
			if err == nil {
				t.Fatal("a cluster node was accepted for decommission — its k3s and workloads would survive the teardown")
			}
			if !errors.Is(err, store.ErrConflict) {
				t.Fatalf("refusal = %v, want ErrConflict so the API answers 409", err)
			}
			// The operator has to be told WHICH cluster, or the refusal is a
			// dead end: they cannot act on "this is a node of a cluster".
			if !strings.Contains(err.Error(), "production") {
				t.Errorf("refusal does not name the cluster: %v", err)
			}
		})
	}

	// Removing it from the cluster is the way out, and it works.
	if err := st.RemoveClusterNode(ctx, orgID, cluster.ID, worker, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginDecommission(ctx, orgID, worker, false, "admin"); err != nil {
		t.Fatalf("a server removed from its cluster must be disconnectable: %v", err)
	}
}

// Force disconnect is the escape hatch for a host that cannot be asked nicely,
// so it must not leave the cluster holding a row that points at a tombstoned
// server — ListClusters counted it as a node and ControlPlaneServerForCluster
// handed its id to the renderer.
func TestForceDisconnectClearsClusterMembership(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_force_cluster"
	envID, cpServer, worker := clusterFixture(t, st, orgID)

	cluster, err := st.CreateCluster(ctx, orgID, store.CreateClusterInput{
		EnvironmentID: envID, Name: "production", ControlPlaneID: cpServer,
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddClusterNode(ctx, orgID, cluster.ID, worker, "admin"); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteServer(ctx, orgID, worker, "admin"); err != nil {
		t.Fatalf("force disconnect: %v", err)
	}
	var rows int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM cluster_nodes WHERE server_id = $1`, worker).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("cluster_nodes still points at a tombstoned server (%d rows)", rows)
	}
}

// A host the agent never reached is removed at once, not waited on.
//
// This is what a stuck row looked like in production: a server connected
// through the wizard, an install command that was never run, and a Disconnect
// that put the row into `decommissioning` to wait for an ack from a machine
// that had never heard of us. The dialog withholds Force disconnect for the
// whole ten-minute window — correctly, while a teardown is genuinely under way
// — so the operator had a row stuck mid-teardown, no affordance and no
// explanation, for a machine the product had never touched. The sweeper cleared
// it after ten minutes, which made a defect look like slowness.
func TestAServerWhoseAgentNeverRegisteredIsRemovedImmediately(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_never_enrolled"

	// Provisioned and never heard from again — the row the connect wizard
	// leaves behind when nobody runs the install command it printed.
	// agent_version is written by the agent's registration and by nothing else,
	// so an empty one is the whole signal.
	prov, err := st.ProvisionServer(ctx, orgID, store.ProvisionInput{
		Name: "203.0.113.9", Type: "general", Provider: "BYO", HostIP: "203.0.113.9",
	}, "operator", time.Hour)
	if err != nil {
		t.Fatalf("ProvisionServer: %v", err)
	}

	state, err := st.BeginDecommission(ctx, orgID, prov.ServerID, false, "operator")
	if err != nil {
		t.Fatalf("BeginDecommission: %v", err)
	}
	if !state.Removed {
		t.Fatalf("state.Removed = false; the dashboard then sends the operator to watch a "+
			"teardown that cannot happen (state = %+v)", state)
	}
	if !state.StartedAt.IsZero() {
		t.Errorf("StartedAt = %v, want zero: nothing was started", state.StartedAt)
	}

	// Gone, not waiting.
	if _, err := st.GetServer(ctx, orgID, prov.ServerID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the server still loads after disconnect: err = %v", err)
	}

	// And the credential is dead, which is what makes "removed" true rather
	// than merely hidden: a host that later runs the install command it was
	// given must not be able to register against this row.
	var live int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_tokens WHERE server_id = $1 AND revoked_at IS NULL`,
		prov.ServerID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("%d agent token(s) still live for a removed server", live)
	}
}

// SIGMA-229: the bound-resources guard has to ask the SAME question the
// renderer asks — "what does this server run?" — and the renderer's answer
// (ResourceHostedHere) counts three things, of which the guard historically
// knew only one.
//
// The hole that matters in practice is per-service Compose placement. A
// Compose app owned by one server can have individual services dragged onto
// another host; the spec, not the resources row, records where they went. So
// the receiving host's `resources` rows are empty, the disconnect dialog
// reports zero blockers, and the graceful teardown proceeds: the agent removes
// the containers and the row is tombstoned, while the app's spec still names a
// server that no longer exists. No other document renders those services, and
// nothing anywhere reports an error.
func TestDecommissionRefusesAHostOfPlacedComposeServices(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_placed_compose"

	appServer := connectServer(t, st, orgID, "srv_app")
	dataServer := connectServer(t, st, orgID, "srv_data")
	proj, err := st.CreateProject(ctx, orgID, "p", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{appServer, dataServer} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: appServer, Name: "shop", Kind: "app",
		Spec: json.RawMessage(`{"compose":{"services":[{"name":"web"},{"name":"worker"},{"name":"redis"}]}}`),
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetComposePlacements(ctx, orgID, app.ID, []store.ComposePlacement{
		{Service: "worker", ServerID: dataServer},
		{Service: "redis", ServerID: dataServer},
	}, "admin"); err != nil {
		t.Fatal(err)
	}

	// srv_data has no `resources` row of its own, but it runs two containers.
	_, err = st.BeginDecommission(ctx, orgID, dataServer, false, "operator")
	var bound store.ErrBoundResources
	if !errors.As(err, &bound) {
		t.Fatalf("BeginDecommission on a host of placed Compose services returned %v; "+
			"want a 409 naming the app whose services run here", err)
	}
	if len(bound.Names) != 1 || bound.Names[0] != "shop" {
		t.Fatalf("boundResources = %v, want [shop]", bound.Names)
	}
	// The force path answers from the same guard.
	if err := st.DeleteServer(ctx, orgID, dataServer, "operator"); !errors.As(err, &bound) {
		t.Fatalf("DeleteServer on a host of placed Compose services returned %v; want the same 409", err)
	}

	// And once the services move back home, the disconnect goes through.
	if _, err := st.SetComposePlacements(ctx, orgID, app.ID, []store.ComposePlacement{
		{Service: "worker", ServerID: appServer},
		{Service: "redis", ServerID: appServer},
	}, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginDecommission(ctx, orgID, dataServer, false, "operator"); err != nil {
		t.Fatalf("BeginDecommission after re-homing the services: %v", err)
	}
}

// SIGMA-229, the other half: a dedicated build server holds no `resources` row
// either — the clone+build ops live in ITS document because a deployment (or a
// branch map) names it as build_server_id. Disconnecting it mid-build tears
// down the builder under a pipeline that then hangs with nothing to report it.
func TestDecommissionRefusesALiveBuildServer(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_live_builder"

	runServer := connectServer(t, st, orgID, "run")
	buildServer := connectServer(t, st, orgID, "build")
	proj, err := st.CreateProject(ctx, orgID, "p", "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{runServer, buildServer} {
		if err := st.AttachServer(ctx, orgID, env.ID, id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: proj.ID, Provider: "github", RepoFullName: "acme/app",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: runServer, Name: "api", Kind: "app",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(ctx, orgID, store.CreateDeploymentInput{
		ResourceID: app.ID, EnvironmentID: env.ID, ServerID: runServer,
		ConnectionID: conn.ID, Trigger: "manual", GitRef: "main", GitSHA: "abc1234567",
	}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE deployments SET build_server_id = $2 WHERE id = $1`, dep.ID, buildServer); err != nil {
		t.Fatal(err)
	}

	if _, err := st.BeginDecommission(ctx, orgID, buildServer, false, "operator"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("BeginDecommission on a live build server returned %v; want a 409", err)
	}
	if err := st.DeleteServer(ctx, orgID, buildServer, "operator"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteServer on a live build server returned %v; want a 409", err)
	}
}

// SIGMA-233: a decommission that times out has to REACH somebody.
//
// The timeout path tombstones the row, revokes the agent token and writes one
// audit entry, and that was the whole of it — the only runtime signal was a
// log.Warn on the control plane's stdout. Nothing entered the alert outbox, so
// nothing reached the operator's channels.
//
// That is the one ending of the three that means the machine is still out
// there. On the ack the host tore itself down; on a force disconnect the
// operator chose it with the cleanup script in front of them. On the timeout
// nobody knows anything: sigmad is still installed, Docker restarts every
// managed container (unless-stopped) the moment the box comes back, and the
// only record the product keeps is a row nothing lists and a line in a log
// nobody ships.
func TestTimeoutStaleDecommissions_EnqueuesAlert(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_decom_alert"

	if _, err := st.CreateAlertChannel(ctx, orgID, "test", store.CreateAlertChannelInput{
		Kind: "slack", Name: "ops", Secret: "https://hooks.slack.com/services/T000/secret",
	}); err != nil {
		t.Fatal(err)
	}
	serverID := connectServer(t, st, orgID, "powered-off")
	if _, err := st.BeginDecommission(ctx, orgID, serverID, false, "operator"); err != nil {
		t.Fatal(err)
	}

	timedOut, err := st.TimeoutStaleDecommissions(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(timedOut) != 1 {
		t.Fatalf("timed out = %+v, want the one stale decommission", timedOut)
	}

	var event, title, body string
	if err := st.Pool.QueryRow(ctx,
		`SELECT event, title, body FROM alert_outbox`).Scan(&event, &title, &body); err != nil {
		t.Fatalf("no alert reached the outbox for a host that still has our software on it: %v", err)
	}
	if event != store.AlertDecommissionTimedOut {
		t.Fatalf("event = %q, want %q", event, store.AlertDecommissionTimedOut)
	}
	// The operator has to be able to act on it: which machine, who started the
	// teardown, and what is now the only way to finish it.
	if !strings.Contains(title+body, "powered-off") {
		t.Errorf("the alert does not name the server: %q / %q", title, body)
	}
	if !strings.Contains(body, "operator") {
		t.Errorf("the alert does not say who started the teardown: %q", body)
	}
	if !strings.Contains(body, "uninstall.sh") {
		t.Errorf("the alert does not point at the manual cleanup that is now the only way to finish: %q", body)
	}
}

// The alert is a fan-out over subscribed channels, so an org with no channels
// must still time out cleanly — the sweeper runs across every org at once, and
// one silent tenant must not stop the others' rows from being settled.
func TestTimeoutStaleDecommissionsWithNoAlertChannels(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_decom_nochannel"

	serverID := connectServer(t, st, orgID, "quiet")
	if _, err := st.BeginDecommission(ctx, orgID, serverID, false, "operator"); err != nil {
		t.Fatal(err)
	}
	timedOut, err := st.TimeoutStaleDecommissions(ctx, 0)
	if err != nil {
		t.Fatalf("the sweep failed for an org with no alert channels: %v", err)
	}
	if len(timedOut) != 1 || timedOut[0].ServerID != serverID {
		t.Fatalf("timed out = %+v, want the one stale decommission", timedOut)
	}
	if _, err := st.GetServer(ctx, orgID, serverID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the row survived its timeout: err = %v", err)
	}
}
