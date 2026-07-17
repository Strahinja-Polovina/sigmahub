package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
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
	var out bytes.Buffer
	if err := restic(ctx, cred, dump, &out,
		"backup", "--stdin", "--stdin-filename", filename, "--json"); err != nil {
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

// resticDumpLatest streams the latest snapshot's dump file to w.
func resticDumpLatest(ctx context.Context, cred Credential, filename string, w io.Writer) error {
	return restic(ctx, cred, nil, w, "dump", "latest", filename)
}
