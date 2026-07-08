// Package sweeper runs periodic control-plane maintenance: flipping servers
// that stopped heartbeating to unreachable and pruning old metrics.
package sweeper

import (
	"context"
	"log/slog"
	"time"
)

type Store interface {
	MarkStaleUnreachable(ctx context.Context, threshold time.Duration) (int64, error)
	PruneMetrics(ctx context.Context, retention time.Duration) (int64, error)
}

type Config struct {
	// Interval between sweeps.
	Interval time.Duration
	// StaleAfter marks a running server unreachable once it hasn't been seen
	// for this long (≈ 3× the agent heartbeat interval).
	StaleAfter time.Duration
	// Retention keeps this much metric history.
	Retention time.Duration
}

// Run sweeps until ctx is cancelled. Blocks; run it in a goroutine.
func Run(ctx context.Context, log *slog.Logger, st Store, cfg Config) {
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := st.MarkStaleUnreachable(ctx, cfg.StaleAfter); err != nil {
				log.Error("sweeper: mark unreachable", "err", err)
			} else if n > 0 {
				log.Info("sweeper: servers marked unreachable", "count", n)
			}
			if n, err := st.PruneMetrics(ctx, cfg.Retention); err != nil {
				log.Error("sweeper: prune metrics", "err", err)
			} else if n > 0 {
				log.Info("sweeper: metrics pruned", "count", n)
			}
		}
	}
}
