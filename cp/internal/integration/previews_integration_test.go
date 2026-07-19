package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestPreviewEnvironmentLifecycle drives P1-12 through the webhook path
// against a real Postgres: PR opened → ephemeral env + resource + enqueued
// deploy; synchronize → new deploy into the same env; closed → teardown with
// the pre-authorised volume removal.
func TestPreviewEnvironmentLifecycle(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_prev"
	envID, serverID := dbTestFixture(t, st, orgID, false, "general")
	_ = envID

	// Connect a repo and enable previews on the fixture server.
	var projectID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id FROM environments WHERE id = $1`, envID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: projectID, RepoFullName: "acme/shop",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetConnectionPreviews(ctx, orgID, conn.ID, true, serverID, "test"); err != nil {
		t.Fatal(err)
	}
	// A template app resource whose spec previews inherit.
	appSpec, _ := json.Marshal(map[string]any{"image": "", "ports": []map[string]any{{"container": 3000}}})
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "web", Kind: "app", Spec: appSpec,
	}, "test"); err != nil {
		t.Fatal(err)
	}

	// PR #7 opened.
	out, err := st.HandleGitWebhook(ctx, store.GitWebhookEvent{
		DeliveryID: "d-pr7-open", EventType: "pull_request", RepoFullName: "acme/shop",
		Action: "opened", PRNumber: 7, Branch: "feat/x", SHA: "aaa111",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PreviewDeploy == nil || out.PreviewDeploy.Kind != "deploy" {
		t.Fatalf("outcome = %+v, want preview deploy", out)
	}
	previews, err := st.ListPreviewEnvironments(ctx, orgID, conn.ID)
	if err != nil || len(previews) != 1 {
		t.Fatalf("previews = %+v err = %v", previews, err)
	}
	pv := previews[0]
	if pv.Status != "open" || pv.PRNumber != 7 || pv.SHA != "aaa111" {
		t.Fatalf("preview = %+v", pv)
	}
	// The ephemeral environment + resource exist, spec copied from the template.
	var prodFlag bool
	var envName string
	if err := st.Pool.QueryRow(ctx,
		`SELECT name, production FROM environments WHERE id = $1`, pv.EnvironmentID).Scan(&envName, &prodFlag); err != nil {
		t.Fatal(err)
	}
	if envName != "pr-7" || prodFlag {
		t.Fatalf("env = %s production=%v", envName, prodFlag)
	}
	var ephemeral bool
	var copiedSpec []byte
	if err := st.Pool.QueryRow(ctx,
		`SELECT ephemeral, spec FROM resources WHERE id = $1`, *pv.ResourceID).Scan(&ephemeral, &copiedSpec); err != nil {
		t.Fatal(err)
	}
	if !ephemeral {
		t.Fatal("preview resource must be ephemeral")
	}
	if string(copiedSpec) == "{}" {
		t.Fatalf("preview spec must copy the template, got %s", copiedSpec)
	}

	// synchronize reuses the same environment and enqueues a fresh deploy.
	out, err = st.HandleGitWebhook(ctx, store.GitWebhookEvent{
		DeliveryID: "d-pr7-sync", EventType: "pull_request", RepoFullName: "acme/shop",
		Action: "synchronize", PRNumber: 7, Branch: "feat/x", SHA: "bbb222",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PreviewDeploy == nil || out.PreviewDeploy.SHA != "bbb222" {
		t.Fatalf("sync outcome = %+v", out)
	}
	previews, _ = st.ListPreviewEnvironments(ctx, orgID, conn.ID)
	if len(previews) != 1 || previews[0].EnvironmentID != pv.EnvironmentID || previews[0].SHA != "bbb222" {
		t.Fatalf("previews after sync = %+v", previews)
	}

	// closed tears the environment + resource down (pre-authorised volume path).
	out, err = st.HandleGitWebhook(ctx, store.GitWebhookEvent{
		DeliveryID: "d-pr7-close", EventType: "pull_request", RepoFullName: "acme/shop",
		Action: "closed", PRNumber: 7, Branch: "feat/x", SHA: "bbb222",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PreviewTeardown == nil || out.PreviewTeardown.ServerID != serverID {
		t.Fatalf("close outcome = %+v", out)
	}
	var envCount, resCount int
	if err := st.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM environments WHERE id = $1`, pv.EnvironmentID).Scan(&envCount); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM resources WHERE id = $1`, *pv.ResourceID).Scan(&resCount); err != nil {
		t.Fatal(err)
	}
	if envCount != 0 || resCount != 0 {
		t.Fatalf("teardown left env=%d res=%d rows", envCount, resCount)
	}
	previews, _ = st.ListPreviewEnvironments(ctx, orgID, conn.ID)
	if len(previews) != 1 || previews[0].Status != "closed" {
		t.Fatalf("previews after close = %+v", previews)
	}

	// A reopened PR builds a fresh preview under the same number.
	out, err = st.HandleGitWebhook(ctx, store.GitWebhookEvent{
		DeliveryID: "d-pr7-reopen", EventType: "pull_request", RepoFullName: "acme/shop",
		Action: "reopened", PRNumber: 7, Branch: "feat/x", SHA: "ccc333",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PreviewDeploy == nil {
		t.Fatalf("reopen outcome = %+v", out)
	}
}

// TestPreviewRejectsUnavailablePreviewServer covers SIGMA-127 and SIGMA-128: a PR
// preview must not be scheduled onto a preview server that is tombstoned
// (deleted_at) or of a type that can't run an app resource — the same invariants
// CreateResource enforces, which ensurePreviewTx previously bypassed.
func TestPreviewRejectsUnavailablePreviewServer(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_prev_reject"
	envID, serverID := dbTestFixture(t, st, orgID, false, "general")

	var projectID string
	if err := st.Pool.QueryRow(ctx,
		`SELECT project_id FROM environments WHERE id = $1`, envID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	conn, err := st.CreateGitConnection(ctx, orgID, store.CreateGitConnectionInput{
		ProjectID: projectID, RepoFullName: "acme/shop",
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	appSpec, _ := json.Marshal(map[string]any{"image": "", "ports": []map[string]any{{"container": 3000}}})
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "web", Kind: "app", Spec: appSpec,
	}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetConnectionPreviews(ctx, orgID, conn.ID, true, serverID, "test"); err != nil {
		t.Fatal(err)
	}

	// Case A: preview server is the wrong type for an app resource (SIGMA-128).
	if _, err := st.Pool.Exec(ctx, `UPDATE servers SET type = 'storage' WHERE id = $1`, serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.HandleGitWebhook(ctx, store.GitWebhookEvent{
		DeliveryID: "d-reject-type", EventType: "pull_request", RepoFullName: "acme/shop",
		Action: "opened", PRNumber: 1, Branch: "b1", SHA: "s1",
	}); err == nil {
		t.Fatal("preview was scheduled onto a storage-type server")
	}

	// Case B: preview server is tombstoned (SIGMA-127).
	if _, err := st.Pool.Exec(ctx,
		`UPDATE servers SET type = 'general', deleted_at = now() WHERE id = $1`, serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.HandleGitWebhook(ctx, store.GitWebhookEvent{
		DeliveryID: "d-reject-tombstone", EventType: "pull_request", RepoFullName: "acme/shop",
		Action: "opened", PRNumber: 2, Branch: "b2", SHA: "s2",
	}); err == nil {
		t.Fatal("preview was scheduled onto a tombstoned server")
	}
}
