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

// TestBackupLifecycle exercises the P1-11 store path against a real Postgres:
// target CRUD (secret envelope) → policy wiring → scheduler due-run creation
// (once per day) → audited per-run credential release with BOLA scoping →
// terminal result ingest → verify-day predicate → fire-drill restore run.
func TestBackupLifecycle(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_bk"
	envID, serverID := dbTestFixture(t, st, orgID, true, "database")

	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "shop", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// No target yet → the scheduler must not enqueue anything.
	servers, err := st.CreateDueBackupRuns(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Fatalf("no-target policy must not schedule, got %v", servers)
	}

	// Target CRUD: the secret key never comes back on the metadata surface.
	target, err := st.CreateBackupTarget(ctx, orgID, "admin", store.CreateBackupTargetInput{
		Name: "minio", Endpoint: "http://minio.internal:9000", Bucket: "backups",
		AccessKey: "AKIA123", SecretKey: "supersecret", ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	targets, err := st.ListBackupTargets(ctx, orgID)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %v err = %v", targets, err)
	}
	if b, _ := json.Marshal(targets[0]); strings.Contains(string(b), "supersecret") {
		t.Fatal("target metadata leaks the secret key")
	}
	if err := st.DeleteBackupTarget(ctx, orgID, target.ID, "admin"); err != nil {
		t.Fatal("unused target must be deletable:", err)
	}
	target, err = st.CreateBackupTarget(ctx, orgID, "admin", store.CreateBackupTargetInput{
		Name: "minio", Endpoint: "http://minio.internal:9000", Bucket: "backups",
		AccessKey: "AKIA123", SecretKey: "supersecret", ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Point the policy at the target.
	tid := target.ID
	if _, err := st.UpdateBackupPolicy(ctx, orgID, res.ID, "admin", store.UpdateBackupPolicyInput{TargetID: &tid}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteBackupTarget(ctx, orgID, target.ID, "admin"); err == nil {
		t.Fatal("in-use target must not be deletable")
	}

	// First sweep enqueues a backup AND a first-day verify; a second sweep the
	// same day enqueues nothing (once per day).
	servers, err = st.CreateDueBackupRuns(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].ServerID != serverID {
		t.Fatalf("due servers = %v", servers)
	}
	again, err := st.CreateDueBackupRuns(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("same-day resweep must be empty, got %v", again)
	}
	runs, err := st.BackupRunsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Kind != "backup" || runs[1].Kind != "verify" {
		t.Fatalf("open runs = %+v", runs)
	}
	backupRun, verifyRun := runs[0], runs[1]
	if backupRun.KeepDaily != 30 {
		t.Fatalf("production retention must keep 30 dailies, got %d", backupRun.KeepDaily)
	}

	// Credential release: audited, and BOLA-scoped to the executing server.
	cred, err := st.BackupCredentialForRun(ctx, serverID, backupRun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if cred.SecretKey != "supersecret" || cred.RepoKey == "" ||
		cred.Repository != "s3:http://minio.internal:9000/backups/sigmahub/"+res.ID {
		t.Fatalf("credential = %+v", cred)
	}
	if _, err := st.BackupCredentialForRun(ctx, "srv_other", backupRun.RunID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign server credential fetch must 404, got %v", err)
	}
	cred2, err := st.BackupCredentialForRun(ctx, serverID, verifyRun.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if cred2.RepoKey != cred.RepoKey {
		t.Fatal("verify must open the same repo key")
	}

	// Terminal results: backup success records the sha the next verify pins.
	if err := st.SetBackupRunResult(ctx, serverID, backupRun.RunID, true, "snapA", "sha-a", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBackupRunResult(ctx, serverID, verifyRun.RunID, true, "", "sha-a", "checksum ok"); err != nil {
		t.Fatal(err)
	}
	// A settled run refuses further reports and drops out of the open set.
	if err := st.SetBackupRunResult(ctx, serverID, backupRun.RunID, false, "", "", "late"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("double-report must 404, got %v", err)
	}
	open, _ := st.BackupRunsForServer(ctx, serverID)
	if len(open) != 0 {
		t.Fatalf("open runs after completion = %+v", open)
	}

	// The per-day verify feed: today is green (one verify, zero failures);
	// yesterday had no runs so it is NOT green (zero-run days are never green).
	days, err := st.VerifyDays(ctx, orgID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 || days[0].Green || !days[1].Green {
		t.Fatalf("verify days = %+v", days)
	}

	// History lists both runs, newest first.
	history, err := st.ListBackupRuns(ctx, orgID, res.ID, 10)
	if err != nil || len(history) != 2 {
		t.Fatalf("history = %+v err = %v", history, err)
	}

	// Fire-drill restore: provision a fresh resource, queue the run on ITS server.
	res2, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "shop-restore", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateRestoreRun(ctx, orgID, res.ID, res2.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind != "restore" {
		t.Fatalf("run = %+v", run)
	}
	open, err = st.BackupRunsForServer(ctx, serverID)
	if err != nil || len(open) != 1 || open[0].Kind != "restore" {
		t.Fatalf("open restore = %+v err = %v", open, err)
	}
	if open[0].RestoreResourceID != res2.ID || open[0].RestoreDatabase != "shop_restore" {
		t.Fatalf("restore target fields = %+v", open[0])
	}

	// The restore run pins the last successful backup's digest, so the agent
	// refuses to load bytes that don't match what was backed up.
	if open[0].ExpectedSha != "sha-a" {
		t.Fatalf("restore run must pin the recorded sha, got %q", open[0].ExpectedSha)
	}

	// Timeout sweep fails a stuck run.
	if n, err := st.TimeoutStaleBackupRuns(ctx, -time.Minute); err != nil || n != 1 {
		t.Fatalf("timeout sweep n=%d err=%v", n, err)
	}
}
