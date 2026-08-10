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
	KindBackupRun         = "backup.run"
	KindBackupBase        = "backup.base"
	KindBackupVerify      = "backup.verify"
	KindBackupRestore     = "backup.restore"
	KindBackupRestorePITR = "backup.restore-pitr"
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
	SnapshotID      string `json:"snapshotId"`
	TargetContainer string `json:"targetContainer"`
	TargetDatabase  string `json:"targetDatabase"`
	TargetUsername  string `json:"targetUsername"`
	TargetTime      string `json:"recoveryTargetTime"`
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
	reg.Register(KindBackupRestorePITR, r.opBackupRestorePITR)
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

// walRetentionTimeout bounds the retention pass that runs after a FAILED backup
// run. It is deliberately short next to opTimeout: this is cleanup, not the
// run's work, and it must not hold the apply loop while the next op waits.
const walRetentionTimeout = 5 * time.Minute

// boundWALRetention forgets out-of-window WAL bundles and reclaims their space,
// independently of whether this run's dump worked (SIGMA-283).
//
// resticForgetWAL is the only thing in the system that ever forgets a WAL
// bundle: the repo-wide forget groups by (host,paths) and each bundle has a
// unique stored path, so it keeps every one of them (SIGMA-108). It used to be
// reachable only after the dump, the stdin backup and the check had all
// succeeded. The WAL shipper, meanwhile, pushes on its own cadence and forgets
// nothing — so a database that outgrows the agent's op cap (pg_dump killed,
// nightly run failed) accumulates ~1,440 wal-*.tar snapshots a day that nothing
// will ever remove. The customer's bucket and bill grow without limit, restore
// times degrade as the snapshot list grows, and the only symptom in the product
// is a failed backup badge that says nothing about storage.
//
// The context is deliberately NOT the op's: the canonical failure this exists
// for is a dump killed by the 25-minute op cap, which leaves ctx already
// expired. Inheriting it would skip retention in exactly the case that needs it.
// Errors are logged, never surfaced: the run has already failed for its own
// reason and overwriting that detail with a retention error would hide it.
func (r *Runner) boundWALRetention(ctx context.Context, cred Credential, spec opSpec) {
	if spec.KeepDaily <= 0 {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), walRetentionTimeout)
	defer cancel()
	// prune=true: this is the only retention this run will do, and forgetting
	// alone merely unreferences the bundles — the bytes stay billed until pruned.
	if err := resticForgetWAL(rctx, cred, spec.KeepDaily, true); err != nil {
		r.log.Warn("wal retention failed after a failed backup run",
			"run", spec.RunID, "resource", spec.ResourceID, "err", err)
	}
}

