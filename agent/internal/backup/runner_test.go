package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// stubRestic writes a shell script that emulates the restic subcommands the
// runner uses: backup --stdin records stdin to a state file and prints the
// JSON summary; dump replays it; init/check/forget record their invocation.
func stubRestic(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
state="` + dir + `"
echo "$@" >> "$state/calls.log"
# Global flags (restic's -o/--option, e.g. the S3 addressing style) come before
# the subcommand, exactly as the real binary takes them.
while [ $# -gt 0 ]; do
  case "$1" in
    -*) shift ;;
    *) break ;;
  esac
done
cmd="$1"
case "$cmd" in
  init) exit 0 ;;
  backup) cat > "$state/stored.bin"; echo '{"message_type":"summary","snapshot_id":"snap123"}' ;;
  check) exit 0 ;;
  forget) exit 0 ;;
  dump)
    # emulate ` + "`dump [--path /f] latest <file>`" + `: only the logical-dump
    # snapshot carries the dump file. Without a --path filter, ` + "`latest`" + ` would
    # resolve to the newest (WAL/base) snapshot in a PITR repo, which lacks it —
    # model that as a hard failure so dropping the filter is caught (SIGMA-83).
    case "$*" in
      *--path*) cat "$state/stored.bin" ;;
      *) echo "file not found in snapshot" >&2; exit 1 ;;
    esac ;;
  snapshots) tag="$3"; if [ -f "$state/snapshots-$tag.json" ]; then cat "$state/snapshots-$tag.json"; else echo "[]"; fi ;;
  restore) snap="$2"; target="$4"; mkdir -p "$target"; case "$snap" in base*) echo basedata > "$target/base.tar" ;; *) echo waldata > "$target/wal-$snap.tar" ;; esac ;;
  *) echo "unknown cmd $cmd" >&2; exit 1 ;;
