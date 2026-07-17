package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// Op kinds registered on both the control plane (reconciler render) and the
// agent. Kept byte-identical with the CP's dsd package.
const (
	KindBackupRun     = "backup.run"
	KindBackupBase    = "backup.base"
	KindBackupVerify  = "backup.verify"
	KindBackupRestore = "backup.restore"
)

// opTimeout bounds one backup/verify/restore execution agent-side, under the
// CP scheduler's 30-minute stale-run sweep so the agent gives up first and
// reports an honest failure.
const opTimeout = 25 * time.Minute

// ErrRunSettled is returned by a CredentialFetcher when the CP reports the run
// is no longer open (already terminal) — the op becomes a no-op, which happens
// when a stale queued DSD version still carries a completed run.
var ErrRunSettled = errors.New("backup run already settled")

// CredentialFetcher resolves one run's restic material from the control plane.
type CredentialFetcher func(ctx context.Context, runID string) (Credential, error)

// Reporter posts a run's terminal outcome (with snapshot id + dump sha) to the
// control plane. Best-effort: the DSD op status is the failure fallback.
type Reporter func(ctx context.Context, runID string, ok bool, snapshotID, dumpSha, detail string)

// Docker is the slice of the container runtime the backup ops need.
type Docker interface {
	ContainerExec(ctx context.Context, containerID string, cmd []string, out io.Writer) (int, string, error)
	PutArchive(ctx context.Context, containerID, path string, tarStream io.Reader) error
	ContainerCreate(ctx context.Context, name string, body any) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerRemove(ctx context.Context, id string, force bool) error
	ImagePull(ctx context.Context, image string) error
}

// opSpec mirrors the CP reconciler's backupOpSpec wire payload.
type opSpec struct {
	RunID           string `json:"runId"`
	ResourceID      string `json:"resourceId"`
	Container       string `json:"container"`
	Engine          string `json:"engine"`
	Image           string `json:"image"`
	Database        string `json:"database"`
	Username        string `json:"username"`
	KeepDaily       int    `json:"keepDaily"`
	KeepWeekly      int    `json:"keepWeekly"`
	KeepMonthly     int    `json:"keepMonthly"`
	ExpectedSha     string `json:"expectedSha"`
	TargetContainer string `json:"targetContainer"`
	TargetDatabase  string `json:"targetDatabase"`
	TargetUsername  string `json:"targetUsername"`
}

// Runner owns the backup op handlers.
type Runner struct {
	docker  Docker
	creds   CredentialFetcher
	report  Reporter
	workDir string // host scratch space for verify/restore dumps (0700, wiped per run)
	log     *slog.Logger
}

func NewRunner(docker Docker, creds CredentialFetcher, report Reporter, workDir string, log *slog.Logger) *Runner {
	return &Runner{docker: docker, creds: creds, report: report, workDir: workDir, log: log}
}

// Register wires the backup op kinds into the apply registry — the single
// enforcement point that keeps unregistered op kinds rejected.
func (r *Runner) Register(reg *apply.Registry) {
	reg.Register(KindBackupRun, r.opBackupRun)
	reg.Register(KindBackupBase, r.opBackupBase)
	reg.Register(KindBackupVerify, r.opBackupVerify)
	reg.Register(KindBackupRestore, r.opBackupRestore)
}

func parseSpec(op dsd.Op) (opSpec, error) {
	var s opSpec
	if err := json.Unmarshal(op.Spec, &s); err != nil {
		return s, fmt.Errorf("decode backup spec: %w", err)
	}
	if s.RunID == "" || s.Engine == "" {
		return s, fmt.Errorf("backup spec missing runId/engine")
	}
	return s, nil
}

// fail reports and returns a run failure in one step.
func (r *Runner) fail(ctx context.Context, runID string, err error) error {
	r.report(ctx, runID, false, "", "", err.Error())
	return err
}

