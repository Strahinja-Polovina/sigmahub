package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// resticBin is resolved via PATH; a var so tests can point it at a stub.
var resticBin = "restic"

// Credential is the per-run restic material fetched from the control plane.
type Credential struct {
	Repository string
	RepoKey    string
	AccessKey  string
	SecretKey  string
	Region     string
}

// env renders the restic process environment. Credentials ride ONLY in the
// child process env — never argv, never disk.
func (c Credential) env() []string {
	env := []string{
		"RESTIC_REPOSITORY=" + c.Repository,
		"RESTIC_PASSWORD=" + c.RepoKey,
		"AWS_ACCESS_KEY_ID=" + c.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + c.SecretKey,
		// restic shells out to nothing, but a minimal PATH keeps exec resolvable.
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if c.Region != "" {
		env = append(env, "AWS_DEFAULT_REGION="+c.Region)
	}
	return env
}

// restic runs one restic command with stdin/stdout wiring and returns stderr
// (capped) for diagnostics.
func restic(ctx context.Context, cred Credential, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, resticBin, args...)
	cmd.Env = cred.env()
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &limitedBuffer{buf: &stderr, max: 4096}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("restic %s: %s", args[0], msg)
	}
	return nil
}

type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.buf.Len() < l.max {
		room := l.max - l.buf.Len()
		if len(p) > room {
			l.buf.Write(p[:room])
		} else {
			l.buf.Write(p)
		}
	}
	return len(p), nil
}

// resticInit initialises the repository, tolerating one that already exists
// (init is the cheapest existence probe that needs no list permissions).
func resticInit(ctx context.Context, cred Credential) error {
	err := restic(ctx, cred, nil, io.Discard, "init")
	if err != nil && (strings.Contains(err.Error(), "already initialized") ||
		strings.Contains(err.Error(), "already exists")) {
		return nil
	}
	return err
}

// resticBackupStdin streams a dump into the repository and returns the new
// snapshot id parsed from restic's JSON summary line.
func resticBackupStdin(ctx context.Context, cred Credential, dump io.Reader, filename string) (string, error) {
	return resticBackupStdinTagged(ctx, cred, dump, filename, "")
}

// resticBackupStdinTagged is resticBackupStdin with an optional restic tag, so
// base backups and WAL bundles (P2-5) land under distinguishable tags in the
// per-resource repo.
func resticBackupStdinTagged(ctx context.Context, cred Credential, dump io.Reader, filename, tag string) (string, error) {
	args := []string{"backup", "--stdin", "--stdin-filename", filename, "--json"}
	if tag != "" {
		args = append(args, "--tag", tag)
	}
	var out bytes.Buffer
	if err := restic(ctx, cred, dump, &out, args...); err != nil {
		return "", err
	}
	// The last JSON line with message_type "summary" carries snapshot_id.
	snapshot := ""
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal([]byte(line), &msg) == nil && msg.MessageType == "summary" && msg.SnapshotID != "" {
			snapshot = msg.SnapshotID
		}
	}
	if snapshot == "" {
		return "", fmt.Errorf("restic backup: no snapshot id in output")
	}
	return snapshot, nil
}

// resticCheck verifies repository structure integrity after a backup.
func resticCheck(ctx context.Context, cred Credential) error {
	return restic(ctx, cred, nil, io.Discard, "check")
}

// resticForget applies the GFS retention policy and prunes unreferenced data.
func resticForget(ctx context.Context, cred Credential, keepDaily, keepWeekly, keepMonthly int) error {
	args := []string{"forget", "--prune", "--keep-daily", fmt.Sprint(keepDaily)}
	if keepWeekly > 0 {
		args = append(args, "--keep-weekly", fmt.Sprint(keepWeekly))
	}
	if keepMonthly > 0 {
		args = append(args, "--keep-monthly", fmt.Sprint(keepMonthly))
	}
	return restic(ctx, cred, nil, io.Discard, args...)
}

