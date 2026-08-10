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
	// PruneRetained retires one bounded batch from each append-only growth table
	// (SIGMA-249). Returns rows deleted per table.
	PruneRetained(ctx context.Context, r store.Retention) (map[string]int64, error)
}

type Config struct {
	// Interval between sweeps.
	Interval time.Duration
	// StaleAfter marks a running server unreachable once it hasn't been seen
	// for this long (≈ 3× the agent heartbeat interval).
	StaleAfter time.Duration
	// Retention keeps this much metric history (server_metrics only — the one
	// table that had a retention sweep before SIGMA-249).
	Retention time.Duration
	// Retain is the per-table history budget for the append-only growth tables:
	// deploy_logs, cp_audit_log, deploy_requests, webhook_deliveries,
	// alert_outbox and idempotency_keys. Zero fields disable their table, so a
	// caller that sets nothing keeps the pre-SIGMA-249 behaviour of never
	// deleting any of it. store/retention.go argues each table's rule.
	Retain store.Retention
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
	// Heartbeat, when set, is called once per sweep with that sweep's outcome
	// (SIGMA-248). It is what makes "the sweeper is running" distinguishable
	// from "the sweeper is erroring on every tick": the loop's only previous
	// reaction to a failure was a log line on a stdout nothing ships anywhere.
	// nil is fine — the loop behaves identically without it.
	Heartbeat func(error)
}

// beat reports a pass's outcome when a heartbeat is configured.
func (c Config) beat(err error) {
	if c.Heartbeat != nil {
		c.Heartbeat(err)
	}
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
			// passErr is the sweep's verdict: the FIRST failure of any step, kept
			// so the heartbeat below reports a partial sweep as a failure. A pass
			// that pruned metrics but could not flip stale servers has not done
			// its job, and reporting success for it would put a fresh timestamp on
			// a loop that is half broken — precisely the reading this is meant to
			// prevent.
			var passErr error
			fail := func(err error) {
				if passErr == nil {
					passErr = err
				}
			}
			if n, err := st.MarkStaleUnreachable(ctx, cfg.StaleAfter); err != nil {
				log.Error("sweeper: mark unreachable", "err", err)
				fail(err)
			} else if n > 0 {
				log.Info("sweeper: servers marked unreachable", "count", n)
			}
			if n, err := st.PruneMetrics(ctx, cfg.Retention); err != nil {
				log.Error("sweeper: prune metrics", "err", err)
				fail(err)
			} else if n > 0 {
				log.Info("sweeper: metrics pruned", "count", n)
			}
			if cfg.DeployTimeout > 0 {
				if n, err := st.TimeoutStaleDeployments(ctx, cfg.DeployTimeout); err != nil {
					log.Error("sweeper: timeout stale deployments", "err", err)
					fail(err)
				} else if n > 0 {
					log.Info("sweeper: deployments timed out", "count", n)
				}
			}
			if cfg.DecommissionTimeout > 0 {
				if timedOut, err := st.TimeoutStaleDecommissions(ctx, cfg.DecommissionTimeout); err != nil {
					log.Error("sweeper: timeout stale decommissions", "err", err)
					fail(err)
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
			// Retention for the append-only growth tables (SIGMA-249). Last in the
			// pass, because everything above it is fleet HEALTH and this is
			// housekeeping: a sweep that is short on time should flip stale servers
			// before it retires old log lines.
			if cfg.Retain.Any() {
				if deleted, err := st.PruneRetained(ctx, cfg.Retain); err != nil {
					log.Error("sweeper: prune retained", "err", err)
					fail(err)
				} else {
					for table, n := range deleted {
						log.Info("sweeper: rows pruned", "table", table, "count", n)
					}
				}
			}
			cfg.beat(passErr)
		}
	}
}