// opBackupRun: engine-native dump (docker exec inside the database's own
// container) piped straight into restic --stdin — the dump stream never
// touches host disk on the backup path. Followed by restic check and the GFS
// forget/prune. The dump's sha256 is recorded for restore-verify.
func (r *Runner) opBackupRun(ctx context.Context, op dsd.Op) error {
	spec, err := parseSpec(op)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	cred, err := r.creds(ctx, spec.RunID)
	if errors.Is(err, ErrRunSettled) {
		r.log.Info("backup run already settled; skipping", "run", spec.RunID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch backup credential: %w", err)
	}
	if err := resticInit(ctx, cred); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("init repository: %w", err))
	}
	dumpCmd, err := dumpCommand(spec.Engine, spec.Database, spec.Username)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}

	hasher := sha256.New()
	pr, pw := io.Pipe()
	execDone := make(chan error, 1)
	go func() {
		exitCode, stderrTail, execErr := r.docker.ContainerExec(ctx, spec.Container, dumpCmd, io.MultiWriter(pw, hasher))
		switch {
		case execErr != nil:
			execErr = fmt.Errorf("dump exec: %w", execErr)
		case exitCode != 0:
			execErr = fmt.Errorf("dump exited %d: %s", exitCode, stderrTail)
		}
		// Closing with the dump's error makes restic's stdin read fail, so a
		// broken dump can never become a "successful" snapshot.
		pw.CloseWithError(execErr)
		execDone <- execErr
	}()

	snapshotID, backupErr := resticBackupStdin(ctx, cred, pr, dumpFilename(spec.Engine))
	dumpErr := <-execDone
	if dumpErr != nil {
		return r.fail(ctx, spec.RunID, dumpErr)
	}
	if backupErr != nil {
		return r.fail(ctx, spec.RunID, backupErr)
	}
	if err := resticCheck(ctx, cred); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("post-backup check: %w", err))
	}
	if spec.KeepDaily > 0 {
		if err := resticForget(ctx, cred, spec.KeepDaily, spec.KeepWeekly, spec.KeepMonthly); err != nil {
			// Retention failure must not undo a good backup: report success with
			// the forget error in the detail so the operator sees it.
			sha := hex.EncodeToString(hasher.Sum(nil))
			r.report(ctx, spec.RunID, true, snapshotID, sha, "backup ok; retention failed: "+err.Error())
			return nil
		}
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	r.report(ctx, spec.RunID, true, snapshotID, sha, "")
	r.log.Info("backup complete", "run", spec.RunID, "snapshot", snapshotID)
	return nil
}

// opBackupBase: physical base backup (P2-5). pg_basebackup streams a tar to
// stdout (docker exec inside the DB container), piped straight into restic
// --stdin under the "base" tag — the PITR starting point WAL segments replay
// from. The stream never touches host disk, and a broken pg_basebackup fails
// restic's stdin (CloseWithError) so it can't become a "successful" snapshot.
func (r *Runner) opBackupBase(ctx context.Context, op dsd.Op) error {
	spec, err := parseSpec(op)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	cred, err := r.creds(ctx, spec.RunID)
	if errors.Is(err, ErrRunSettled) {
		r.log.Info("base backup already settled; skipping", "run", spec.RunID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch backup credential: %w", err)
	}
	if err := resticInit(ctx, cred); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("init repository: %w", err))
	}
	baseCmd, err := baseBackupCommand(spec.Engine, spec.Username)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}

	hasher := sha256.New()
	pr, pw := io.Pipe()
	execDone := make(chan error, 1)
	go func() {
		exitCode, stderrTail, execErr := r.docker.ContainerExec(ctx, spec.Container, baseCmd, io.MultiWriter(pw, hasher))
		switch {
		case execErr != nil:
			execErr = fmt.Errorf("basebackup exec: %w", execErr)
		case exitCode != 0:
			execErr = fmt.Errorf("basebackup exited %d: %s", exitCode, stderrTail)
		}
		pw.CloseWithError(execErr)
		execDone <- execErr
	}()

	snapshotID, backupErr := resticBackupStdinTagged(ctx, cred, pr, "base.tar", "base")
	baseErr := <-execDone
	if baseErr != nil {
		return r.fail(ctx, spec.RunID, baseErr)
	}
	if backupErr != nil {
		return r.fail(ctx, spec.RunID, backupErr)
	}
	if err := resticCheck(ctx, cred); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("post-basebackup check: %w", err))
	}
	sha := hex.EncodeToString(hasher.Sum(nil))
	r.report(ctx, spec.RunID, true, snapshotID, sha, "")
	r.log.Info("base backup complete", "run", spec.RunID, "snapshot", snapshotID)
	return nil
}

