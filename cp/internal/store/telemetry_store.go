package store

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// tenantCache maps org → VictoriaMetrics accountID; stable, so cache forever.
var (
	tenantMu    sync.Mutex
	tenantCache = map[string]int{}
)

// OrgTenant returns the org's numeric telemetry tenant, allocating one on
// first use (VictoriaMetrics cluster accountIDs are numeric; orgs are strings).
func (s *Store) OrgTenant(ctx context.Context, orgID string) (int, error) {
	tenantMu.Lock()
	if t, ok := tenantCache[orgID]; ok {
		tenantMu.Unlock()
		return t, nil
	}
	tenantMu.Unlock()

	var tenant int
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO org_tenants (org_id) VALUES ($1)
		ON CONFLICT (org_id) DO UPDATE SET org_id = org_tenants.org_id
		RETURNING tenant`, orgID).Scan(&tenant)
	if err != nil {
		return 0, err
	}
	tenantMu.Lock()
	tenantCache[orgID] = tenant
	tenantMu.Unlock()
	return tenant, nil
}

// forgetOrgTenant drops a cached mapping. Called by PurgeOrg (SIGMA-298):
// the cache comment above says "stable, so cache forever", which was true right
// up until orgs could be deleted. Without this, an org id provisioned again
// after a teardown would be handed its predecessor's retired tenant straight
// out of process memory, with no query to correct it — and the new customer's
// series would land in the deleted customer's tenant.
func forgetOrgTenant(orgID string) {
	tenantMu.Lock()
	delete(tenantCache, orgID)
	tenantMu.Unlock()
}

// TelemetryResourceMeta is the label enrichment for one resource's series and
// log streams: {org, project, env, server, resource} — the hard allowlist.
type TelemetryResourceMeta struct {
	ProjectID     string
	EnvironmentID string
}

// TelemetryResourceMetaForServer resolves a resource's project/env labels,
// scoped to the reporting server (an agent can only label series for
// resources scheduled on ITS host — mirrors the secrets BOLA guard).
func (s *Store) TelemetryResourceMetaForServer(ctx context.Context, serverID, resourceID string) (TelemetryResourceMeta, error) {
	var m TelemetryResourceMeta
	err := s.Pool.QueryRow(ctx,
		`SELECT project_id, environment_id FROM resources WHERE id = $1 AND server_id = $2`,
		resourceID, serverID).Scan(&m.ProjectID, &m.EnvironmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TelemetryResourceMeta{}, ErrNotFound
	}
	return m, err
}

// SweepUsageHours writes the idempotent hourly (resource, hour) usage rows —
// the A-4 metering hook. Safe to run any number of times per hour.
func (s *Store) SweepUsageHours(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO usage_hours (org_id, resource_id, hour)
		SELECT org_id, id, date_trunc('hour', $1::timestamptz) FROM resources
		ON CONFLICT DO NOTHING`, now.UTC())
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DeployStats is the M1 gate's deploy half: success rate over the last N
// terminal deploys plus the org's first successful deploy (TTFD's deploy leg;
// the signup leg lives web-side where the better-auth accounts are).
type DeployStats struct {
	Window        int        `json:"window"`
	Total         int        `json:"total"`
	Succeeded     int        `json:"succeeded"`
	FirstDeployAt *time.Time `json:"firstDeployAt"`
}

// DeployStatsForOrg computes the org's terminal-deploy success rate over the
// last `window` deploys (from the immutable P1-9 rows, per SIGMA-52).
func (s *Store) DeployStatsForOrg(ctx context.Context, orgID string, window int) (DeployStats, error) {
	if window <= 0 || window > 1000 {
		window = 500
	}
	out := DeployStats{Window: window}
	rows, err := s.Pool.Query(ctx, `
		SELECT status FROM deployments
		 WHERE org_id = $1 AND status IN ('success', 'failed', 'rolled_back')
		 ORDER BY created_at DESC LIMIT $2`, orgID, window)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return out, err
		}
		out.Total++
		if status == "success" || status == "rolled_back" {
			out.Succeeded++
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	var first *time.Time
	err = s.Pool.QueryRow(ctx, `
		SELECT MIN(created_at) FROM deployments WHERE org_id = $1 AND status IN ('success', 'rolled_back')`,
		orgID).Scan(&first)
	if err != nil {
		return out, err
	}
	out.FirstDeployAt = first
	return out, nil
}

// ConnectedServerCount counts the org's live (heartbeating) servers — the
// usage meter every euro of self-serve revenue keys off.
func (s *Store) ConnectedServerCount(ctx context.Context, orgID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM servers
		 WHERE org_id = $1 AND deleted_at IS NULL AND status = 'running'`, orgID).Scan(&n)
	return n, err
}