// resticForgetWAL bounds WAL-bundle retention (SIGMA-108). Every WAL bundle is a
// stdin backup with a UNIQUE stored path (/wal-<ts>.tar), so the repo-wide
// resticForget — which groups by "host,paths" and keeps at least one snapshot
// per group — never forgets a single WAL snapshot, growing the repo without
// bound. Regrouping the "wal"-tagged snapshots by tag collapses them into one
// group, so --keep-within can drop WAL older than the window. The window is the
// base-backup keep-daily span, so any base still retained at daily granularity
// can still roll forward. No --prune here: the caller's resticForget prune
// reclaims the newly-unreferenced data in a single pass.
func resticForgetWAL(ctx context.Context, cred Credential, keepDays int) error {
	if keepDays <= 0 {
		return nil
	}
	return restic(ctx, cred, nil, io.Discard,
		"forget", "--tag", "wal", "--group-by", "tags",
		"--keep-within", fmt.Sprintf("%dd", keepDays))
}

// resticDumpSnapshot streams one LOGICAL-DUMP snapshot's dump file to w.
//
// snapshotID empty means `latest`. The `--path /<filename>` filter constrains
// `latest` to snapshots that carry the dump file: a PITR-enabled resource shares
// one repo across logical dumps, base backups (tag "base") and WAL bundles (tag
// "wal", shipped continuously), so an unfiltered `dump latest` would resolve to
// the newest snapshot — almost always a WAL/base bundle that has no dump file —
// and error out, breaking restore-verify and fire-drill restore for every PITR
// resource (SIGMA-83). stdin backups record the path as "/<stdin-filename>".
//
// A fire-drill restore names its snapshot explicitly (SIGMA-245): the control
// plane pinned the run to the snapshot whose digest it recorded, and `latest`
// is not that snapshot on any day where a newer dump landed in the repo after
// the pin. The filter stays harmless when an id is given.
func resticDumpSnapshot(ctx context.Context, cred Credential, snapshotID, filename string, w io.Writer) error {
	snap := snapshotID
	if snap == "" {
		snap = "latest"
	}
	return restic(ctx, cred, nil, w, "dump", "--path", "/"+filename, snap, filename)
}

// snapshotMeta is one restic snapshot's id + creation time (P2-5b PITR needs to
// order base/WAL snapshots against a recovery target).
type snapshotMeta struct {
	ID   string
	Time time.Time
}

// resticSnapshotsByTag lists the repo's snapshots carrying a tag, oldest first.
// Used to select the base backup ≤ the recovery target and the WAL bundles that
// roll forward to it.
func resticSnapshotsByTag(ctx context.Context, cred Credential, tag string) ([]snapshotMeta, error) {
	var out bytes.Buffer
	if err := restic(ctx, cred, nil, &out, "snapshots", "--tag", tag, "--json"); err != nil {
		return nil, err
	}
	var raw []struct {
		ID   string `json:"id"`
		Time string `json:"time"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	metas := make([]snapshotMeta, 0, len(raw))
	for _, s := range raw {
		if s.ID == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, s.Time)
		if err != nil {
			if t, err = time.Parse(time.RFC3339, s.Time); err != nil {
				continue
			}
		}
		metas = append(metas, snapshotMeta{ID: s.ID, Time: t})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Time.Before(metas[j].Time) })
	return metas, nil
}

// resticRestoreSnapshot restores one snapshot's file(s) into targetDir. Used for
// PITR staging: it recovers the stdin-backed base.tar / wal-*.tar without the
// caller needing the exact stored filename (restic restore writes them under
// targetDir at their in-snapshot path).
func resticRestoreSnapshot(ctx context.Context, cred Credential, snapshotID, targetDir string) error {
	return restic(ctx, cred, nil, io.Discard, "restore", snapshotID, "--target", targetDir)
}