// dumpToWorkFile streams the latest snapshot's dump to a 0600 host work file,
// returning its path, sha256 and size. Verify/restore need a seekable file to
// tar into the target container; it is removed by the caller's cleanup.
// (The backup path itself never lands on host disk — this is restore-side
// staging, wiped immediately after the load.)
func (r *Runner) dumpToWorkFile(ctx context.Context, cred Credential, runID, engine string) (path, sha string, size int64, err error) {
	dir := filepath.Join(r.workDir, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", 0, err
	}
	path = filepath.Join(dir, dumpFilename(engine))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", "", 0, err
	}
	hasher := sha256.New()
	err = resticDumpLatest(ctx, cred, dumpFilename(engine), io.MultiWriter(f, hasher))
	cerr := f.Close()
	if err != nil {
		return "", "", 0, fmt.Errorf("restic dump: %w", err)
	}
	if cerr != nil {
		return "", "", 0, cerr
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", "", 0, err
	}
	return path, hex.EncodeToString(hasher.Sum(nil)), st.Size(), nil
}

// putFile tars one file into the container at destDir/<name>.
func (r *Runner) putFile(ctx context.Context, containerID, destDir, name, path string, size int64) error {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		werr := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size})
		if werr == nil {
			var f *os.File
			if f, werr = os.Open(path); werr == nil {
				_, werr = io.Copy(tw, f)
				f.Close()
			}
		}
		if werr == nil {
			werr = tw.Close()
		}
		pw.CloseWithError(werr)
	}()
	return r.docker.PutArchive(ctx, containerID, destDir, pr)
}

// execOK runs a command in a container and errors on a non-zero exit.
func (r *Runner) execOK(ctx context.Context, containerID string, cmd []string, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	exitCode, stderrTail, err := r.docker.ContainerExec(ctx, containerID, cmd, out)
	if err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("%s exited %d: %s", cmd[0], exitCode, stderrTail)
	}
	return nil
}

// waitReady polls the engine's readiness probe until it succeeds or the
// deadline passes.
func (r *Runner) waitReady(ctx context.Context, containerID string, cmd []string, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	for {
		if err := r.execOK(ctx, containerID, cmd, nil); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("engine not ready within %s", deadline)
		case <-time.After(2 * time.Second):
		}
	}
}

// loadAndProbe places the dump into the container, loads it and runs the
// row-count probe. Shared by verify (scratch container) and restore (target).
func (r *Runner) loadAndProbe(ctx context.Context, containerID, engine, database, username, dumpPath string, size int64) (string, error) {
	if err := r.putFile(ctx, containerID, "/tmp", "load.dump", dumpPath, size); err != nil {
		return "", fmt.Errorf("stage dump: %w", err)
	}
	loadCmd, err := loadCommand(engine, database, username, "/tmp/load.dump")
	if err != nil {
		return "", err
	}
	if err := r.execOK(ctx, containerID, loadCmd, nil); err != nil {
		return "", fmt.Errorf("load: %w", err)
	}
	probeCmd, err := probeCommand(engine, database, username)
	if err != nil {
		return "", err
	}
	if probeCmd == nil {
		return "", nil
	}
	var probeOut bytes.Buffer
	if err := r.execOK(ctx, containerID, probeCmd, &limitedBuffer{buf: &probeOut, max: 256}); err != nil {
		return "", fmt.Errorf("probe: %w", err)
	}
	return strings.TrimSpace(probeOut.String()), nil
}

