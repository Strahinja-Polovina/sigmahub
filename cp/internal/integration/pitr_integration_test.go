package integration

// P2-5 PITR integration: enabling PITR is postgres-only, WAL archiving turns
// on in the render target, a daily base backup joins the schedule, the WAL
// shipper's credential release + status write-back work end to end, and the
// PITR window surfaces on the database info.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestPITREndToEnd(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_pitr"
	envID, serverID := dbTestFixture(t, st, orgID, true, "database")

	pg, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "shop", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	redis, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "cache", Kind: "redis", Spec: json.RawMessage(`{}`),
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

	// PITR is postgres-only.
	on := true
	if _, err := st.UpdateBackupPolicy(ctx, orgID, redis.ID, "admin", store.UpdateBackupPolicyInput{TargetID: &tid, PitrEnabled: &on}); err == nil {
		t.Fatal("PITR on redis must be rejected")
	}
	if _, err := st.UpdateBackupPolicy(ctx, orgID, pg.ID, "admin", store.UpdateBackupPolicyInput{TargetID: &tid, PitrEnabled: &on}); err != nil {
		t.Fatalf("enable PITR on postgres: %v", err)
	}

	// The render target now carries the PITR flag; WAL targets list the resource.
	targets, err := st.DBTargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if !targets[pg.ID].PITR {
		t.Fatal("postgres render target must have PITR set")
	}
	walTargets, err := st.WALTargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if len(walTargets) != 1 || walTargets[0].ResourceID != pg.ID {
		t.Fatalf("wal targets = %v", walTargets)
	}

	// The daily schedule now includes a base backup alongside the dump backup.
	if _, err := st.CreateDueBackupRuns(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	runs, err := st.BackupRunsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, r := range runs {
		if r.ResourceID == pg.ID {
			kinds[r.Kind]++
		}
	}
	if kinds["backup"] != 1 || kinds["basebackup"] != 1 {
		t.Fatalf("PITR postgres schedule = %v, want one backup + one basebackup", kinds)
	}

	// WAL credential release: before any scheduled run the repo key doesn't
	// exist yet, so there's nothing to ship into.
	// (CreateDueBackupRuns above already ran ensureRepoKeyTx, so it now works.)
	cred, err := st.WALCredentialForResource(ctx, serverID, pg.ID)
	if err != nil {
		t.Fatalf("wal credential: %v", err)
	}
	if cred.RepoKey == "" || cred.SecretKey != "supersecret" || cred.Repository == "" {
		t.Fatalf("wal credential = %+v", cred)
	}
	// BOLA: another server can't fetch this resource's WAL credential.
	if _, err := st.WALCredentialForResource(ctx, "srv_other", pg.ID); err == nil {
		t.Fatal("cross-server WAL credential must fail")
	}
	// The release is audited.
	var audits int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM cp_audit_log WHERE org_id = $1 AND action = 'WAL repo key unwrapped (agent)'`,
		orgID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("WAL release audits = %d, want 1", audits)
	}

	// Status write-back drives the PITR window on the database info.
	seg := "000000010000000000000007"
	if err := st.SetWALStatus(ctx, serverID, pg.ID, seg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWALStatus(ctx, "srv_other", pg.ID, seg, time.Now()); err == nil {
		t.Fatal("cross-server WAL status must fail")
	}
	info, err := st.GetDatabaseInfo(ctx, orgID, pg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Backup == nil || !info.Backup.PitrEnabled {
		t.Fatal("info must report PITR enabled")
	}
	if info.LastWalSegment != seg || info.LastWalAt == nil {
		t.Fatalf("info WAL window = %q / %v", info.LastWalSegment, info.LastWalAt)
	}

	// Disabling PITR drops it from the WAL targets and the render flag.
	off := false
	if _, err := st.UpdateBackupPolicy(ctx, orgID, pg.ID, "admin", store.UpdateBackupPolicyInput{PitrEnabled: &off}); err != nil {
		t.Fatal(err)
	}
	walTargets, err = st.WALTargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if len(walTargets) != 0 {
		t.Fatalf("disabled PITR still lists wal targets: %v", walTargets)
	}
}

// TestPITRRestoreToTimestampWindow covers the P2-5b server-side window
// validation: a base backup must precede the target, the WAL archive must cover
// it, the time can't be in the future, and the happy path queues a restore-pitr
// run carrying the recovery target time. Timestamps are set directly so the
// window checks are deterministic.
func TestPITRRestoreToTimestampWindow(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_pitr_r"
	envID, serverID := dbTestFixture(t, st, orgID, true, "database")

	pg, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "shop", Kind: "postgres", Spec: json.RawMessage(`{}`),
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
	on := true
	if _, err := st.UpdateBackupPolicy(ctx, orgID, pg.ID, "admin", store.UpdateBackupPolicyInput{TargetID: &tid, PitrEnabled: &on}); err != nil {
		t.Fatal(err)
	}
	// Schedule (creates the base backup run + materializes the repo key).
	if _, err := st.CreateDueBackupRuns(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Deterministic window: base finished 2h ago, WAL archived up to 10m ago.
	baseAt := time.Now().Add(-2 * time.Hour)
	walAt := time.Now().Add(-10 * time.Minute)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE backup_runs SET status='success', finished_at=$1
		  WHERE resource_id=$2 AND kind='basebackup'`, baseAt, pg.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWALStatus(ctx, serverID, pg.ID, "000000010000000000000009", walAt); err != nil {
		t.Fatal(err)
	}

	// A fresh postgres resource is the restore target.
	fresh, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "shop-pitr", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Future target → rejected.
	if _, err := st.CreateRestoreToTimestampRun(ctx, orgID, pg.ID, fresh.ID, time.Now().Add(time.Hour), "admin"); err == nil {
		t.Fatal("future recovery time must be rejected")
	}
	// Before any base backup → rejected.
	if _, err := st.CreateRestoreToTimestampRun(ctx, orgID, pg.ID, fresh.ID, time.Now().Add(-3*time.Hour), "admin"); err == nil {
		t.Fatal("target before the base backup must be rejected")
	}
	// After the WAL archive window → rejected.
	if _, err := st.CreateRestoreToTimestampRun(ctx, orgID, pg.ID, fresh.ID, time.Now().Add(-5*time.Minute), "admin"); err == nil {
		t.Fatal("target beyond archived WAL must be rejected")
	}
	// In-window → queued restore-pitr run carrying the recovery target time.
	want := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	run, err := st.CreateRestoreToTimestampRun(ctx, orgID, pg.ID, fresh.ID, want, "admin")
	if err != nil {
		t.Fatalf("in-window restore: %v", err)
	}
	if run.Kind != "restore-pitr" {
		t.Fatalf("run kind = %q", run.Kind)
	}
	runs, err := st.BackupRunsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.BackupRunSpec
	for i := range runs {
		if runs[i].RunID == run.ID {
			found = &runs[i]
		}
	}
	if found == nil {
		t.Fatal("restore-pitr run not rendered for the server")
	}
	if found.RestoreResourceID != fresh.ID || found.RecoveryTargetTime == nil ||
		!found.RecoveryTargetTime.UTC().Truncate(time.Second).Equal(want) {
		t.Fatalf("rendered run = %+v (want target %s)", found, want)
	}

	// The PITR restore is audited.
	var audits int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM cp_audit_log WHERE org_id = $1 AND action = 'PITR restore queued'`,
		orgID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("PITR restore audits = %d, want 1", audits)
	}
}
