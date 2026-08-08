package reconciler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestRenderBackupOpsOrderingAndSecretFreedom(t *testing.T) {
	runs := []store.BackupRunSpec{
		{RunID: "run_b", Kind: "backup", ResourceID: "res_db", Engine: "postgres", Database: "shop", Username: "sigma", KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6},
		{RunID: "run_v", Kind: "verify", ResourceID: "res_db", Engine: "postgres", Database: "shop", Username: "sigma", ExpectedSha: "abc123"},
	}
	ops, _ := renderOps("srv_t", dbSpecs("postgres"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets("postgres", "database"), nil, nil, runs, nil, ACMEConfig{}, clusterRender{})

	bk, ok := opByID(ops, "bkr:run_b")
	if !ok || bk.Kind != dsd.KindBackupRun {
		t.Fatalf("backup op = %+v", bk)
	}
	// The backup depends on the database container being applied first.
	if len(bk.DependsOn) != 1 || bk.DependsOn[0] != "res:res_db" {
		t.Fatalf("backup deps = %v", bk.DependsOn)
	}
	if !strings.Contains(string(bk.Spec), `"keepDaily":7`) {
		t.Fatalf("backup spec missing retention: %s", bk.Spec)
	}
	vf, ok := opByID(ops, "bkr:run_v")
	if !ok || vf.Kind != dsd.KindBackupVerify {
		t.Fatalf("verify op = %+v", vf)
	}
	// First-day verify runs after the same-document backup.
	if len(vf.DependsOn) != 1 || vf.DependsOn[0] != "bkr:run_b" {
		t.Fatalf("verify deps = %v", vf.DependsOn)
	}
	if !strings.Contains(string(vf.Spec), `"expectedSha":"abc123"`) {
		t.Fatalf("verify spec missing pinned sha: %s", vf.Spec)
	}
	// A backup op spec carries identifiers only — no command and no credential.
	raw, _ := json.Marshal(ops)
	for _, needle := range []string{"restic", "pg_dump", "secretKey", "repoKey", "accessKey"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("DSD leaks %q", needle)
		}
	}
}

// TestRenderBackupVerifyHeldUntilBackupSha pins SIGMA-137: a verify whose
// same-day backup has not yet produced a sha (ExpectedSha empty) is NOT
// rendered — rendering it would pin a stale sha and fail against this day's
// fresh dump. It renders once the sha is known.
func TestRenderBackupVerifyHeldUntilBackupSha(t *testing.T) {
	held := []store.BackupRunSpec{
		{RunID: "run_b", Kind: "backup", ResourceID: "res_db", Engine: "postgres", Database: "shop", Username: "sigma", KeepDaily: 7},
		{RunID: "run_v", Kind: "verify", ResourceID: "res_db", Engine: "postgres", Database: "shop", Username: "sigma", ExpectedSha: ""},
	}
	ops, _ := renderOps("srv_t", dbSpecs("postgres"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets("postgres", "database"), nil, nil, held, nil, ACMEConfig{}, clusterRender{})
	if _, ok := opByID(ops, "bkr:run_v"); ok {
		t.Fatal("verify with an empty (unresolved) same-day sha must not be rendered")
	}
	// The backup itself is still rendered.
	if _, ok := opByID(ops, "bkr:run_b"); !ok {
		t.Fatal("the backup op must still render")
	}

	// Once the sha is resolved, the verify renders.
	ready := []store.BackupRunSpec{
		{RunID: "run_v", Kind: "verify", ResourceID: "res_db", Engine: "postgres", Database: "shop", Username: "sigma", ExpectedSha: "sha-today"},
	}
	ops2, _ := renderOps("srv_t", dbSpecs("postgres"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets("postgres", "database"), nil, nil, ready, nil, ACMEConfig{}, clusterRender{})
	vf, ok := opByID(ops2, "bkr:run_v")
	if !ok || vf.Kind != dsd.KindBackupVerify {
		t.Fatalf("verify should render once the sha is known: %+v", vf)
	}
	if !strings.Contains(string(vf.Spec), `"expectedSha":"sha-today"`) {
		t.Fatalf("verify spec sha = %s", vf.Spec)
	}
}

func TestRenderRestorePITROpCarriesTargetTime(t *testing.T) {
	target := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	runs := []store.BackupRunSpec{
		{RunID: "run_p", Kind: "restore-pitr", ResourceID: "res_src", Engine: "postgres",
			Database: "shop", Username: "sigma",
			RestoreResourceID: "res_new", RestoreDatabase: "shop_pitr", RestoreUsername: "sigma",
			RecoveryTargetTime: &target},
	}
	ops, _ := renderOps("srv_t", nil, nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, nil, nil, nil, runs, nil, ACMEConfig{}, clusterRender{})
	op, ok := opByID(ops, "bkr:run_p")
	if !ok || op.Kind != dsd.KindBackupRestorePITR {
		t.Fatalf("pitr op = %+v", op)
	}
	s := string(op.Spec)
	if !strings.Contains(s, `"targetContainer":"sigmahub-res_new"`) ||
		!strings.Contains(s, `"targetDatabase":"shop_pitr"`) ||
		!strings.Contains(s, `"recoveryTargetTime":"2027-03-01T12:00:00Z"`) {
		t.Fatalf("pitr spec = %s", s)
	}
	// The recovery secret/repo material never rides the DSD (same invariant as
	// every backup op).
	if strings.Contains(s, "repoKey") || strings.Contains(s, "secretKey") {
		t.Fatalf("pitr spec leaked credentials: %s", s)
	}
}

func TestRenderRestoreOpTargetsNewResource(t *testing.T) {
	runs := []store.BackupRunSpec{
		{RunID: "run_r", Kind: "restore", ResourceID: "res_src", Engine: "postgres",
			Database: "shop", Username: "sigma", ExpectedSha: "sha-a",
			RestoreResourceID: "res_new", RestoreDatabase: "shop_restore", RestoreUsername: "sigma"},
	}
	ops, _ := renderOps("srv_t", nil, nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, nil, nil, nil, runs, nil, ACMEConfig{}, clusterRender{})
	op, ok := opByID(ops, "bkr:run_r")
	if !ok || op.Kind != dsd.KindBackupRestore {
		t.Fatalf("restore op = %+v", op)
	}
	s := string(op.Spec)
	if !strings.Contains(s, `"targetContainer":"sigmahub-res_new"`) ||
		!strings.Contains(s, `"targetDatabase":"shop_restore"`) ||
		!strings.Contains(s, `"expectedSha":"sha-a"`) {
		t.Fatalf("restore spec = %s", s)
	}
}
