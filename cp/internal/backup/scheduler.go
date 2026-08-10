// Package backup owns the P1-11 scheduling primitive: the reconciler is
// level-triggered spec sync with no notion of time, so this scheduler is the
// component that turns backup policies into due backup/verify runs (typed DSD
// ops) on a wall-clock cadence, and fails runs that stopped making progress.
package backup

import (
	"context"
	"log/slog"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/supervise"
)

// Store is the slice of the persistence layer the scheduler needs.
type Store interface {
	CreateDueBackupRuns(ctx context.Context, now time.Time) ([]struct{ ServerID, OrgID string }, error)
	TimeoutStaleBackupRuns(ctx context.Context, execMaxAge, queueMaxAge time.Duration) (int, error)
}

// Reconciler is nudged for every server that received new runs.
type Reconciler interface {
	ReconcileAsync(orgID, serverID string)
}

// Config tunes the scheduler loop.
type Config struct {
	// Interval between due-work sweeps. The schedule granularity is daily, so
	// a minute-level sweep is more than enough resolution.
	Interval time.Duration
	// RunTimeout fails a RUNNING run whose agent stopped reporting, measured
	// from dispatch (started_at). Sized just above the agent's 25-minute op cap.
	RunTimeout time.Duration
	// QueueTimeout fails a PENDING run that was never dispatched. Queue time is
	// unbounded by design — verify rows wait for their backup's sha and the
	// agent applies ops serially (SIGMA-163) — so this is deliberately generous.
	QueueTimeout time.Duration
	// Heartbeat, when set, is called once per sweep with that sweep's outcome
	// (SIGMA-248). This loop is the one that most needs it: when
	// CreateDueBackupRuns starts erroring, no run is created for any tenant, and
	// `backup_failed` cannot fire because it needs a run to exist before it can
	// fail. Backups go off fleet-wide and the only trace is a log line. nil is
	// fine — the loop behaves identically without it.
	Heartbeat func(error)
}

// Run sweeps until ctx is cancelled. Blocks; run in a goroutine.
func Run(ctx context.Context, log *slog.Logger, st Store, rec Reconciler, cfg Config) {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 30 * time.Minute
	}
	if cfg.QueueTimeout <= 0 {
		cfg.QueueTimeout = 6 * time.Hour
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Supervised (SIGMA-250): a panic in one sweep abandons that sweep
			// instead of terminating the control plane for every tenant.
			err := supervise.Pass(log, "backup_scheduler", func() error {
				return sweep(ctx, log, st, rec, cfg)
			})
			if cfg.Heartbeat != nil {
				cfg.Heartbeat(err)
			}
		}
	}
}

// sweep is one pass. The returned error is the FIRST failure of the pass, so
// the heartbeat reports a half-done sweep as failed: a tick that timed out
// stale runs but enqueued none of the due ones has not scheduled anybody's
// backups.
func sweep(ctx context.Context, log *slog.Logger, st Store, rec Reconciler, cfg Config) error {
	var passErr error
	servers, err := st.CreateDueBackupRuns(ctx, time.Now())
	if err != nil {
		log.Error("backup scheduler: enqueue", "err", err)
		passErr = err
	}
	for _, sv := range servers {
		rec.ReconcileAsync(sv.OrgID, sv.ServerID)
	}
	if n, err := st.TimeoutStaleBackupRuns(ctx, cfg.RunTimeout, cfg.QueueTimeout); err != nil {
		log.Error("backup scheduler: timeout sweep", "err", err)
		if passErr == nil {
			passErr = err
		}
	} else if n > 0 {
		log.Warn("backup scheduler: timed out stale runs", "count", n)
	}
	return passErr
}
