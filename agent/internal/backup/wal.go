package backup

// P2-5 WAL shipper: continuous archiving into the per-resource restic repo.
// Postgres' archive_command drops completed segments into a spool volume
// inside the container; this loop periodically bundles the ready segments
// (tar via docker exec, streamed into restic under the "wal" tag) and, only
// after restic confirms the bundle, deletes exactly those segments. A segment
// is never deleted before it is durably in the repo, and a half-written
// segment is never shipped (the archive_command writes tmp+rename).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// WALSpoolDir is where postgres archives completed segments (must match the
// reconciler's archive_command mount).
const WALSpoolDir = "/var/lib/postgresql/wal-archive"

// walTimeFmt stamps the per-cycle bundle filename (injected in tests, since
// time.Now is otherwise nondeterministic).
const walBundlePrefix = "wal-"

// WALCredFetcher resolves a resource's restic credential for shipping.
type WALCredFetcher func(ctx context.Context, resourceID string) (Credential, error)

// WALTargetLister lists the resources this server should ship.
type WALTargetLister func(ctx context.Context) ([]string, error)

// WALReporter records a cycle's high-water mark (best-effort).
type WALReporter func(ctx context.Context, resourceID, lastSegment string)

// WALShipper drains spool volumes into restic on an interval.
type WALShipper struct {
	docker   Docker
	targets  WALTargetLister
	creds    WALCredFetcher
	report   WALReporter
	log      logger
	interval time.Duration
	// now is injectable for deterministic bundle names in tests.
	now func() time.Time
}

// logger is the tiny slog slice the shipper uses (keeps the file test-light).
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

// NewWALShipper builds a shipper. interval <= 0 defaults to 1 minute.
func NewWALShipper(docker Docker, targets WALTargetLister, creds WALCredFetcher, report WALReporter, log logger, interval time.Duration) *WALShipper {
	if interval <= 0 {
		interval = time.Minute
	}
	return &WALShipper{
		docker: docker, targets: targets, creds: creds, report: report,
		log: log, interval: interval, now: time.Now,
	}
}

// Run ships every target's spool each interval until ctx ends. One target's
// failure never blocks the others.
func (s *WALShipper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			targets, err := s.targets(ctx)
			if err != nil {
				s.log.Warn("wal: list targets", "err", err)
				continue
			}
			for _, resourceID := range targets {
				if err := s.shipOne(ctx, resourceID); err != nil {
					s.log.Warn("wal: ship failed", "resource", resourceID, "err", err)
				}
			}
		}
	}
}

// shipOne bundles a resource's ready segments into restic, then deletes the
// shipped ones. Ordering is what makes it safe: ship first, delete only what
// was confirmed shipped.
func (s *WALShipper) shipOne(ctx context.Context, resourceID string) error {
	// Matches dsd.ContainerName on the CP side (sigmahub-<resourceID>).
	container := "sigmahub-" + resourceID
	segments, err := s.listSegments(ctx, container)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return nil // nothing ready this cycle
	}

	cred, err := s.creds(ctx, resourceID)
	if err != nil {
		return fmt.Errorf("fetch wal credential: %w", err)
	}
	if err := resticInit(ctx, cred); err != nil {
		return fmt.Errorf("init repository: %w", err)
	}

	// tar exactly the segments we listed (paths relative to the spool dir), so
	// segments that complete mid-cycle aren't shipped-then-orphaned.
	tarCmd := append([]string{"tar", "-C", WALSpoolDir, "-cf", "-"}, segments...)
	pr, pw := io.Pipe()
	execDone := make(chan error, 1)
	go func() {
		exitCode, stderrTail, execErr := s.docker.ContainerExec(ctx, container, tarCmd, pw)
		switch {
		case execErr != nil:
			execErr = fmt.Errorf("tar wal exec: %w", execErr)
		case exitCode != 0:
			execErr = fmt.Errorf("tar wal exited %d: %s", exitCode, stderrTail)
		}
		pw.CloseWithError(execErr)
		execDone <- execErr
	}()

	bundle := walBundlePrefix + s.now().UTC().Format("20060102T150405Z") + ".tar"
	_, backupErr := resticBackupStdinTagged(ctx, cred, pr, bundle, "wal")
	// Close the read end as soon as restic returns. If restic exited early with
	// an error, os/exec does NOT close pr, so the tar goroutine would block
	// forever on pw.Write and <-execDone would deadlock the whole shipper
	// (SIGMA-69). Closing pr makes the pending write return ErrClosedPipe.
	_ = pr.Close()
	tarErr := <-execDone
	if tarErr != nil {
		return tarErr
	}
	if backupErr != nil {
		return backupErr
	}

	// Shipped and durable — now delete exactly those segments.
	rmCmd := append([]string{"rm", "-f"}, prefixPaths(WALSpoolDir, segments)...)
	if exitCode, stderrTail, err := s.docker.ContainerExec(ctx, container, rmCmd, io.Discard); err != nil {
		s.log.Warn("wal: cleanup exec", "resource", resourceID, "err", err)
	} else if exitCode != 0 {
		s.log.Warn("wal: cleanup nonzero", "resource", resourceID, "detail", stderrTail)
	}

	last := segments[len(segments)-1]
	if s.report != nil {
		s.report(ctx, resourceID, last)
	}
	s.log.Info("wal shipped", "resource", resourceID, "segments", len(segments), "through", last)
	return nil
}

// listSegments lists ready WAL segment filenames (sorted), excluding any
// leftover .tmp files the archive_command is still writing.
func (s *WALShipper) listSegments(ctx context.Context, container string) ([]string, error) {
	var out bytes.Buffer
	// -1 one-per-line; missing dir (PITR just enabled, none archived yet) is
	// not an error — treat a nonzero exit as "nothing to ship".
	exitCode, _, err := s.docker.ContainerExec(ctx, container,
		[]string{"sh", "-c", "ls -1 " + WALSpoolDir + " 2>/dev/null || true"}, &out)
	if err != nil {
		return nil, fmt.Errorf("list wal exec: %w", err)
	}
	if exitCode != 0 {
		return nil, nil
	}
	var segs []string
	for _, line := range strings.Split(out.String(), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.HasSuffix(name, ".tmp") {
			continue
		}
		segs = append(segs, name)
	}
	sort.Strings(segs)
	return segs, nil
}

func prefixPaths(dir string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = path.Join(dir, n)
	}
	return out
}
