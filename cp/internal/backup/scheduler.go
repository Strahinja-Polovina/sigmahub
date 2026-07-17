// Package backup owns the P1-11 scheduling primitive: the reconciler is
// level-triggered spec sync with no notion of time, so this scheduler is the
// component that turns backup policies into due backup/verify runs (typed DSD
// ops) on a wall-clock cadence, and fails runs that stopped making progress.
package backup

import (
	"context"
	"log/slog"
	"time"
)

// Store is the slice of the persistence layer the scheduler needs.
type Store interface {
	CreateDueBackupRuns(ctx context.Context, now time.Time) ([]struct{ ServerID, OrgID string }, error)
	TimeoutStaleBackupRuns(ctx context.Context, maxAge time.Duration) (int, error)
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
	// RunTimeout fails a run stuck pending/running (crashed agent, lost
	// report) so the day honestly reads not-green and tomorrow's run enqueues.
	RunTimeout time.Duration
}

// Run sweeps until ctx is cancelled. Blocks; run in a goroutine.
func Run(ctx context.Context, log *slog.Logger, st Store, rec Reconciler, cfg Config) {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.RunTimeout <= 0 {
		cfg.RunTimeout = 30 * time.Minute
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			servers, err := st.CreateDueBackupRuns(ctx, time.Now())
			if err != nil {
				log.Error("backup scheduler: enqueue", "err", err)
			}
			for _, sv := range servers {
				rec.ReconcileAsync(sv.OrgID, sv.ServerID)
			}
			if n, err := st.TimeoutStaleBackupRuns(ctx, cfg.RunTimeout); err != nil {
				log.Error("backup scheduler: timeout sweep", "err", err)
			} else if n > 0 {
				log.Warn("backup scheduler: timed out stale runs", "count", n)
			}
		}
	}
}