// opBackupVerify: the automated restore-verify. Byte-level half — restic dump
// of the latest snapshot must hash to exactly the sha recorded at backup time.
// Serving half — the dump is loaded into a throwaway scratch container (no
// network, no ports, throwaway credentials) and a row-count probe must answer.
// An unrestored backup counts as no backup.
func (r *Runner) opBackupVerify(ctx context.Context, op dsd.Op) error {
	spec, err := parseSpec(op)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	cred, err := r.creds(ctx, spec.RunID)
	if errors.Is(err, ErrRunSettled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch backup credential: %w", err)
	}
	defer os.RemoveAll(filepath.Join(r.workDir, spec.RunID))
	dumpPath, sha, size, err := r.dumpToWorkFile(ctx, cred, spec.RunID, spec.Engine)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	if spec.ExpectedSha != "" && sha != spec.ExpectedSha {
		return r.fail(ctx, spec.RunID, fmt.Errorf("checksum mismatch: restored %s, recorded %s", sha[:12], spec.ExpectedSha[:12]))
	}

	// Scratch container: engine image, isolated (network none), throwaway
	// bootstrap credentials, no managed label so the drift GC never touches it.
	name := "sigmahub-verify-" + spec.RunID
	env := scratchEnv(spec.Engine, spec.Database, spec.Username)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	envList := make([]string, 0, len(env))
	for _, k := range keys {
		envList = append(envList, k+"="+env[k])
	}
	body := map[string]any{
		"Image":  spec.Image,
		"Env":    envList,
		"Labels": map[string]string{"sigmahub.verify": "true"},
		"HostConfig": map[string]any{
			"NetworkMode":   "none",
			"SecurityOpt":   []string{"no-new-privileges:true", "apparmor=docker-default"},
			"Privileged":    false,
			"RestartPolicy": map[string]any{"Name": "no"},
		},
	}
	id, err := r.docker.ContainerCreate(ctx, name, body)
	if err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("create scratch container: %w", err))
	}
	defer func() {
		// Best-effort teardown on the background context: ctx may already be done.
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		if err := r.docker.ContainerRemove(rmCtx, id, true); err != nil {
			r.log.Warn("verify scratch remove failed", "err", err, "container", name)
		}
	}()
	if err := r.docker.ContainerStart(ctx, id); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("start scratch container: %w", err))
	}
	// redis verify is redis-check-rdb over the staged file — no server
	// readiness (the scratch server runs unauthenticated defaults).
	if spec.Engine != "redis" {
		readyCmd, err := readyCommand(spec.Engine, spec.Database, spec.Username)
		if err != nil {
			return r.fail(ctx, spec.RunID, err)
		}
		if err := r.waitReady(ctx, id, readyCmd, 90*time.Second); err != nil {
			return r.fail(ctx, spec.RunID, err)
		}
	}
	probe, err := r.loadAndProbe(ctx, id, spec.Engine, spec.Database, spec.Username, dumpPath, size)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	detail := "checksum ok; scratch load ok"
	if probe != "" {
		detail += "; probe=" + probe
	}
	r.report(ctx, spec.RunID, true, "", sha, detail)
	r.log.Info("restore-verify passed", "run", spec.RunID, "resource", spec.ResourceID)
	return nil
}

// opBackupRestore: the fire-drill flow — load the latest snapshot into the
// freshly provisioned target resource's own container (P1-10 created it with
// new credentials; the engine-native load runs inside it).
func (r *Runner) opBackupRestore(ctx context.Context, op dsd.Op) error {
	spec, err := parseSpec(op)
	if err != nil {
		return err
	}
	if spec.TargetContainer == "" {
		return fmt.Errorf("restore spec missing target container")
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	cred, err := r.creds(ctx, spec.RunID)
	if errors.Is(err, ErrRunSettled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetch backup credential: %w", err)
	}
	defer os.RemoveAll(filepath.Join(r.workDir, spec.RunID))
	dumpPath, sha, size, err := r.dumpToWorkFile(ctx, cred, spec.RunID, spec.Engine)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	// The CP pins the last successful backup's digest on the run — a restore
	// must never load bytes that don't match what was backed up.
	if spec.ExpectedSha != "" && sha != spec.ExpectedSha {
		return r.fail(ctx, spec.RunID, fmt.Errorf("checksum mismatch: restored %s, recorded %s", sha[:12], spec.ExpectedSha[:12]))
	}
	readyCmd, err := readyCommand(spec.Engine, spec.TargetDatabase, spec.TargetUsername)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	if err := r.waitReady(ctx, spec.TargetContainer, readyCmd, 120*time.Second); err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	probe, err := r.loadAndProbe(ctx, spec.TargetContainer, spec.Engine, spec.TargetDatabase, spec.TargetUsername, dumpPath, size)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	detail := "restored into " + spec.TargetContainer
	if probe != "" {
		detail += "; probe=" + probe
	}
	r.report(ctx, spec.RunID, true, "", sha, detail)
	r.log.Info("restore complete", "run", spec.RunID, "target", spec.TargetContainer)
	return nil
}

