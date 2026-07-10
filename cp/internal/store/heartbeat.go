package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MetricSample is one resource-usage reading from an agent heartbeat.
type MetricSample struct {
	CPUPct  float64 `json:"cpuPct"`
	MemPct  float64 `json:"memPct"`
	DiskPct float64 `json:"diskPct"`
	Load1   float64 `json:"load1"`
}

// MetricPoint is a stored sample with its timestamp, for the dashboard.
type MetricPoint struct {
	MetricSample
	RecordedAt time.Time `json:"recordedAt"`
}

// HeartbeatInput is one agent check-in.
type HeartbeatInput struct {
	AgentVersion string
	Facts        json.RawMessage
	Metrics      *MetricSample
}

// RecordHeartbeat updates a server's liveness and appends a metrics sample.
// First heartbeat (or one after a stale gap) flips provisioning/unreachable →
// running; the sweeper handles running → unreachable on missed heartbeats.
// Status flips are audited.
func (s *Store) RecordHeartbeat(ctx context.Context, serverID string, in HeartbeatInput) error {
	facts := normalizeFacts(in.Facts)

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The self-join FROM clause exposes the pre-update row, so one statement
	// both applies the update and reports whether the status flipped.
	var orgID, name, prevStatus, status string
	err = tx.QueryRow(ctx, `
		UPDATE servers s
		   SET last_seen_at = now(),
		       agent_version = COALESCE(NULLIF($2, ''), s.agent_version),
		       facts = $3,
		       status = CASE WHEN s.status IN ('provisioning', 'unreachable') THEN 'running' ELSE s.status END
		  FROM servers old
		 WHERE s.id = $1 AND old.id = s.id
		 RETURNING s.org_id, s.name, old.status, s.status`,
		serverID, in.AgentVersion, facts).Scan(&orgID, &name, &prevStatus, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("record heartbeat: %w", err)
	}
	if prevStatus != status {
		if _, err := tx.Exec(ctx, `
			INSERT INTO cp_audit_log (org_id, actor, action, target)
			VALUES ($1, 'sigmad', 'Server running', $2)`, orgID, name); err != nil {
			return fmt.Errorf("audit: %w", err)
		}
	}

	if in.Metrics != nil {
		m := in.Metrics
		if _, err := tx.Exec(ctx, `
			INSERT INTO server_metrics (server_id, cpu_pct, mem_pct, disk_pct, load1)
			VALUES ($1, $2, $3, $4, $5)`,
			serverID, m.CPUPct, m.MemPct, m.DiskPct, m.Load1); err != nil {
			return fmt.Errorf("insert metric: %w", err)
		}
	}
	return tx.Commit(ctx)
}

// MetricsSince returns an org-scoped server's samples newer than `since`, oldest
// first. The org join prevents reading another org's metrics by server id.
func (s *Store) MetricsSince(ctx context.Context, orgID, serverID string, since time.Time) ([]MetricPoint, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT m.cpu_pct, m.mem_pct, m.disk_pct, m.load1, m.recorded_at
		  FROM server_metrics m
		  JOIN servers s ON s.id = m.server_id
		 WHERE s.org_id = $1 AND m.server_id = $2 AND m.recorded_at >= $3
		 ORDER BY m.recorded_at`,
		orgID, serverID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []MetricPoint{}
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.CPUPct, &p.MemPct, &p.DiskPct, &p.Load1, &p.RecordedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkStaleUnreachable flips running servers with no heartbeat within the
// threshold to unreachable, writing one audit row per flipped server. Returns
// how many changed. The cutoff is computed in SQL (now() - interval) so it
// stays in the DB clock domain that wrote last_seen_at — CP↔DB clock skew
// must not make live servers flap.
func (s *Store) MarkStaleUnreachable(ctx context.Context, threshold time.Duration) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `
		WITH flipped AS (
			UPDATE servers
			   SET status = 'unreachable'
			 WHERE status = 'running'
			   AND (last_seen_at IS NULL OR last_seen_at < now() - make_interval(secs => $1))
			 RETURNING org_id, name
		)
		INSERT INTO cp_audit_log (org_id, actor, action, target)
		SELECT org_id, 'sweeper', 'Server unreachable', name FROM flipped`,
		threshold.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PruneMetrics deletes samples older than the retention window. Same DB-clock
// cutoff as MarkStaleUnreachable.
func (s *Store) PruneMetrics(ctx context.Context, retention time.Duration) (int64, error) {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM server_metrics WHERE recorded_at < now() - make_interval(secs => $1)`,
		retention.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
