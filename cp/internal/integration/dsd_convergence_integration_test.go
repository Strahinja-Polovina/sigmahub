package integration

// SIGMA-247: a server whose apply keeps failing exhausts its re-drive budget
// and then goes quiet. Nothing told the operator. This exercises the whole
// path: agent reports a failing host:* op, the CP re-drives it up to the cap,
// and at the cap it must both fire a dsd_apply_failed alert and report the
// divergence on the server read model the dashboard consumes.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/reconciler"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestDSDApplyFailureAlertsAndSurfacesNonConvergence(t *testing.T) {
	st, dsdKey := testStore(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := reconciler.New(log, st, dsdKey)

	orgID := "org_conv"
	// A channel subscribed to every event, so the outbox has somewhere to fan out to.
	if _, err := st.CreateAlertChannel(ctx, orgID, "test", store.CreateAlertChannelInput{
		Kind: "slack", Name: "ops", Secret: "https://hooks.slack.com/services/T000/secret",
	}); err != nil {
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

	// First render: the host ops (nftables, sshd) always exist for a registered server.
	if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	v, err := st.CurrentDSDVersion(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if v == 0 {
		t.Fatal("no DSD rendered for a freshly registered server")
	}

	// The host's nftables op fails, over and over. The agent reports the whole
	// document as not converged each time and the CP re-drives it — until the
	// SIGMA-116 cap, after which it stops re-issuing.
	failedOp := "host:nftables:" + serverID
	for i := 0; i < 8; i++ {
		cur, err := st.CurrentDSDVersion(ctx, serverID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ApplyDSDStatus(ctx, serverID, cur, nil, false, []string{failedOp}); err != nil {
			t.Fatalf("apply status %d: %v", i, err)
		}
		if err := rec.Reconcile(ctx, orgID, serverID); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	// 1. The operator gets told — exactly once, not once per resync. The stuck
	// version is frozen at the cap, so the dedup key has to hold even though the
	// loop above kept reconciling after the budget ran out.
	if n := outboxRows(t, st, store.AlertDSDApplyFailed); n != 1 {
		t.Fatalf("dsd_apply_failed alerts = %d, want exactly 1", n)
	}

	// 2. The dashboard read model says the server has diverged, and names the op.
	srv, err := st.GetServer(ctx, orgID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Converged {
		t.Fatal("server read model reports converged while its apply keeps failing")
	}
	if srv.ApplyOK {
		t.Fatal("server read model reports applyOk=true while its apply keeps failing")
	}
	if srv.RedriveCount < 5 {
		t.Fatalf("redriveCount = %d, want the cap (5)", srv.RedriveCount)
	}
	if len(srv.ApplyFailedOps) != 1 || srv.ApplyFailedOps[0] != failedOp {
		t.Fatalf("applyFailedOps = %v, want [%s]", srv.ApplyFailedOps, failedOp)
	}

	// And the state clears when the apply finally succeeds — the badge must not
	// be sticky, or the first real failure after a fixed one reads as old news.
	if _, err := st.ApplyDSDStatus(ctx, serverID, srv.DSDVersion, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	srv, err = st.GetServer(ctx, orgID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if !srv.Converged || !srv.ApplyOK || len(srv.ApplyFailedOps) != 0 {
		t.Fatalf("after a successful apply: converged=%v applyOk=%v failedOps=%v",
			srv.Converged, srv.ApplyOK, srv.ApplyFailedOps)
	}
}
