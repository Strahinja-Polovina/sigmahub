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

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// stubRestic writes a shell script that emulates the restic subcommands the
// runner uses: backup --stdin records stdin to a state file and prints the
// JSON summary; dump replays it; init/check/forget record their invocation.
func stubRestic(t *testing.T, dir string) {
	t.Helper()
	script := `#!/bin/sh
state="` + dir + `"
cmd="$1"
echo "$@" >> "$state/calls.log"
case "$cmd" in
  init) exit 0 ;;
  backup) cat > "$state/stored.bin"; echo '{"message_type":"summary","snapshot_id":"snap123"}' ;;
  check) exit 0 ;;
  forget) exit 0 ;;
  dump) cat "$state/stored.bin" ;;
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
	if cmd[0] == "pg_dump" || strings.Contains(strings.Join(cmd, " "), "mysqldump") {
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
	ok               bool
	snapshot, sha    string
	detail           string
	count            int
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
	// Scratch container was created and removed; the dump was staged into it.
	if len(fd.created) != 1 || !strings.HasPrefix(fd.created[0], "sigmahub-verify-") {
		t.Fatalf("scratch containers = %v", fd.created)
	}
	if len(fd.removed) != 1 || len(fd.putPaths) != 1 {
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
	err := r.opBackupRestore(context.Background(), backupOp(t, KindBackupRestore, opSpec{
		RunID: "run_8", ResourceID: "res_db", Engine: "postgres", Image: "postgres:16.6",
		Database: "shop", Username: "sigma",
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
