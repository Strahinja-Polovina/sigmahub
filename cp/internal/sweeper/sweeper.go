// Package sweeper runs periodic control-plane maintenance: flipping servers
// that stopped heartbeating to unreachable and pruning old metrics.
package sweeper

import (
	"context"
	"log/slog"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

type Store interface {
	MarkStaleUnreachable(ctx context.Context, threshold time.Duration) (int64, error)
	PruneMetrics(ctx context.Context, retention time.Duration) (int64, error)
	TimeoutStaleDeployments(ctx context.Context, timeout time.Duration) (int64, error)
	TimeoutStaleDecommissions(ctx context.Context, timeout time.Duration) ([]store.DecommissionTimedOut, error)
}

type Config struct {
	// Interval between sweeps.
	Interval time.Duration
	// StaleAfter marks a running server unreachable once it hasn't been seen
	// for this long (≈ 3× the agent heartbeat interval).
	StaleAfter time.Duration
	// Retention keeps this much metric history.
	Retention time.Duration
	// DeployTimeout fails a deployment that has been in flight this long without
	// reaching a terminal state. Without it a deploy whose agent dies mid-flight
	// stays "building" forever: nothing else ever transitions it, so no
	// deploy_failed alert fires and the log pane streams indefinitely
	// (SIGMA-182). Backup runs have had this safety net since P1-11; deployments
	// did not. Keep it comfortably above the agent's own op timeouts.
	DeployTimeout time.Duration
	// DecommissionTimeout completes a graceful decommission the agent never
	// acked (SIGMA-204). A host powered off between the operator pressing
	// Disconnect and the agent picking the op up would otherwise hold its row
	// forever: 'decommissioning' is not 'running', so the staleness sweep never
	// touches it and nothing else ever transitions it. Generous — the teardown
	// stops containers with a grace period and the agent may be mid-long-poll —
	// but bounded, because the operator is watching.
	DecommissionTimeout time.Duration
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
			if cfg.DeployTimeout > 0 {
				if n, err := st.TimeoutStaleDeployments(ctx, cfg.DeployTimeout); err != nil {
					log.Error("sweeper: timeout stale deployments", "err", err)
				} else if n > 0 {
					log.Info("sweeper: deployments timed out", "count", n)
				}
			}
			if cfg.DecommissionTimeout > 0 {
				if timedOut, err := st.TimeoutStaleDecommissions(ctx, cfg.DecommissionTimeout); err != nil {
					log.Error("sweeper: timeout stale decommissions", "err", err)
				} else {
					for _, d := range timedOut {
						// Per-server, at warn: the machine still has our binary,
						// unit, tunnel and containers on it. This line is no
						// longer the only thing that says so — the store enqueues
						// a decommission_timed_out alert in the same transaction
						// as the tombstone (SIGMA-233), because stdout on the
						// control plane is shipped nowhere and this is the one
						// ending of a disconnect that leaves a live host behind
						// without anybody choosing it. The actor is included so
						// the log agrees with the notification the person who
						// pressed Disconnect just received.
						log.Warn("sweeper: decommission timed out; server removed without agent confirmation",
							"server", d.ServerID, "org", d.OrgID, "name", d.Name, "actor", d.Actor)
					}
				}
			}
		}
	}
}