// shortSha caps a digest for error messages. A raw `s[:12]` slice-panics if the
// CP ever sends a non-empty digest shorter than 12 chars, and there is no
// recover() anywhere in the agent — one malformed field would take the process
// down (SIGMA-78).
func shortSha(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
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
	// Unblock the dump goroutine if restic exited early: os/exec won't close pr,
	// so a pending pw.Write would hang the handler (and the DSD apply loop)
	// forever on <-execDone (SIGMA-69). ctx cancellation can't interrupt a
	// blocked io.Pipe write; closing pr can.
	_ = pr.Close()
	dumpErr := <-execDone
	// From here the repo is open and usable, so every exit must still bound WAL
	// retention — it is the WAL shipper's growth we are capping, and that shipper
	// does not care whether tonight's dump worked (SIGMA-283).
	if dumpErr != nil {
		r.boundWALRetention(ctx, cred, spec)
		return r.fail(ctx, spec.RunID, dumpErr)
	}
	if backupErr != nil {
		r.boundWALRetention(ctx, cred, spec)
		return r.fail(ctx, spec.RunID, backupErr)
	}
	if err := resticCheck(ctx, cred); err != nil {
		r.boundWALRetention(ctx, cred, spec)
		return r.fail(ctx, spec.RunID, fmt.Errorf("post-backup check: %w", err))
	}
	if spec.KeepDaily > 0 {
		// Forget out-of-window WAL bundles first (no prune) so the single prune in
		// resticForget reclaims their data too — the repo-wide forget can never
		// prune WAL on its own because each bundle has a unique path (SIGMA-108).
		if err := resticForgetWAL(ctx, cred, spec.KeepDaily, false); err != nil {
			sha := hex.EncodeToString(hasher.Sum(nil))
			r.report(ctx, spec.RunID, true, snapshotID, sha, "backup ok; WAL retention failed: "+err.Error())
			return nil
		}
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
	// See opBackupRun: close pr so an early restic exit can't deadlock the
	// basebackup goroutine on <-execDone (SIGMA-69).
	_ = pr.Close()
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

// dumpToWorkFile streams a snapshot's dump to a 0600 host work file, returning
// its path, sha256 and size. Verify/restore need a seekable file to tar into the
// target container; it is removed by the caller's cleanup.
// (The backup path itself never lands on host disk — this is restore-side
// staging, wiped immediately after the load.)
//
// snapshotID selects WHICH snapshot: a restore is pinned to the one the control
// plane recorded the digest of (SIGMA-245), so "the newest snapshot in the repo"
// and "the snapshot this run promised to load" cannot drift apart. Empty means
// `latest`, which is what verify wants and what pre-pin runs carry.
func (r *Runner) dumpToWorkFile(ctx context.Context, cred Credential, runID, engine, snapshotID string) (path, sha string, size int64, err error) {
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
	err = resticDumpSnapshot(ctx, cred, snapshotID, dumpFilename(engine), io.MultiWriter(f, hasher))
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
	// A verify checks the newest dump in the repo — that is what it is for.
	dumpPath, sha, size, err := r.dumpToWorkFile(ctx, cred, spec.RunID, spec.Engine, "")
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	// Byte-level half. When a digest was recorded, a mismatch is a hard failure.
	// When none was recorded (SIGMA-109) — the first-day case where verify is
	// rendered before the backup's dump_sha256 has landed, or a transient window
	// before the async sha report — we cannot compare, so we run the serving half
	// but must NOT claim the checksum passed; the detail says so plainly rather
	// than mislabelling an unverified run "checksum ok".
	checksumDetail := "checksum ok"
	if spec.ExpectedSha == "" {
		checksumDetail = "checksum NOT verified (no recorded digest)"
	} else if sha != spec.ExpectedSha {
		return r.fail(ctx, spec.RunID, fmt.Errorf("checksum mismatch: restored %s, recorded %s", shortSha(sha), shortSha(spec.ExpectedSha)))
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
	// Clear any leftover scratch container of this deterministic name (e.g. from a
	// prior run the agent restarted mid-op, whose deferred remove never ran). It
	// carries no managed label, so GC/reconcile never reap it; without this
	// ContainerCreate would 409 on the name collision and every retry would wedge
	// (SIGMA-124). Best-effort: a 404 (nothing there) is fine.
	_ = r.docker.ContainerRemove(ctx, name, true)
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
	detail := checksumDetail + "; scratch load ok"
	if probe != "" {
		detail += "; probe=" + probe
	}
	r.report(ctx, spec.RunID, true, "", sha, detail)
	r.log.Info("restore-verify passed", "run", spec.RunID, "resource", spec.ResourceID, "checksum", checksumDetail)
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
	// A restore loads the snapshot the CP pinned the run to, not whatever is
	// newest in the repo (SIGMA-245). Empty falls back to `latest` for runs
	// queued before the pin existed.
	dumpPath, sha, size, err := r.dumpToWorkFile(ctx, cred, spec.RunID, spec.Engine, spec.SnapshotID)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	// The CP pins the last successful backup's digest on the run — a restore
	// must never load bytes that don't match what was backed up. Unlike verify,
	// an EMPTY recorded digest is a hard failure here: loading an unverifiable
	// snapshot straight into the freshly provisioned target is exactly the
	// silent-gate-skip this must not allow (SIGMA-78).
	if spec.ExpectedSha == "" {
		return r.fail(ctx, spec.RunID, fmt.Errorf("restore refused: run carries no recorded checksum to verify the snapshot against"))
	}
	if sha != spec.ExpectedSha {
		return r.fail(ctx, spec.RunID, fmt.Errorf("checksum mismatch: restored %s, recorded %s", shortSha(sha), shortSha(spec.ExpectedSha)))
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
	// Report the snapshot back so the run's history keeps saying which one was
	// loaded — the result write overwrites the pin the CP recorded.
	r.report(ctx, spec.RunID, true, spec.SnapshotID, sha, detail)
	r.log.Info("restore complete", "run", spec.RunID, "target", spec.TargetContainer)
	return nil
}

// opBackupRestorePITR: point-in-time recovery (P2-5b, postgres only). Recover a
// throwaway container to the target time — untar the newest base backup taken
// before it, replay the archived WAL up to recovery_target_time, promote — then
// pg_dump the recovered state and load it into the freshly provisioned target
// resource. Recovery happens in a scratch container so the reconciler-managed
// target is never fought over; the target ends up with a logical copy of the
// point-in-time state (mirrors the fire-drill restore's load half).
func (r *Runner) opBackupRestorePITR(ctx context.Context, op dsd.Op) error {
	spec, err := parseSpec(op)
	if err != nil {
		return err
	}
	if spec.Engine != "postgres" {
		return fmt.Errorf("pitr restore is postgres-only, got %q", spec.Engine)
	}
	if spec.TargetContainer == "" {
		return fmt.Errorf("pitr restore missing target container")
	}
	targetTime, err := time.Parse(time.RFC3339, spec.TargetTime)
	if err != nil {
		return fmt.Errorf("pitr restore: invalid target time %q", spec.TargetTime)
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
	dir := filepath.Join(r.workDir, spec.RunID)
	defer os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return r.fail(ctx, spec.RunID, err)
	}

	// 1. The newest base backup taken at or before the target is the replay
	//    start point; WAL rolls forward from it to the target time.
	bases, err := resticSnapshotsByTag(ctx, cred, "base")
	if err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("list base snapshots: %w", err))
	}
	var base snapshotMeta
	for _, b := range bases {
		if !b.Time.After(targetTime) {
			base = b // sorted oldest-first: keep the latest ≤ target
		}
	}
	if base.ID == "" {
		return r.fail(ctx, spec.RunID, fmt.Errorf("no base backup on or before %s", targetTime.UTC().Format(time.RFC3339)))
	}

	// 2. Stage the base tar + the WAL bundles that carry the target. restic
	//    restore recovers each snapshot's file by id, so exact filenames aren't
	//    needed. WAL bundles up to and including the first one after the target
	//    hold every segment replay needs; earlier ones are harmless (postgres
	//    requests only the segments the base's checkpoint rolls forward through).
	if err := resticRestoreSnapshot(ctx, cred, base.ID, dir); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("restore base: %w", err))
	}
	basePath := filepath.Join(dir, "base.tar")
	if _, err := os.Stat(basePath); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("base backup missing after restore: %w", err))
	}
	wals, err := resticSnapshotsByTag(ctx, cred, "wal")
	if err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("list wal snapshots: %w", err))
	}
	walDir := filepath.Join(dir, "wal")
	if err := os.MkdirAll(walDir, 0o700); err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	var walSpansTarget bool
	for _, w := range wals {
		if err := resticRestoreSnapshot(ctx, cred, w.ID, walDir); err != nil {
			return r.fail(ctx, spec.RunID, fmt.Errorf("restore wal: %w", err))
		}
		if w.Time.After(targetTime) {
			walSpansTarget = true
			break // this bundle spans the target; no later WAL is needed
		}
	}
	// If no shipped WAL bundle was archived AFTER the target, the segments that
	// carry the state up to the target are still in the source's spool (not yet
	// shipped) or missing. Postgres would then promote at the last consistent
	// point it CAN reach — an earlier time — and we'd report the restore as a
	// success at a silently-earlier timestamp (SIGMA-77). Refuse instead of
	// delivering quiet data-currency loss on a DR operation. (The CP's SIGMA-67
	// window check reduces but can't eliminate this — the shipped high-water mark
	// can advance past a bundle that later isn't replayable.)
	if !walSpansTarget {
		newest := "none"
		if len(wals) > 0 {
			newest = wals[len(wals)-1].Time.UTC().Format(time.RFC3339)
		}
		return r.fail(ctx, spec.RunID, fmt.Errorf(
			"cannot recover to %s: no archived WAL shipped past the target (newest archived WAL is %s) — retry after the next WAL ship or choose an earlier target",
			targetTime.UTC().Format(time.RFC3339), newest))
	}
	walBundles, err := filepath.Glob(filepath.Join(walDir, "wal-*.tar"))
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}

	// 3. Recovery scratch container: base image, isolated, no ports. The
	//    entrypoint (derived from the engine, never the DSD) untars PGDATA +
	//    WAL, writes the recovery config, and starts postgres as the postgres
	//    user; postgres replays to the target time and promotes.
	recoveryCmd, err := pitrRecoveryScript(spec.Engine, targetTime.UTC().Format(time.RFC3339))
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	name := "sigmahub-pitr-" + spec.RunID
	body := map[string]any{
		"Image":      spec.Image,
		"Entrypoint": recoveryCmd,
		"Labels":     map[string]string{"sigmahub.pitr": "true"},
		"HostConfig": map[string]any{
			"NetworkMode":   "none",
			"SecurityOpt":   []string{"no-new-privileges:true", "apparmor=docker-default"},
			"Privileged":    false,
			"RestartPolicy": map[string]any{"Name": "no"},
		},
	}
	// Clear any leftover recovery container of this deterministic name (SIGMA-124),
	// so a mid-op restart can't wedge every retry on a 409 name collision.
	_ = r.docker.ContainerRemove(ctx, name, true)
	id, err := r.docker.ContainerCreate(ctx, name, body)
	if err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("create recovery container: %w", err))
	}
	defer func() {
		rmCtx, rmCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer rmCancel()
		if err := r.docker.ContainerRemove(rmCtx, id, true); err != nil {
			r.log.Warn("pitr recovery remove failed", "err", err, "container", name)
		}
	}()

	// Stage the tars into the created (not yet started) container's /tmp; the
	// entrypoint untars them on start.
	baseSt, err := os.Stat(basePath)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	if err := r.putFile(ctx, id, "/tmp", "base.tar", basePath, baseSt.Size()); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("stage base: %w", err))
	}
	for i, b := range walBundles {
		st, err := os.Stat(b)
		if err != nil {
			return r.fail(ctx, spec.RunID, err)
		}
		if err := r.putFile(ctx, id, "/tmp", fmt.Sprintf("wal-%03d.tar", i), b, st.Size()); err != nil {
			return r.fail(ctx, spec.RunID, fmt.Errorf("stage wal: %w", err))
		}
	}
	if err := r.docker.ContainerStart(ctx, id); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("start recovery container: %w", err))
	}
	// Recovery + promote can take a while; pg_isready only accepts connections
	// once the cluster has finished replaying and promoted.
	readyCmd, err := readyCommand(spec.Engine, spec.Database, spec.Username)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	if err := r.waitReady(ctx, id, readyCmd, 10*time.Minute); err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("recovery did not complete: %w", err))
	}

	// 4. Dump the recovered source database and load it into the fresh target.
	dumpCmd, err := dumpCommand(spec.Engine, spec.Database, spec.Username)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	dumpPath := filepath.Join(dir, dumpFilename(spec.Engine))
	sha, size, err := r.execDumpToFile(ctx, id, dumpCmd, dumpPath)
	if err != nil {
		return r.fail(ctx, spec.RunID, fmt.Errorf("dump recovered state: %w", err))
	}
	targetReady, err := readyCommand(spec.Engine, spec.TargetDatabase, spec.TargetUsername)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	if err := r.waitReady(ctx, spec.TargetContainer, targetReady, 120*time.Second); err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	probe, err := r.loadAndProbe(ctx, spec.TargetContainer, spec.Engine, spec.TargetDatabase, spec.TargetUsername, dumpPath, size)
	if err != nil {
		return r.fail(ctx, spec.RunID, err)
	}
	detail := "recovered to " + targetTime.UTC().Format(time.RFC3339) + "; loaded into " + spec.TargetContainer
	if probe != "" {
		detail += "; probe=" + probe
	}
	r.report(ctx, spec.RunID, true, "", sha, detail)
	r.log.Info("pitr restore complete", "run", spec.RunID, "target", spec.TargetContainer, "time", spec.TargetTime)
	return nil
}

// execDumpToFile runs an engine dump inside a container, streaming stdout to a
// 0600 host work file, and returns the dump's sha256 + size.
func (r *Runner) execDumpToFile(ctx context.Context, container string, cmd []string, path string) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	code, stderrTail, err := r.docker.ContainerExec(ctx, container, cmd, io.MultiWriter(f, hasher))
	cerr := f.Close()
	if err != nil {
		return "", 0, fmt.Errorf("exec dump: %w", err)
	}
	if code != 0 {
		return "", 0, fmt.Errorf("dump exited %d: %s", code, stderrTail)
	}
	if cerr != nil {
		return "", 0, cerr
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), st.Size(), nil
}
