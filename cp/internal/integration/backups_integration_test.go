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

// TestDailySweepIsIdempotentAcrossRuns is the SIGMA-72 regression: running the
// daily scheduler twice for the same day must not double-insert backup runs
// (partial unique index + ON CONFLICT DO NOTHING), and MarkDestructiveOpApplied
// is server-scoped (SIGMA-74).
func TestDailySweepIsIdempotentAcrossRuns(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_sweep"
	envID, serverID := dbTestFixture(t, st, orgID, true, "database")

	pg, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "shop", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	tgt, err := st.CreateBackupTarget(ctx, orgID, "admin", store.CreateBackupTargetInput{
		Name: "minio", Endpoint: "http://m:9000", Bucket: "b", AccessKey: "AK", SecretKey: "supersecret", ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tid := tgt.ID
	if _, err := st.UpdateBackupPolicy(ctx, orgID, pg.ID, "admin", store.UpdateBackupPolicyInput{TargetID: &tid}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if _, err := st.CreateDueBackupRuns(ctx, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDueBackupRuns(ctx, now); err != nil {
		t.Fatal(err)
	}
	// Even after two sweeps in the same day, exactly one 'backup' run exists.
	var backups int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM backup_runs WHERE resource_id=$1 AND kind='backup'`, pg.ID).Scan(&backups); err != nil {
		t.Fatal(err)
	}
	if backups != 1 {
		t.Fatalf("two sweeps produced %d backup runs, want 1 (SIGMA-72)", backups)
	}

	// SIGMA-74: MarkDestructiveOpApplied is scoped by server_id. Seed a pending
	// destructive op on this server and confirm a foreign server can't apply it.
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO pending_destructive_ops (id, org_id, server_id, op_kind, target)
		VALUES ('pdo_x', $1, $2, 'volume.remove', 'sigmahub-x-data')`, orgID, serverID); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDestructiveOpApplied(ctx, "srv_other", "pdo_x"); err != nil {
		t.Fatal(err)
	}
	var applied *time.Time
	st.Pool.QueryRow(ctx, `SELECT applied_at FROM pending_destructive_ops WHERE id='pdo_x'`).Scan(&applied)
	if applied != nil {
		t.Fatal("a foreign server must not mark this server's destructive op applied (SIGMA-74)")
	}
	if err := st.MarkDestructiveOpApplied(ctx, serverID, "pdo_x"); err != nil {
		t.Fatal(err)
	}
	st.Pool.QueryRow(ctx, `SELECT applied_at FROM pending_destructive_ops WHERE id='pdo_x'`).Scan(&applied)
	if applied == nil {
		t.Fatal("the owning server should mark its own destructive op applied")
	}
}

// TestRepoKeySurvivesDelete is the SIGMA-170 regression. The restic repo
// password lived only on the backup_policies row, which cascades from
// resources (and thus from projects/environments). Deleting a database — or the
// project it sat in — destroyed the only key to that customer's offsite
// snapshots, while the snapshots themselves stayed in their bucket, still
// billing and now permanently undecryptable. The key must outlive the cascade,
// and an operator must be able to export it.
func TestRepoKeySurvivesDelete(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_rk"
	envID, serverID := dbTestFixture(t, st, orgID, true, "database")

	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "ledger", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	target, err := st.CreateBackupTarget(ctx, orgID, "admin", store.CreateBackupTargetInput{
		Name: "minio", Endpoint: "http://minio.internal:9000", Bucket: "backups",
		AccessKey: "AKIA123", SecretKey: "supersecret", ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	tid := target.ID
	if _, err := st.UpdateBackupPolicy(ctx, orgID, res.ID, "admin", store.UpdateBackupPolicyInput{TargetID: &tid}); err != nil {
		t.Fatal(err)
	}
	// A sweep materialises the repo key (ensureRepoKeyTx runs on first schedule).
	if _, err := st.CreateDueBackupRuns(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}

	before, err := st.ExportRepoKey(ctx, orgID, res.ID, "admin")
	if err != nil {
		t.Fatalf("export while live: %v", err)
	}
	if before == "" {
		t.Fatal("exported repo key is empty")
	}

	if _, err := st.DeleteResource(ctx, orgID, res.ID, "admin"); err != nil {
		t.Fatal(err)
	}

	// The whole point: the same password still opens the snapshots afterwards.
	after, err := st.ExportRepoKey(ctx, orgID, res.ID, "admin")
	if err != nil {
		t.Fatalf("export after delete: %v", err)
	}
	if after != before {
		t.Fatalf("repo key changed across delete: %q → %q", before, after)
	}

	archived, err := st.ListArchivedRepoKeys(ctx, orgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 || archived[0].ResourceID != res.ID {
		t.Fatalf("archived keys = %+v, want one for %s", archived, res.ID)
	}
	if archived[0].ResourceName != "ledger" {
		t.Errorf("archived resource name = %q, want ledger", archived[0].ResourceName)
	}

	// A resource that never had a policy has nothing to export — 404, not a
	// zero-value key that would look like a valid password.
	if _, err := st.ExportRepoKey(ctx, orgID, "res_nonexistent", "admin"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("export for unknown resource = %v, want ErrNotFound", err)
	}
}
