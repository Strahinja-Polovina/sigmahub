package store

// SIGMA-249: retention for the control plane's append-only growth tables.
//
// Before this, the only time-based DELETE anywhere in the store was
// PruneMetrics on server_metrics. Everything else grew forever:
//
//   - deploy_logs takes one row per streamed build-log line per deployment. A
//     200-server install doing 30 deploys a day at ~2,500 lines each writes
//     75,000 rows a day — roughly 27 million rows and tens of gigabytes of TEXT
//     a year, on a table nothing reads beyond the last 500 lines of a build
//     that is still running.
//   - cp_audit_log takes a row per DSD version issued, so the 60s fleet resync
//     alone writes 200 rows a minute whenever documents churn, plus one per KMS
//     unwrap.
//   - deploy_requests, webhook_deliveries, alert_outbox and idempotency_keys
//     are all append-only with no cleanup at all.
//
// The failure is not a slow page. Autovacuum falls behind on tables nobody
// queries, the compose host's single disk fills, Postgres goes read-only, and
// the control plane fails for every tenant at once.
//
// Two rules run through all of it:
//
//  1. Never delete outstanding WORK. A pending alert_outbox row is an alert
//     nobody has received; a queued deploy_request is a push nobody has
//     deployed. Both look exactly like old history by age and are the opposite
//     of it, so both are filtered on status rather than on age alone.
//  2. Delete in bounded batches. A first sweep after this ships may face a
//     year of rows, and one unbounded DELETE would hold locks and bloat WAL for
//     as long as it takes. Each table is capped per pass; the sweeper runs
//     every 30 seconds and catches up over a few minutes.

import (
	"context"
	"fmt"
	"time"
)

// pruneBatch caps how many rows one pass deletes from one table. Sized so a
// single statement stays short even on a busy host, while 30-second sweeps
// still retire ~10 million rows a day — comfortably faster than any install
// can produce them.
const pruneBatch = 5000

// Retention is the per-table history budget. A zero duration DISABLES that
// table's sweep: retention that deletes a customer's audit trail is not
// something to arrive at by forgetting to set a field, so every table is an
// explicit choice by the operator wiring the sweeper up. cp/cmd/sigmahub-cp
// states the product defaults and why each one is what it is.
type Retention struct {
	// DeployLogs keeps a FINISHED deployment's build-log lines this long after
	// it finished. Measured on the deployment, not on the line: a build that has
	// been streaming for an hour is still being watched, and its first lines are
	// exactly the ones that explain what went wrong.
	DeployLogs time.Duration
	// Audit keeps cp_audit_log rows. The one entry here with a compliance
	// dimension — it is what answers "who changed this, and when" — so it wants
	// a deliberately long, explicitly chosen value rather than one arrived at by
	// omission.
	Audit time.Duration
	// DeployRequests keeps DRAINED git deploy requests. Queued ones are never
	// touched: the drain has not run them yet.
	DeployRequests time.Duration
	// WebhookDeliveries keeps the provider delivery ids that make a redelivered
	// webhook a no-op. Only useful for as long as a provider might retry, which
	// is hours, so this is generous at days.
	WebhookDeliveries time.Duration
	// AlertOutbox keeps FINALIZED deliveries (sent or permanently failed).
	// Pending and in-flight rows are never touched at any age — that row is an
	// alert nobody has received yet, and deleting it loses the notification
	// silently, which is the failure the outbox exists to prevent.
	AlertOutbox time.Duration
	// IdempotencyKeys keeps stored responses so a client retry replays instead
	// of re-executing. Retries happen in seconds; days is already far past the
	// point where a replay could still be the same request.
	IdempotencyKeys time.Duration
}

// Any reports whether any table has a retention configured, so a caller can
// skip the sweep entirely rather than issue six no-op statements.
func (r Retention) Any() bool {
	return r.DeployLogs > 0 || r.Audit > 0 || r.DeployRequests > 0 ||
		r.WebhookDeliveries > 0 || r.AlertOutbox > 0 || r.IdempotencyKeys > 0
}

// PruneRetained deletes one bounded batch from each configured table and
// returns the rows removed per table (keyed by table name, absent when that
// table was disabled or had nothing to delete).
//
// Each statement is independent and self-committing: a failure on one table
// must not roll back the others, because the point of the sweep is to keep a
// disk from filling and partial progress is progress.
func (s *Store) PruneRetained(ctx context.Context, r Retention) (map[string]int64, error) {
	out := map[string]int64{}
	var firstErr error
	prune := func(table string, retain time.Duration, sql string) {
		if retain <= 0 {
			return
		}
		tag, err := s.Pool.Exec(ctx, sql, retain.Seconds(), pruneBatch)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("prune %s: %w", table, err)
			}
			return
		}
		if n := tag.RowsAffected(); n > 0 {
			out[table] = n
		}
	}

	// deploy_logs is joined back to its deployment: retention is a property of
	// the deployment's life, and the line's own `at` is only when that line was
	// streamed. ctid-batching is what keeps the statement short — there is no
	// index that orders this predicate, so a plain LIMIT-less DELETE would scan
	// and lock the whole table on the first sweep after an upgrade.
	prune("deploy_logs", r.DeployLogs, `
		DELETE FROM deploy_logs
		 WHERE ctid IN (
		     SELECT l.ctid
		       FROM deploy_logs l
		       JOIN deployments d ON d.id = l.deployment_id
		      WHERE d.status NOT IN ('queued','building','deploying')
		        AND COALESCE(d.finished_at, d.created_at) < now() - make_interval(secs => $1)
		      LIMIT $2)`)

	prune("cp_audit_log", r.Audit, `
		DELETE FROM cp_audit_log
		 WHERE ctid IN (
		     SELECT ctid FROM cp_audit_log
		      WHERE created_at < now() - make_interval(secs => $1)
		      LIMIT $2)`)

	// Status filter, not just age: a request still 'queued' is a push the drain
	// has not turned into a deployment yet. Deleting it would drop somebody's
	// deploy on the floor with no trace.
	prune("deploy_requests", r.DeployRequests, `
		DELETE FROM deploy_requests
		 WHERE ctid IN (
		     SELECT ctid FROM deploy_requests
		      WHERE status <> 'queued'
		        AND created_at < now() - make_interval(secs => $1)
		      LIMIT $2)`)

	prune("webhook_deliveries", r.WebhookDeliveries, `
		DELETE FROM webhook_deliveries
		 WHERE ctid IN (
		     SELECT ctid FROM webhook_deliveries
		      WHERE received_at < now() - make_interval(secs => $1)
		      LIMIT $2)`)

	// Only finalized deliveries. 'pending' and 'sending' rows are undelivered
	// alerts — including ones a crashed dispatcher will reclaim — and an old one
	// is a symptom to investigate, never garbage to collect.
	prune("alert_outbox", r.AlertOutbox, `
		DELETE FROM alert_outbox
		 WHERE ctid IN (
		     SELECT ctid FROM alert_outbox
		      WHERE status IN ('sent','failed')
		        AND created_at < now() - make_interval(secs => $1)
		      LIMIT $2)`)

	prune("idempotency_keys", r.IdempotencyKeys, `
		DELETE FROM idempotency_keys
		 WHERE ctid IN (
		     SELECT ctid FROM idempotency_keys
		      WHERE created_at < now() - make_interval(secs => $1)
		      LIMIT $2)`)

	return out, firstErr
}