esac
`
	path := filepath.Join(dir, "restic")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := resticBin
	resticBin = path
	t.Cleanup(func() { resticBin = old })
}

// fakeDocker implements the Docker slice: exec of a dump command emits
// dumpData; scratch lifecycle and loads are recorded.
type fakeDocker struct {
	dumpData  []byte
	execCalls [][]string
	created   []string
	removed   []string
	putPaths  []string
	execFail  bool
}

func (f *fakeDocker) ContainerExec(_ context.Context, _ string, cmd []string, out io.Writer) (int, string, error) {
	f.execCalls = append(f.execCalls, cmd)
	if f.execFail {
		return 1, "boom", nil
	}
	// The dump commands write the payload; ready/load/probe commands just succeed.
	if cmd[0] == "pg_dump" || cmd[0] == "pg_basebackup" || strings.Contains(strings.Join(cmd, " "), "mysqldump") {
		_, _ = out.Write(f.dumpData)
	}
	if strings.Contains(strings.Join(cmd, " "), "information_schema.tables") {
		_, _ = out.Write([]byte("42\n"))
	}
	return 0, "", nil
}

func (f *fakeDocker) PutArchive(_ context.Context, _ string, path string, tarStream io.Reader) error {
	f.putPaths = append(f.putPaths, path)
	_, _ = io.Copy(io.Discard, tarStream)
	return nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, name string, _ any) (string, error) {
	f.created = append(f.created, name)
	return "cid-" + name, nil
}
func (f *fakeDocker) ContainerStart(context.Context, string) error { return nil }
func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ bool) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeDocker) ImagePull(context.Context, string) error { return nil }

type reported struct {
	ok            bool
	snapshot, sha string
	detail        string
	count         int
}

func testRunner(t *testing.T, fd *fakeDocker) (*Runner, *reported) {
	t.Helper()
	dir := t.TempDir()
	stubRestic(t, dir)
	rep := &reported{}
	r := NewRunner(fd,
		func(context.Context, string) (Credential, error) {
			return Credential{Repository: "s3:s3.example/bucket/sigmahub/res_db", RepoKey: "k", AccessKey: "a", SecretKey: "s"}, nil
		},
		func(_ context.Context, _ string, ok bool, snapshotID, sha, detail string) {
			rep.ok, rep.snapshot, rep.sha, rep.detail = ok, snapshotID, sha, detail
			rep.count++
		},
		filepath.Join(dir, "work"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return r, rep
}

func backupOp(t *testing.T, kind string, spec opSpec) dsd.Op {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return dsd.Op{ID: "bkr:" + spec.RunID, Kind: kind, Spec: b}
}

func TestBackupRunPipesDumpIntoResticAndReportsSha(t *testing.T) {
	dump := []byte("-- pg dump payload --")
	fd := &fakeDocker{dumpData: dump}
	r, rep := testRunner(t, fd)

	op := backupOp(t, KindBackupRun, opSpec{
		RunID: "run_1", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma", KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6,
	})
	if err := r.opBackupRun(context.Background(), op); err != nil {
		t.Fatalf("backup run: %v", err)
	}
	wantSha := sha256.Sum256(dump)
	if !rep.ok || rep.snapshot != "snap123" || rep.sha != hex.EncodeToString(wantSha[:]) {
		t.Fatalf("reported = %+v", rep)
	}
	// The dump command must be the engine-native tool, never a raw shell from
	// the DSD (the spec carries no command field at all).
	if fd.execCalls[0][0] != "pg_dump" {
		t.Fatalf("dump cmd = %v", fd.execCalls[0])
	}
}

func TestBaseBackupPipesIntoResticUnderBaseTag(t *testing.T) {
	base := []byte("PG_BASEBACKUP_TAR_STREAM")
	fd := &fakeDocker{dumpData: base}
	r, rep := testRunner(t, fd)

	op := backupOp(t, KindBackupBase, opSpec{
		RunID: "run_base", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma",
	})
	if err := r.opBackupBase(context.Background(), op); err != nil {
		t.Fatalf("base backup: %v", err)
	}
	wantSha := sha256.Sum256(base)
	if !rep.ok || rep.snapshot != "snap123" || rep.sha != hex.EncodeToString(wantSha[:]) {
		t.Fatalf("reported = %+v", rep)
	}
	// Must use pg_basebackup, never a shell from the DSD.
	if fd.execCalls[0][0] != "pg_basebackup" {
		t.Fatalf("base cmd = %v", fd.execCalls[0])
	}
}

func TestBaseBackupRejectsNonPostgres(t *testing.T) {
	r, _ := testRunner(t, &fakeDocker{})
	err := r.opBackupBase(context.Background(), backupOp(t, KindBackupBase, opSpec{
		RunID: "run_x", ResourceID: "res_db", Container: "c", Engine: "mysql", Database: "d", Username: "u",
	}))
	if err == nil {
		t.Fatal("base backup on mysql must fail")
	}
}

func TestBackupRunFailedDumpNeverReportsSuccess(t *testing.T) {
	fd := &fakeDocker{execFail: true}
	r, rep := testRunner(t, fd)
	op := backupOp(t, KindBackupRun, opSpec{
		RunID: "run_2", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma",
	})
	if err := r.opBackupRun(context.Background(), op); err == nil {
		t.Fatal("failed dump must fail the op")
	}
	if rep.ok || rep.count != 1 {
		t.Fatalf("reported = %+v, want single failure report", rep)
	}
}

func TestVerifyChecksAgainstRecordedShaAndScratchLoads(t *testing.T) {
	dump := []byte("verify payload")
	fd := &fakeDocker{dumpData: dump}
	r, rep := testRunner(t, fd)

	// Backup first so the stub repo holds the payload.
	if err := r.opBackupRun(context.Background(), backupOp(t, KindBackupRun, opSpec{
		RunID: "run_3", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma",
	})); err != nil {
		t.Fatal(err)
	}
	recorded := rep.sha

	err := r.opBackupVerify(context.Background(), backupOp(t, KindBackupVerify, opSpec{
		RunID: "run_4", ResourceID: "res_db", Engine: "postgres",
		Image: "postgres:16.6", Database: "shop", Username: "sigma", ExpectedSha: recorded,
	}))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !rep.ok || rep.sha != recorded || !strings.Contains(rep.detail, "checksum ok") {
		t.Fatalf("reported = %+v", rep)
	}
	// Scratch container was created and removed; the dump was staged into it. Two
	// removes: a best-effort pre-clear by name (SIGMA-124) plus the deferred
	// teardown by id.
	if len(fd.created) != 1 || !strings.HasPrefix(fd.created[0], "sigmahub-verify-") {
		t.Fatalf("scratch containers = %v", fd.created)
	}
	if len(fd.removed) != 2 || len(fd.putPaths) != 1 {
		t.Fatalf("teardown/staging = removed %v put %v", fd.removed, fd.putPaths)
	}
	// The work file is wiped after the run.
	if _, err := os.Stat(filepath.Join(r.workDir, "run_4")); !os.IsNotExist(err) {
		t.Fatal("verify work dir must be removed")
	}
}

func TestVerifyFailsOnChecksumMismatch(t *testing.T) {
	dump := []byte("payload v1")
	fd := &fakeDocker{dumpData: dump}
	r, rep := testRunner(t, fd)

	if err := r.opBackupRun(context.Background(), backupOp(t, KindBackupRun, opSpec{
		RunID: "run_5", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma",
	})); err != nil {
		t.Fatal(err)
	}
	wrong := sha256.Sum256([]byte("different payload"))
	err := r.opBackupVerify(context.Background(), backupOp(t, KindBackupVerify, opSpec{
		RunID: "run_6", ResourceID: "res_db", Engine: "postgres",
		Image: "postgres:16.6", Database: "shop", Username: "sigma",
		ExpectedSha: hex.EncodeToString(wrong[:]),
	}))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if rep.ok {
		t.Fatal("mismatch must report failure")
	}
	// No scratch container should have been created for a bad checksum.
	if len(fd.created) != 0 {
		t.Fatalf("scratch containers = %v", fd.created)
	}
}

func TestRestoreLoadsIntoTargetContainer(t *testing.T) {
	dump := []byte("restore payload")
	fd := &fakeDocker{dumpData: dump}
	r, rep := testRunner(t, fd)

	if err := r.opBackupRun(context.Background(), backupOp(t, KindBackupRun, opSpec{
		RunID: "run_7", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma",
	})); err != nil {
		t.Fatal(err)
	}
	// A restore carries the recorded digest of the snapshot it loads (SIGMA-78);
	// here it matches the dump the fake restic serves.
	sum := sha256.Sum256(dump)
	err := r.opBackupRestore(context.Background(), backupOp(t, KindBackupRestore, opSpec{
		RunID: "run_8", ResourceID: "res_db", Engine: "postgres", Image: "postgres:16.6",
		Database: "shop", Username: "sigma", ExpectedSha: hex.EncodeToString(sum[:]),
		TargetContainer: "sigmahub-res_new", TargetDatabase: "shop_restore", TargetUsername: "sigma",
	}))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !rep.ok || !strings.Contains(rep.detail, "restored into sigmahub-res_new") {
		t.Fatalf("reported = %+v", rep)
	}
	// Restore never creates a scratch container — it loads into the target.
	if len(fd.created) != 0 {
		t.Fatalf("restore must not create containers, got %v", fd.created)
	}
	// The load command targeted the restore database.
	joined := ""
	for _, c := range fd.execCalls {
		joined += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(joined, "shop_restore") {
		t.Fatalf("load must target the new database:\n%s", joined)
	}
}

// TestRestoreRefusesEmptyRecordedDigest is the SIGMA-78 gate: a restore whose
// run carries no recorded checksum is refused rather than silently loading an
// unverifiable snapshot into the freshly provisioned target.
func TestRestoreRefusesEmptyRecordedDigest(t *testing.T) {
	fd := &fakeDocker{dumpData: []byte("restore payload")}
	r, rep := testRunner(t, fd)
	if err := r.opBackupRun(context.Background(), backupOp(t, KindBackupRun, opSpec{
		RunID: "run_9", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma",
	})); err != nil {
		t.Fatal(err)
	}
	err := r.opBackupRestore(context.Background(), backupOp(t, KindBackupRestore, opSpec{
		RunID: "run_10", ResourceID: "res_db", Engine: "postgres", Image: "postgres:16.6",
		Database: "shop", Username: "sigma", // ExpectedSha deliberately empty
		TargetContainer: "sigmahub-res_new", TargetDatabase: "shop_restore", TargetUsername: "sigma",
	}))
	if err == nil || !strings.Contains(err.Error(), "no recorded checksum") {
		t.Fatalf("want refusal on empty digest, got %v", err)
	}
	if rep.ok {
		t.Fatal("empty-digest restore must report failure")
	}
	// It must bail before loading into the target: no exec references the target
	// database (only the earlier backup run's pg_dump of the source is present).
	for _, c := range fd.execCalls {
		if strings.Contains(strings.Join(c, " "), "shop_restore") {
			t.Fatalf("refused restore still touched the target: %v", fd.execCalls)
		}
	}
}

// TestShortShaNeverPanics guards the digest formatter against a malformed (too
// short) digest, which a raw slice would panic on (SIGMA-78).
func TestShortShaNeverPanics(t *testing.T) {
	for _, s := range []string{"", "abc", "0123456789ab", "0123456789abcdef"} {
		got := shortSha(s)
		if len(s) < 12 && got != s {
			t.Fatalf("shortSha(%q) = %q, want %q", s, got, s)
		}
		if len(s) >= 12 && len(got) != 12 {
			t.Fatalf("shortSha(%q) = %q, want 12 chars", s, got)
		}
	}
}

// TestVerifyDumpFiltersByPath is the SIGMA-83 guard: verify/restore must select
// the logical-dump snapshot by --path, not the newest snapshot repo-wide (which
// for a PITR resource is a continuously-shipped WAL bundle with no dump file).
func TestVerifyDumpFiltersByPath(t *testing.T) {
	dir := t.TempDir()
	stubRestic(t, dir)
	fd := &fakeDocker{dumpData: []byte("payload")}
	rep := &reported{}
	r := NewRunner(fd,
		func(context.Context, string) (Credential, error) {
			return Credential{Repository: "s3:s3.example/bucket/sigmahub/res_db", RepoKey: "k", AccessKey: "a", SecretKey: "s"}, nil
		},
		func(_ context.Context, _ string, ok bool, snapshotID, sha, detail string) {
			rep.ok, rep.snapshot, rep.sha, rep.detail = ok, snapshotID, sha, detail
			rep.count++
		},
		filepath.Join(dir, "work"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err := r.opBackupRun(context.Background(), backupOp(t, KindBackupRun, opSpec{
		RunID: "run_pp1", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma",
	})); err != nil {
		t.Fatal(err)
	}
	if err := r.opBackupVerify(context.Background(), backupOp(t, KindBackupVerify, opSpec{
		RunID: "run_pp2", ResourceID: "res_db", Engine: "postgres", Image: "postgres:16.6",
		Database: "shop", Username: "sigma",
	})); err != nil {
		t.Fatalf("verify: %v", err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "dump --path /dump.sql latest dump.sql") {
		t.Fatalf("dump must filter to the logical-dump snapshot by path:\n%s", calls)
	}
}

func TestDumpCommandsNeverCarrySecrets(t *testing.T) {
	// Host-side argv must reference in-container env vars, never values; a DSD
	// spec has no command field, so this catalog is the only source.
	for _, engine := range []string{"postgres", "mysql", "redis", "mongodb"} {
		cmd, err := dumpCommand(engine, "db", "user")
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(cmd, " ")
		if strings.Contains(joined, "password=") {
			t.Fatalf("%s dump leaks a literal: %q", engine, joined)
		}
		if engine != "postgres" && !strings.Contains(joined, "$") {
			t.Fatalf("%s dump must take credentials from container env: %q", engine, joined)
		}
	}
	if _, err := dumpCommand("clickhouse", "db", "user"); err == nil {
		t.Fatal("unknown engine must be rejected")
	}
}

// TestPITRRestoreRecoversAndLoadsIntoTarget is the P2-5b orchestration contract
// (stub restic + fake docker): pick the newest base ≤ the target, stage base +
// WAL, run recovery in a throwaway container (created + removed), then pg_dump
// the recovered state and load it into the fresh target. Real WAL replay is a
// postgres path validated on staging; this asserts the flow, not the bytes.
func TestPITRRestoreRecoversAndLoadsIntoTarget(t *testing.T) {
	dir := t.TempDir()
	stubRestic(t, dir)
	now := time.Now().UTC()
	base := now.Add(-2 * time.Hour)
	target := now.Add(-1 * time.Hour)
	// base ≤ target; two WAL bundles, one before and one after the target.
	if err := os.WriteFile(filepath.Join(dir, "snapshots-base.json"),
		[]byte(`[{"id":"basesnap","time":"`+base.Format(time.RFC3339Nano)+`"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshots-wal.json"),
		[]byte(`[{"id":"walA","time":"`+base.Add(30*time.Minute).Format(time.RFC3339Nano)+`"},`+
			`{"id":"walB","time":"`+now.Format(time.RFC3339Nano)+`"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	fd := &fakeDocker{dumpData: []byte("recovered dump payload")}
	rep := &reported{}
	r := NewRunner(fd,
		func(context.Context, string) (Credential, error) {
			return Credential{Repository: "s3:s3.example/bucket/sigmahub/res_db", RepoKey: "k", AccessKey: "a", SecretKey: "s"}, nil
		},
		func(_ context.Context, _ string, ok bool, snapshotID, sha, detail string) {
			rep.ok, rep.snapshot, rep.sha, rep.detail = ok, snapshotID, sha, detail
			rep.count++
		},
		filepath.Join(dir, "work"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	op := backupOp(t, KindBackupRestorePITR, opSpec{
		RunID: "run_pitr", ResourceID: "res_db", Engine: "postgres",
		Database: "shop", Username: "sigma",
		TargetContainer: "sigmahub-res_new", TargetDatabase: "shop2", TargetUsername: "sigma",
		TargetTime: target.Format(time.RFC3339),
	})
	if err := r.opBackupRestorePITR(context.Background(), op); err != nil {
		t.Fatalf("pitr restore: %v", err)
	}
	if !rep.ok {
		t.Fatalf("expected success, detail=%q", rep.detail)
	}
	// A throwaway recovery container was created and torn down. Two removes: a
	// best-effort pre-clear by name (SIGMA-124) plus the deferred teardown by id.
	if len(fd.created) != 1 || !strings.HasPrefix(fd.created[0], "sigmahub-pitr-") {
		t.Fatalf("recovery container = %v", fd.created)
	}
	if len(fd.removed) != 2 {
		t.Fatalf("recovery container must be removed, removed=%v", fd.removed)
	}
	// pg_dump ran (inside the recovery container), then a psql load into the target.
	var sawDump, sawLoad bool
	for _, c := range fd.execCalls {
		if c[0] == "pg_dump" {
			sawDump = true
		}
		if c[0] == "psql" && strings.Contains(strings.Join(c, " "), "-f") {
			sawLoad = true
		}
	}
	if !sawDump {
		t.Fatal("recovered state must be dumped with pg_dump")
	}
	if !sawLoad {
		t.Fatal("recovered dump must be loaded into the target with psql")
	}
}

func TestPITRRestoreRejectsNonPostgres(t *testing.T) {
	r, _ := testRunner(t, &fakeDocker{})
	op := backupOp(t, KindBackupRestorePITR, opSpec{
		RunID: "run_x", Engine: "mysql", TargetContainer: "c",
		TargetTime: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	if err := r.opBackupRestorePITR(context.Background(), op); err == nil {
		t.Fatal("pitr restore must reject non-postgres engines")
	}
}

func TestPITRRestoreFailsWithoutBaseBeforeTarget(t *testing.T) {
	dir := t.TempDir()
	stubRestic(t, dir)
	now := time.Now().UTC()
	// The only base backup is NEWER than the target — nothing to replay from.
	if err := os.WriteFile(filepath.Join(dir, "snapshots-base.json"),
		[]byte(`[{"id":"basesnap","time":"`+now.Format(time.RFC3339Nano)+`"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := &reported{}
	r := NewRunner(&fakeDocker{},
		func(context.Context, string) (Credential, error) { return Credential{Repository: "s3:x/y/z"}, nil },
		func(_ context.Context, _ string, ok bool, _, _, detail string) {
			rep.ok = ok
			rep.detail = detail
			rep.count++
		},
		filepath.Join(dir, "work"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	op := backupOp(t, KindBackupRestorePITR, opSpec{
		RunID: "run_nb", Engine: "postgres", Database: "shop", Username: "sigma",
		TargetContainer: "sigmahub-res_new", TargetDatabase: "shop2", TargetUsername: "sigma",
		TargetTime: now.Add(-2 * time.Hour).Format(time.RFC3339),
	})
	if err := r.opBackupRestorePITR(context.Background(), op); err == nil {
		t.Fatal("expected failure when no base backup precedes the target")
	}
	if rep.ok {
		t.Fatal("run must be reported failed")
	}
}

// TestBackupRunDoesNotDeadlockWhenResticExitsEarly reproduces SIGMA-69: if
// restic exits non-zero WITHOUT draining stdin, the dump goroutine used to block
// on pw.Write forever and the handler hung on <-execDone. The fix closes pr
// after restic returns. Run under a timeout so a regression fails, not hangs.
func TestBackupRunDoesNotDeadlockWhenResticExitsEarly(t *testing.T) {
	dir := t.TempDir()
	// `backup` exits 1 immediately without reading stdin (the early-exit case).
	// Skip restic's global flags (the S3 addressing option) to reach the subcommand.
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -*) shift ;; *) break ;; esac; done\n" +
		"case \"$1\" in\n  init) exit 0 ;;\n  backup) exit 1 ;;\n  *) exit 0 ;;\nesac\n"
	path := filepath.Join(dir, "restic")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := resticBin
	resticBin = path
	t.Cleanup(func() { resticBin = old })

	// A large dump so pw.Write blocks with no reader draining the pipe.
	fd := &fakeDocker{dumpData: []byte(strings.Repeat("x", 1<<20))}
	rep := &reported{}
	r := NewRunner(fd,
		func(context.Context, string) (Credential, error) { return Credential{Repository: "s3:x/y/z"}, nil },
		func(_ context.Context, _ string, ok bool, _, _, detail string) {
			rep.ok = ok
			rep.detail = detail
			rep.count++
		},
		filepath.Join(dir, "work"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	op := backupOp(t, KindBackupRun, opSpec{
		RunID: "run_dl", ResourceID: "res_db", Container: "c",
		Engine: "postgres", Database: "db", Username: "u",
	})

	done := make(chan error, 1)
	go func() { done <- r.opBackupRun(context.Background(), op) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a failure from the early restic exit")
		}
		if rep.ok {
			t.Fatal("run must be reported failed")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("opBackupRun deadlocked on early restic exit (SIGMA-69 regression)")
	}
}

// TestWALRetentionRunsWhenDumpFails is the SIGMA-283 regression. resticForgetWAL
// is the ONLY thing that ever forgets a WAL bundle — the repo-wide forget groups
// by (host,paths) and every bundle has a unique stored path, so it keeps all of
// them (SIGMA-108). It used to be reachable only after the dump, the stdin
// backup and the check had all succeeded, while the WAL shipper kept pushing
// ~1,440 bundles a day regardless. A database that outgrows the agent's op cap
// therefore fails its nightly dump forever AND grows the customer's bucket
// forever, with nothing in the product saying so — the only symptom is a failed
// backup badge, which says nothing about storage.
func TestWALRetentionRunsWhenDumpFails(t *testing.T) {
	dir := t.TempDir()
	stubRestic(t, dir)
	fd := &fakeDocker{execFail: true} // pg_dump exits nonzero, as if killed
	rep := &reported{}
	r := NewRunner(fd,
		func(context.Context, string) (Credential, error) {
			return Credential{Repository: "s3:s3.example/bucket/sigmahub/res_db", RepoKey: "k", AccessKey: "a", SecretKey: "s"}, nil
		},
		func(_ context.Context, _ string, ok bool, snapshotID, sha, detail string) {
			rep.ok, rep.snapshot, rep.sha, rep.detail = ok, snapshotID, sha, detail
			rep.count++
		},
		filepath.Join(dir, "work"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	err := r.opBackupRun(context.Background(), backupOp(t, KindBackupRun, opSpec{
		RunID: "run_walret", ResourceID: "res_db", Container: "sigmahub-res_db",
		Engine: "postgres", Database: "shop", Username: "sigma",
		KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6,
	}))
	if err == nil {
		t.Fatal("failed dump must still fail the op")
	}
	if rep.ok {
		t.Fatal("failed dump must not report success")
	}

	calls, rerr := os.ReadFile(filepath.Join(dir, "calls.log"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(calls), "forget --tag wal") {
		t.Fatalf("a failed dump left WAL retention unbounded — the shipper keeps adding "+
			"bundles nothing will ever forget (SIGMA-283):\n%s", calls)
	}
	// Forgetting alone only drops snapshots; the customer keeps paying for the
	// data until it is pruned.
	if !strings.Contains(string(calls), "--prune") {
		t.Fatalf("WAL retention on a failed run must reclaim the data, not just "+
			"unreference it:\n%s", calls)
	}
}

// TestWALRetentionSurvivesAnExpiredOpDeadline pins the reason the retention pass
// does not inherit the op's context: the canonical failure it exists for is a
// dump killed by the 25-minute op cap, which leaves the op context already dead.
// Retention that inherited it would be skipped in exactly the case that needs it.
func TestWALRetentionSurvivesAnExpiredOpDeadline(t *testing.T) {
	dir := t.TempDir()
	stubRestic(t, dir)
	fd := &fakeDocker{execFail: true}
	rep := &reported{}
	r := NewRunner(fd,
		func(context.Context, string) (Credential, error) {
			return Credential{Repository: "s3:s3.example/bucket/sigmahub/res_db", RepoKey: "k", AccessKey: "a", SecretKey: "s"}, nil
		},
		func(_ context.Context, _ string, ok bool, snapshotID, sha, detail string) {
			rep.ok, rep.snapshot, rep.sha, rep.detail = ok, snapshotID, sha, detail
			rep.count++
		},
		filepath.Join(dir, "work"),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	cred := Credential{Repository: "s3:s3.example/bucket/sigmahub/res_db", RepoKey: "k"}
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	r.boundWALRetention(dead, cred, opSpec{RunID: "run_dead", KeepDaily: 7})

	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "forget --tag wal") {
		t.Fatalf("retention was skipped because the op context was already dead:\n%s", calls)
	}
}

// TestCredentialEnvHonoursForcePathStyle is the SIGMA-287 regression. The
// force_path_style column has existed since migration 0020: the create API
// accepts it, the store persists it, the target list echoes it back, both
// credential paths read it, the web client types it and this package's client
// deserialises it. It then stopped dead — backup.Credential had no such field,
// so nothing the agent ran ever addressed the bucket differently.
//
// An operator whose S3-compatible gateway needs virtual-host addressing set
// forcePathStyle:false, watched every surface confirm the setting, and kept
// getting the same S3 error, with no way to discover the toggle was decorative
// short of reading agent source.
//
// The control restic actually exposes is the extended option
// `s3.bucket-lookup` (auto|dns|path) — an argv flag, not an environment
// variable. AWS_S3_FORCE_PATH_STYLE is an AWS SDK convention that restic (which
// uses minio-go) never reads; exporting it would have been a second inert
// control, which is the disease this ticket treats. So the assertion is on the
// rendered invocation.
func TestCredentialEnvHonoursForcePathStyle(t *testing.T) {
	s3 := "s3:https://gateway.example/bucket/sigmahub/res_db"

	path := Credential{Repository: s3, ForcePathStyle: true}.opts()
	if !containsArg(path, "--option=s3.bucket-lookup=path") {
		t.Errorf("forcePathStyle:true rendered %v, want path-style bucket lookup", path)
	}
	dns := Credential{Repository: s3, ForcePathStyle: false}.opts()
	if !containsArg(dns, "--option=s3.bucket-lookup=dns") {
		t.Errorf("forcePathStyle:false rendered %v, want virtual-host bucket lookup — "+
			"the setting crossed six layers and changed nothing (SIGMA-287)", dns)
	}

	// Only S3 repositories carry an S3 addressing option.
	if got := (Credential{Repository: "/var/backups/local", ForcePathStyle: true}).opts(); len(got) != 0 {
		t.Errorf("non-s3 repository rendered %v, want no S3 option", got)
	}

	// The credential env stays exactly what it was: secrets ride there, and
	// nothing about addressing belongs in it.
	env := Credential{Repository: s3, RepoKey: "k", AccessKey: "a", SecretKey: "s", ForcePathStyle: false}.env()
	for _, e := range env {
		if strings.HasPrefix(e, "AWS_S3_FORCE_PATH_STYLE") {
			t.Errorf("env carries %q, which restic does not read — an inert control is what SIGMA-287 is about", e)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestResticInvocationCarriesTheAddressingOption proves the option reaches the
// restic process, not just the Credential method — the gap SIGMA-287 was made
// of was precisely a value that existed everywhere except in the invocation.
func TestResticInvocationCarriesTheAddressingOption(t *testing.T) {
	dir := t.TempDir()
	stubRestic(t, dir)
	cred := Credential{
		Repository: "s3:https://gateway.example/bucket/sigmahub/res_db",
		RepoKey:    "k", AccessKey: "a", SecretKey: "s",
	} // ForcePathStyle false — the operator's virtual-host gateway
	if err := resticInit(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "--option=s3.bucket-lookup=dns") {
		t.Fatalf("restic was invoked without the addressing option:\n%s", calls)
	}
}
