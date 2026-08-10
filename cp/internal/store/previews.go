package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PreviewEnvironment is one PR's ephemeral environment record.
type PreviewEnvironment struct {
	ID            string     `json:"id"`
	ConnectionID  string     `json:"connectionId"`
	PRNumber      int        `json:"prNumber"`
	EnvironmentID string     `json:"environmentId"`
	ResourceID    *string    `json:"resourceId"`
	Branch        string     `json:"branch"`
	SHA           string     `json:"sha"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"createdAt"`
	ClosedAt      *time.Time `json:"closedAt"`
}

// SetConnectionPreviews flips a connection's preview flag and designates the
// server preview resources land on. The server must belong to the org.
func (s *Store) SetConnectionPreviews(ctx context.Context, orgID, connID string, enabled bool, serverID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if enabled {
		if serverID == "" {
			return ErrInvalid{Msg: "a preview server is required to enable previews"}
		}
		var one int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`,
			orgID, serverID).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
	}
	var srv *string
	if serverID != "" {
		srv = &serverID
	}
	tag, err := tx.Exec(ctx, `
		UPDATE git_connections SET previews_enabled = $3, preview_server_id = $4
		 WHERE org_id = $1 AND id = $2`, orgID, connID, enabled, srv)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	action := "Previews disabled"
	if enabled {
		action = "Previews enabled"
	}
	if err := auditTx(ctx, tx, orgID, actor, action, connID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListPreviewEnvironments returns a connection's preview records, open first,
// newest first within status.
func (s *Store) ListPreviewEnvironments(ctx context.Context, orgID, connID string) ([]PreviewEnvironment, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, connection_id, pr_number, environment_id, resource_id, branch, sha, status, created_at, closed_at
		  FROM preview_environments
		 WHERE org_id = $1 AND connection_id = $2
		 ORDER BY (status = 'open') DESC, created_at DESC LIMIT 50`, orgID, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PreviewEnvironment{}
	for rows.Next() {
		var p PreviewEnvironment
		if err := rows.Scan(&p.ID, &p.ConnectionID, &p.PRNumber, &p.EnvironmentID, &p.ResourceID, &p.Branch, &p.SHA, &p.Status, &p.CreatedAt, &p.ClosedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ensurePreviewTx creates (or refreshes) the PR's ephemeral environment +
// resource: environment "pr-<n>" in the connection's project, the designated
// preview server attached, and ONE ephemeral app resource whose spec is copied
// from the connection's newest non-ephemeral app resource (ports/health/
// compose config carry over; domains do not — previews are reachable through
// the operator's wildcard setup, the preserved DNS-01 hook). Returns the
// environment id for the deploy enqueue.
func ensurePreviewTx(ctx context.Context, tx pgx.Tx, conn GitConnection, prNumber int, branch, sha string) (envID string, resourceID string, err error) {
	// Serialize concurrent PR events per connection.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('preview:' || $1))`, conn.ID); err != nil {
		return "", "", err
	}
	var pv PreviewEnvironment
	err = tx.QueryRow(ctx, `
		SELECT id, environment_id, COALESCE(resource_id, '')
		  FROM preview_environments
		 WHERE connection_id = $1 AND pr_number = $2 AND status = 'open'`,
		conn.ID, prNumber).Scan(&pv.ID, &pv.EnvironmentID, &resourceID)
	if err == nil {
		// The stored pointers are NOT protected: environment_id is deliberately not
		// a foreign key and resource_id is ON DELETE SET NULL, while both targets
		// are user-deletable from the dashboard (a `pr-<n>` env renders with an
		// ordinary Remove button). Returning a dangling environment_id makes the
		// caller's enqueueDeployTx violate deploy_requests' FK, which aborts the
		// whole webhook transaction — including the dedup insert — so the CP 500s
		// on every later push to that PR and redeliveries repeat it forever
		// (SIGMA-165). Verify both pointers and self-heal by recreating instead.
		var live bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM environments WHERE id = $1)`, pv.EnvironmentID).Scan(&live); err != nil {
			return "", "", err
		}
		if live && resourceID != "" {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM resources WHERE id = $1)`, resourceID).Scan(&live); err != nil {
				return "", "", err
			}
		}
		if live && resourceID != "" {
			// Existing preview: refresh head metadata; the caller enqueues the deploy.
			if _, err := tx.Exec(ctx,
				`UPDATE preview_environments SET branch = $2, sha = $3 WHERE id = $1`,
				pv.ID, branch, sha); err != nil {
				return "", "", err
			}
			return pv.EnvironmentID, resourceID, nil
		}
		// Orphaned: close the stale row and fall through to the create path. The
		// partial unique index is on (connection_id, pr_number) WHERE status='open',
		// so closing it first leaves room for the replacement.
		if _, err := tx.Exec(ctx,
			`UPDATE preview_environments SET status = 'closed', closed_at = now() WHERE id = $1`, pv.ID); err != nil {
			return "", "", err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}

	if conn.PreviewServerID == "" {
		return "", "", ErrInvalid{Msg: "previews enabled but no preview server designated"}
	}
	// The preview server must still be LIVE and able to run an app resource — the
	// same invariants CreateResource enforces. Without this an ephemeral app is
	// scheduled onto a tombstoned server (deleted_at set, agent token revoked → it
	// never deploys) — SIGMA-127 — or onto a wrong-type host (a dedicated
	// database/storage server), bypassing the availability matrix — SIGMA-128.
	var previewType string
	if err := tx.QueryRow(ctx,
		`SELECT type FROM servers WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`,
		conn.OrgID, conn.PreviewServerID).Scan(&previewType); errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrInvalid{Msg: "preview server is unavailable (deleted or not found)"}
	} else if err != nil {
		return "", "", err
	}
	allowedApp := false
	for _, t := range AllowedServerTypes("app") {
		if t == previewType {
			allowedApp = true
			break
		}
	}
	if !allowedApp {
		return "", "", ErrInvalid{Msg: fmt.Sprintf("preview server type %q cannot run app resources", previewType)}
	}
	envID = newID("env")
	envName := fmt.Sprintf("pr-%d", prNumber)
	if _, err := tx.Exec(ctx, `
		INSERT INTO environments (id, org_id, project_id, name, production)
		VALUES ($1, $2, $3, $4, FALSE)`, envID, conn.OrgID, conn.ProjectID, envName); err != nil {
		if isUniqueViolation(err) {
			return "", "", fmt.Errorf("%w: an environment named %q already exists", ErrConflict, envName)
		}
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO env_servers (environment_id, server_id, org_id)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, envID, conn.PreviewServerID, conn.OrgID); err != nil {
		return "", "", err
	}
	// Template spec: the newest non-ephemeral app resource in the connection's
	// project (its detection-derived ports/health/compose config).
	spec := json.RawMessage(`{}`)
	err = tx.QueryRow(ctx, `
		SELECT spec FROM resources
		 WHERE org_id = $1 AND project_id = $2 AND kind = 'app' AND NOT ephemeral
		 ORDER BY created_at DESC LIMIT 1`, conn.OrgID, conn.ProjectID).Scan(&spec)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	resourceID = newID("res")
	if _, err := tx.Exec(ctx, `
		INSERT INTO resources (id, org_id, project_id, environment_id, server_id, name, kind, spec, ephemeral)
		VALUES ($1, $2, $3, $4, $5, $6, 'app', $7, TRUE)`,
		resourceID, conn.OrgID, conn.ProjectID, envID, conn.PreviewServerID, envName, spec); err != nil {
		return "", "", err
	}
	// The template spec is copied verbatim, placements included, so the derived
	// placement rows have to be written for the copy too (SIGMA-332).
	if err := syncServicePlacementsTx(ctx, tx, resourceID); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO preview_environments (id, org_id, connection_id, pr_number, environment_id, resource_id, branch, sha)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		newID("prv"), conn.OrgID, conn.ID, prNumber, envID, resourceID, branch, sha); err != nil {
		return "", "", err
	}
	if err := auditTx(ctx, tx, conn.OrgID, WebhookActor, "Preview environment created", envName+" ("+conn.RepoFullName+")"); err != nil {
		return "", "", err
	}
	return envID, resourceID, nil
}

// teardownPreviewTx closes a PR's preview: every ephemeral resource's volumes
// go through the pre-authorised destructive path (system-audited, uniform with
// the interactive flow), the resource and environment rows are removed, and
// the preview record is marked closed. Returns the distinct servers that hosted
// anything in the environment, for the caller's post-commit reconcile
// (ok=false when no open preview exists).
func teardownPreviewTx(ctx context.Context, tx pgx.Tx, conn GitConnection, prNumber int) (servers []string, ok bool, err error) {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('preview:' || $1))`, conn.ID); err != nil {
		return nil, false, err
	}
	var pvID, envID string
	var resID *string
	err = tx.QueryRow(ctx, `
		SELECT id, environment_id, resource_id FROM preview_environments
		 WHERE connection_id = $1 AND pr_number = $2 AND status = 'open'`,
		conn.ID, prNumber).Scan(&pvID, &envID, &resID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// SIGMA-280. A preview environment is not only the ephemeral app resource we
	// created: a developer testing a migration adds a managed database to it
	// through the ordinary New Resource wizard and points a backup target at it.
	// Deleting the environment below cascades those backup_policies away, and
	// with them the ONLY copy of the wrapped restic repo password — while the
	// snapshots stay in the customer's bucket, still billing and now permanently
	// undecryptable. SIGMA-170 made that copy mandatory before any such cascade;
	// this path bypassed it. Archive first, exactly as DeleteEnvironment does.
	if err := archiveRepoKeysTx(ctx, tx, conn.OrgID, "environment", envID); err != nil {
		return nil, false, fmt.Errorf("archive repo keys: %w", err)
	}
	// Same reason this is the shared helper rather than a preview-local loop:
	// the environment may hold more than the preview's own resource, and each
	// ephemeral one needs its volumes queued for removal and its server
	// re-rendered. The helper is what DeleteEnvironment uses, so the two paths
	// cannot drift again.
	servers, err = cascadeResourceCleanupTx(ctx, tx, conn.OrgID, "environment_id", envID)
	if err != nil {
		return nil, false, err
	}
	if resID != nil {
		// Belt and braces: the preview's own resource is normally inside envID
		// (and so already handled above), but delete it by id so a stray row can
		// never outlive its preview.
		var srv string
		err = tx.QueryRow(ctx,
			`DELETE FROM resources WHERE org_id = $1 AND id = $2 RETURNING COALESCE(server_id, '')`,
			conn.OrgID, *resID).Scan(&srv)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, err
		}
		if err == nil && srv != "" {
			seen := false
			for _, s := range servers {
				if s == srv {
					seen = true
					break
				}
			}
			if !seen {
				servers = append(servers, srv)
			}
		}
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM environments WHERE org_id = $1 AND id = $2`, conn.OrgID, envID); err != nil {
		return nil, false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE preview_environments SET status = 'closed', closed_at = now() WHERE id = $1`, pvID); err != nil {
		return nil, false, err
	}
	if err := auditTx(ctx, tx, conn.OrgID, WebhookActor, "Preview environment torn down",
		fmt.Sprintf("pr-%d (%s)", prNumber, conn.RepoFullName)); err != nil {
		return nil, false, err
	}
	return servers, true, nil
}
