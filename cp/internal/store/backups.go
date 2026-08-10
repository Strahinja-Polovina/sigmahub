package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Backup run lifecycle (P1-11). The scheduler inserts pending rows, the
// reconciler renders pending/running rows as typed DSD ops, the agent executes
// and reports the terminal result. An unrestored backup counts as no backup.
const (
	BackupRunPending = "pending"
	BackupRunRunning = "running"
	BackupRunSuccess = "success"
	BackupRunFailed  = "failed"
)

// targetAAD / repoKeyAAD bind ciphertexts to their row identity (mirrors
// secretAAD): moved ciphertext fails to open.
func targetAAD(orgID, targetID string) []byte  { return []byte(orgID + "|bktarget|" + targetID) }
func repoKeyAAD(orgID, policyID string) []byte { return []byte(orgID + "|bkrepo|" + policyID) }

// BackupTarget is an S3-compatible backup destination's metadata. The secret
// key never rides on this type.
type BackupTarget struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Endpoint       string    `json:"endpoint"`
	Bucket         string    `json:"bucket"`
	Region         string    `json:"region"`
	ForcePathStyle bool      `json:"forcePathStyle"`
	AccessKey      string    `json:"accessKey"`
	CreatedBy      string    `json:"createdBy"`
	CreatedAt      time.Time `json:"createdAt"`
}

// CreateBackupTargetInput describes a new S3-compatible target. SecretKey is
// encrypted under the org DEK at rest.
type CreateBackupTargetInput struct {
	Name           string
	Endpoint       string
	Bucket         string
	Region         string
	ForcePathStyle bool
	AccessKey      string
	SecretKey      string
}

// CreateBackupTarget stores an S3-compatible backup target with its secret key
// envelope-encrypted. Audited.
func (s *Store) CreateBackupTarget(ctx context.Context, orgID, actor string, in CreateBackupTargetInput) (BackupTarget, error) {
	if in.Name == "" || in.Bucket == "" || in.AccessKey == "" || in.SecretKey == "" {
		return BackupTarget{}, ErrInvalid{Msg: "name, bucket, accessKey and secretKey are required"}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BackupTarget{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	dekID, dek, err := s.activeDEKTx(ctx, tx, orgID)
	if err != nil {
		return BackupTarget{}, err
	}
	t := BackupTarget{
		ID: newID("bkt"), Name: in.Name, Endpoint: in.Endpoint, Bucket: in.Bucket,
		Region: in.Region, ForcePathStyle: in.ForcePathStyle, AccessKey: in.AccessKey, CreatedBy: actor,
	}
	nonce, ct, err := gcmSeal(dek, targetAAD(orgID, t.ID), []byte(in.SecretKey))
	if err != nil {
		return BackupTarget{}, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO backup_targets (id, org_id, name, endpoint, bucket, region, force_path_style, access_key, secret_ciphertext, secret_nonce, dek_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING created_at`,
		t.ID, orgID, t.Name, t.Endpoint, t.Bucket, t.Region, t.ForcePathStyle, t.AccessKey, ct, nonce, dekID, actor,
	).Scan(&t.CreatedAt)
	if isUniqueViolation(err) {
		return BackupTarget{}, fmt.Errorf("%w: a backup target named %q already exists", ErrConflict, in.Name)
	}
	if err != nil {
		return BackupTarget{}, fmt.Errorf("insert backup target: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Backup target created", t.Name); err != nil {
		return BackupTarget{}, err
	}
	return t, tx.Commit(ctx)
}

// ListBackupTargets returns target METADATA (never the secret key).
func (s *Store) ListBackupTargets(ctx context.Context, orgID string) ([]BackupTarget, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, name, endpoint, bucket, region, force_path_style, access_key, created_by, created_at
		  FROM backup_targets WHERE org_id = $1 ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackupTarget{}
	for rows.Next() {
		var t BackupTarget
		if err := rows.Scan(&t.ID, &t.Name, &t.Endpoint, &t.Bucket, &t.Region, &t.ForcePathStyle, &t.AccessKey, &t.CreatedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteBackupTarget removes a target unless a policy still points at it.
func (s *Store) DeleteBackupTarget(ctx context.Context, orgID, targetID, actor string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var inUse bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM backup_policies WHERE org_id = $1 AND target_id = $2)`,
		orgID, targetID).Scan(&inUse); err != nil {
		return err
	}
	if inUse {
		return ErrInvalid{Msg: "backup target is in use by a backup policy; point the policy elsewhere first"}
	}
	var name string
	err = tx.QueryRow(ctx,
		`DELETE FROM backup_targets WHERE org_id = $1 AND id = $2 RETURNING name`, orgID, targetID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	// The FK backstop (SIGMA-135) rejects deleting a target a policy still
	// references, even when the EXISTS check above raced a concurrent
	// UpdateBackupPolicy. Surface the same friendly conflict instead of a raw
	// SQLSTATE.
	if isForeignKeyViolation(err) {
		return ErrInvalid{Msg: "backup target is in use by a backup policy; point the policy elsewhere first"}
	}
	if err != nil {
		return err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Backup target deleted", name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpdateBackupPolicyInput mutates a database's backup policy. Nil fields keep
// the current value; TargetID "" clears the target (backups pause loudly).
type UpdateBackupPolicyInput struct {
	TargetID    *string
	Schedule    *string
	KeepDaily   *int
	KeepWeekly  *int
	KeepMonthly *int
	Enabled     *bool
	// PitrEnabled (P2-5): postgres only — enabling on another engine is a
	// typed error, never a silent no-op.
	PitrEnabled *bool
}

// UpdateBackupPolicy updates a database resource's backup policy. Audited.
func (s *Store) UpdateBackupPolicy(ctx context.Context, orgID, resourceID, actor string, in UpdateBackupPolicyInput) (BackupPolicy, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BackupPolicy{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bp BackupPolicy
	var target *string
	err = tx.QueryRow(ctx, `
		SELECT id, resource_id, schedule, keep_daily, keep_weekly, keep_monthly, target_id, enabled, pitr_enabled
		  FROM backup_policies WHERE org_id = $1 AND resource_id = $2 FOR UPDATE`,
		orgID, resourceID).Scan(&bp.ID, &bp.ResourceID, &bp.Schedule, &bp.KeepDaily, &bp.KeepWeekly, &bp.KeepMonthly, &target, &bp.Enabled, &bp.PitrEnabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupPolicy{}, ErrNotDatabase
	}
	if err != nil {
		return BackupPolicy{}, err
	}
	bp.TargetID = target

	if in.TargetID != nil {
		if *in.TargetID == "" {
			bp.TargetID = nil
		} else {
			// The target must belong to this org — never a cross-tenant pointer.
			var one int
			if err := tx.QueryRow(ctx,
				`SELECT 1 FROM backup_targets WHERE org_id = $1 AND id = $2`, orgID, *in.TargetID).Scan(&one); errors.Is(err, pgx.ErrNoRows) {
				return BackupPolicy{}, ErrInvalid{Msg: "unknown backup target"}
			} else if err != nil {
				return BackupPolicy{}, err
			}
			bp.TargetID = in.TargetID
		}
	}
	if in.Schedule != nil {
		if *in.Schedule != "daily" {
			return BackupPolicy{}, ErrInvalid{Msg: `schedule must be "daily" in v1`}
		}
		bp.Schedule = *in.Schedule
	}
	if in.KeepDaily != nil {
		bp.KeepDaily = *in.KeepDaily
	}
	if in.KeepWeekly != nil {
		bp.KeepWeekly = *in.KeepWeekly
	}
	if in.KeepMonthly != nil {
		bp.KeepMonthly = *in.KeepMonthly
	}
	if bp.KeepDaily < 1 || bp.KeepDaily > 365 || bp.KeepWeekly < 0 || bp.KeepWeekly > 52 || bp.KeepMonthly < 0 || bp.KeepMonthly > 24 {
		return BackupPolicy{}, ErrInvalid{Msg: "retention out of range"}
	}
	if in.Enabled != nil {
		bp.Enabled = *in.Enabled
	}
	if in.PitrEnabled != nil {
		if *in.PitrEnabled {
			var engine string
			if err := tx.QueryRow(ctx,
				`SELECT engine FROM db_credentials WHERE org_id = $1 AND resource_id = $2`,
				orgID, resourceID).Scan(&engine); err != nil {
				return BackupPolicy{}, err
			}
			if engine != "postgres" {
				return BackupPolicy{}, ErrInvalid{Msg: "point-in-time recovery is available for postgres resources only"}
			}
			// PITR turns on archive_mode; with no target the WAL shipper has
			// nowhere to drain to, so the spool grows until archive_command
			// fails and Postgres stops recycling WAL (SIGMA-71).
			if bp.TargetID == nil {
				return BackupPolicy{}, ErrInvalid{Msg: "point-in-time recovery requires a backup target"}
			}
		}
		bp.PitrEnabled = *in.PitrEnabled
	}
	// Clearing the target must force PITR off in the same update — archiving to
	// nowhere would otherwise silently fill the spool (SIGMA-71).
	if bp.TargetID == nil {
		bp.PitrEnabled = false
	}
	if _, err := tx.Exec(ctx, `
		UPDATE backup_policies
		   SET target_id = $3, schedule = $4, keep_daily = $5, keep_weekly = $6, keep_monthly = $7, enabled = $8, pitr_enabled = $9, updated_at = now()
		 WHERE org_id = $1 AND resource_id = $2`,
		orgID, resourceID, bp.TargetID, bp.Schedule, bp.KeepDaily, bp.KeepWeekly, bp.KeepMonthly, bp.Enabled, bp.PitrEnabled); err != nil {
		// The FK backstop (SIGMA-135) rejects pointing a policy at a target that
		// a concurrent DeleteBackupTarget removed after the validation read above.
		if isForeignKeyViolation(err) {
			return BackupPolicy{}, ErrInvalid{Msg: "unknown backup target"}
		}
		return BackupPolicy{}, fmt.Errorf("update backup policy: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Backup policy updated", resourceID); err != nil {
		return BackupPolicy{}, err
	}
	return bp, tx.Commit(ctx)
}

// ensureRepoKeyTx generates and wraps the per-resource restic repo key on
// first use. The plaintext is never persisted — only handed to the executing
// agent via the audited BackupCredentialForRun path.
func (s *Store) ensureRepoKeyTx(ctx context.Context, tx pgx.Tx, orgID, policyID string) error {
	var have []byte
	if err := tx.QueryRow(ctx,
		`SELECT repo_key_ciphertext FROM backup_policies WHERE org_id = $1 AND id = $2 FOR UPDATE`,
		orgID, policyID).Scan(&have); err != nil {
		return err
	}
	if len(have) > 0 {
		return nil
	}
	key := make([]byte, 24)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	dekID, dek, err := s.activeDEKTx(ctx, tx, orgID)
	if err != nil {
		return err
	}
	nonce, ct, err := gcmSeal(dek, repoKeyAAD(orgID, policyID), []byte(hex.EncodeToString(key)))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		UPDATE backup_policies SET repo_key_ciphertext = $3, repo_key_nonce = $4, repo_dek_id = $5
		 WHERE org_id = $1 AND id = $2`, orgID, policyID, ct, nonce, dekID)
	return err
}

// runDayUTC renders THE expression that decides which UTC day a backup run
// belongs to, optionally qualified by a table alias.
//
// It is a function rather than four hand-written copies because the copies
// disagreed: the scheduler and migration 0030's partial unique index keyed a
// run's day on created_at, while VerifyDays — the query the M1 30-day
// green-streak gate reads — bucketed on finished_at, so every run that crossed
// midnight landed on a different day in the calendar than in the schedule
// (SIGMA-285). Keep the text byte-identical to the index expression in
// migration 0030, including the 'UTC' literal's case: Postgres matches an
// expression index by the parsed expression, and 'utc' is a different constant
// from 'UTC'.
func runDayUTC(alias string) string {
	if alias != "" {
		alias += "."
	}
	return "((" + alias + "created_at AT TIME ZONE 'UTC')::date)"
}

// CreateDueBackupRuns is the scheduler's tick: for every enabled, targeted
// database policy it inserts (at most) one backup run per day and one verify
// run per day (verify only once THIS day's backup has succeeded and recorded a
// dump sha — see the predicate below, SIGMA-282). Returns the
// distinct servers whose DSDs must re-render. The reconciler is level-triggered
// spec sync with no time primitive — this is the time primitive (SIGMA-50).
func (s *Store) CreateDueBackupRuns(ctx context.Context, now time.Time) (servers []struct{ ServerID, OrgID string }, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	day := now.UTC().Truncate(24 * time.Hour)
	rows, err := tx.Query(ctx, `
		SELECT p.id, p.org_id, p.resource_id, dc.server_id,
		       EXISTS (SELECT 1 FROM backup_runs r WHERE r.policy_id = p.id AND r.kind = 'backup'
		                  AND r.created_at >= $1) AS backed_today,
		       EXISTS (SELECT 1 FROM backup_runs r WHERE r.policy_id = p.id AND r.kind = 'verify'
		                  AND r.created_at >= $1) AS verified_today,
		       -- A verify checks THIS day's dump against THIS day's sha, which is
		       -- exactly what BackupRunsForServer resolves ExpectedSha from. So the
		       -- question the enqueue must ask is not "has this policy ever
		       -- succeeded" but "does today's dump exist yet" (SIGMA-282).
		       EXISTS (SELECT 1 FROM backup_runs r WHERE r.policy_id = p.id AND r.kind = 'backup'
		                  AND r.status = 'success' AND r.created_at >= $1
		                  AND COALESCE(r.dump_sha256, '') <> '') AS sha_today,
		       (p.pitr_enabled AND dc.engine = 'postgres' AND NOT EXISTS (
		            SELECT 1 FROM backup_runs r WHERE r.policy_id = p.id AND r.kind = 'basebackup'
		               AND r.created_at >= $1)) AS base_due
		  FROM backup_policies p
		  JOIN db_credentials dc ON dc.resource_id = p.resource_id
		 WHERE p.enabled AND p.target_id IS NOT NULL`, day)
	if err != nil {
		return nil, err
	}
	type due struct {
		policyID, orgID, resourceID, serverID string
		backup, verify, base                  bool
	}
	var work []due
	for rows.Next() {
		var d due
		var backedToday, verifiedToday, shaToday bool
		if err := rows.Scan(&d.policyID, &d.orgID, &d.resourceID, &d.serverID, &backedToday, &verifiedToday, &shaToday, &d.base); err != nil {
			rows.Close()
			return nil, err
		}
		d.backup = !backedToday
		// Do not enqueue a verify until the dump it is meant to check exists.
		//
		// This used to be `!verifiedToday && (hasSuccess || d.backup)`, where
		// hasSuccess meant "succeeded at any point in this policy's life". Both
		// halves enqueue a verify for a day whose dump may never appear, and
		// SIGMA-137 (rightly) taught the reconciler to hold such a verify back
		// until its own day's sha is known — so on a day whose backup fails the
		// row is undispatchable by construction. It sat pending until
		// TimeoutStaleBackupRuns failed it at the 6h queue budget with a detail
		// blaming the control plane, giving on-call a second, fictitious alert per
		// database per night ("verify_failed: never dispatched") beside the honest
		// backup_failed, and marking the day red for a second, invented reason the
		// M1 green-streak gate cannot tell from real verification breakage.
		//
		// The scheduler ticks every minute, so waiting for the sha costs at most a
		// minute of latency on days that do back up successfully.
		d.verify = !verifiedToday && shaToday
		if d.backup || d.verify || d.base {
			work = append(work, d)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	for _, d := range work {
		if err := s.ensureRepoKeyTx(ctx, tx, d.orgID, d.policyID); err != nil {
			return nil, fmt.Errorf("ensure repo key: %w", err)
		}
		if d.backup {
			if _, err := tx.Exec(ctx, `
				INSERT INTO backup_runs (id, org_id, resource_id, policy_id, server_id, kind)
				VALUES ($1, $2, $3, $4, $5, 'backup')
				ON CONFLICT (policy_id, kind, `+runDayUTC("")+`) WHERE kind IN ('backup', 'basebackup', 'verify') DO NOTHING`,
				newID("run"), d.orgID, d.resourceID, d.policyID, d.serverID); err != nil {
				return nil, err
			}
		}
		if d.verify {
			if _, err := tx.Exec(ctx, `
				INSERT INTO backup_runs (id, org_id, resource_id, policy_id, server_id, kind)
				VALUES ($1, $2, $3, $4, $5, 'verify')
				ON CONFLICT (policy_id, kind, `+runDayUTC("")+`) WHERE kind IN ('backup', 'basebackup', 'verify') DO NOTHING`,
				newID("run"), d.orgID, d.resourceID, d.policyID, d.serverID); err != nil {
				return nil, err
			}
		}
		// P2-5: the daily physical base backup PITR replays from.
		if d.base {
			if _, err := tx.Exec(ctx, `
				INSERT INTO backup_runs (id, org_id, resource_id, policy_id, server_id, kind)
				VALUES ($1, $2, $3, $4, $5, 'basebackup')
				ON CONFLICT (policy_id, kind, `+runDayUTC("")+`) WHERE kind IN ('backup', 'basebackup', 'verify') DO NOTHING`,
				newID("run"), d.orgID, d.resourceID, d.policyID, d.serverID); err != nil {
				return nil, err
			}
		}
		if !seen[d.serverID] {
			seen[d.serverID] = true
			servers = append(servers, struct{ ServerID, OrgID string }{d.serverID, d.orgID})
		}
	}
	return servers, tx.Commit(ctx)
}

// TimeoutStaleBackupRuns fails runs that stopped making progress, so a crashed
// agent can't freeze the schedule (tomorrow's run still enqueues) and the day
// honestly reads not-green. Timed-out runs alert like any other failure (P2-6)
// — this path bypasses SetBackupRunResult.
//
// Two budgets (SIGMA-163): a RUNNING run is timed from started_at — the moment
// the agent fetched its credential — with execMaxAge sized just above the
// agent's own 25-minute op cap. A PENDING run has not consumed any execution
// budget: verify rows are deliberately withheld until their backup's sha is
// known (SIGMA-137) and the agent applies ops serially, so queue time is
// unbounded by design and gets the far larger queueMaxAge. The old single
// created_at ceiling force-failed the day's verify whenever the dump alone
// took ~12 minutes.
func (s *Store) TimeoutStaleBackupRuns(ctx context.Context, execMaxAge, queueMaxAge time.Duration) (int, error) {
	now := time.Now()
	var timedOut int
	err := s.Pool.QueryRow(ctx, `
		WITH stale AS (
			SELECT id, status AS old_status
			  FROM backup_runs
			 WHERE (status = 'running' AND COALESCE(started_at, created_at) < $1)
			    OR (status = 'pending' AND created_at < $2)
		),
		failed AS (
			UPDATE backup_runs b
			   SET status = 'failed',
			       detail = CASE WHEN s.old_status = 'pending' THEN 'timed out in queue' ELSE 'timed out' END,
			       finished_at = now()
			  FROM stale s
			 WHERE b.id = s.id
			 RETURNING b.id, b.org_id, b.kind, b.resource_id, s.old_status
		),
		enqueued AS (
			INSERT INTO alert_outbox (org_id, channel_id, event, dedup_key, title, body)
			SELECT f.org_id, r.channel_id,
			       CASE WHEN f.kind = 'verify' THEN 'verify_failed' ELSE 'backup_failed' END,
			       'bkr:' || f.id,
			       CASE f.kind WHEN 'backup' THEN 'Backup' WHEN 'verify' THEN 'Restore-verify' ELSE 'Restore' END
			         || ' timed out for ' || COALESCE(res.name, f.resource_id),
			       CASE WHEN f.old_status = 'pending'
			            THEN 'The run was never dispatched within its queue window and was failed by the scheduler.'
			            ELSE 'The dispatched run stopped reporting and was failed by the scheduler (agent crashed or unreachable mid-run).'
			       END
			  FROM failed f
			  LEFT JOIN resources res ON res.id = f.resource_id
			  JOIN alert_rules r ON r.org_id = f.org_id
			   AND r.event = CASE WHEN f.kind = 'verify' THEN 'verify_failed' ELSE 'backup_failed' END
			  JOIN alert_channels c ON c.id = r.channel_id AND c.enabled
			 WHERE NOT EXISTS (
				SELECT 1 FROM alert_outbox o
				 WHERE o.channel_id = r.channel_id AND o.dedup_key = 'bkr:' || f.id
			 )
		)
		SELECT count(*) FROM failed`,
		now.Add(-execMaxAge), now.Add(-queueMaxAge)).Scan(&timedOut)
	if err != nil {
		return 0, err
	}
	return timedOut, nil
}

// BackupRunSpec is the reconciler's render input for one open backup run.
type BackupRunSpec struct {
	RunID       string
	Kind        string // backup|verify|restore
	ResourceID  string // repo owner (the source database)
	ServerID    string
	Engine      string
	Database    string
	Username    string
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	ExpectedSha string // verify: this day's successful backup's dump sha; restore: the digest pinned on the run
	// SnapshotID is the restic snapshot a restore run was pinned to when it was
	// created (SIGMA-245). Empty for backup/verify runs, which have not produced
	// a snapshot yet or work off `latest` by design.
	SnapshotID string
	// Restore runs load into this resource (fresh P1-10 database).
	RestoreResourceID string
	RestoreDatabase   string
	RestoreUsername   string
	// RecoveryTargetTime is set only for restore-pitr runs: the point in time
	// the agent replays WAL up to. nil for every other kind.
	RecoveryTargetTime *time.Time
}

// BackupRunsForServer returns the open (pending/running) runs the reconciler
// renders into a server's DSD, oldest first for deterministic op ordering.
func (s *Store) BackupRunsForServer(ctx context.Context, serverID string) ([]BackupRunSpec, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT r.id, r.kind, r.resource_id, r.server_id, dc.engine, dc.dbname, dc.username,
		       p.keep_daily, p.keep_weekly, p.keep_monthly,
		       -- A VERIFY must check THIS day's backup, not the previous day's:
		       -- correlate the expected sha to the same UTC day's successful backup.
		       -- Empty until that backup succeeds, which the reconciler uses to hold
		       -- the verify until its dump exists (SIGMA-137).
		       --
		       -- Every other kind reads the digest recorded ON THE RUN. A restore
		       -- pins the snapshot it will load when it is created, so it must not
		       -- be re-derived from a calendar day here: correlating it meant a
		       -- restore queued on a day whose backup had failed rendered with an
		       -- empty digest, which the agent refuses outright — after the CP had
		       -- already provisioned the target database (SIGMA-245).
		       CASE WHEN r.kind = 'verify'
		            THEN COALESCE((SELECT b.dump_sha256 FROM backup_runs b
		                            WHERE b.policy_id = r.policy_id AND b.kind = 'backup' AND b.status = 'success'
		                              AND `+runDayUTC("b")+` = `+runDayUTC("r")+`
		                            ORDER BY b.finished_at DESC LIMIT 1), '')
		            ELSE COALESCE(r.dump_sha256, '')
		       END AS expected_sha,
		       COALESCE(r.snapshot_id, ''),
		       COALESCE(r.restore_resource_id, ''),
		       COALESCE(rdc.dbname, ''), COALESCE(rdc.username, ''),
		       r.recovery_target_time
		  FROM backup_runs r
		  JOIN backup_policies p ON p.id = r.policy_id
		  JOIN db_credentials dc ON dc.resource_id = r.resource_id
		  LEFT JOIN db_credentials rdc ON rdc.resource_id = r.restore_resource_id
		 WHERE r.server_id = $1 AND r.status IN ('pending', 'running')
		 ORDER BY r.created_at,
		          CASE r.kind WHEN 'backup' THEN 0 WHEN 'basebackup' THEN 1 WHEN 'verify' THEN 2 ELSE 3 END,
		          r.id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackupRunSpec{}
	for rows.Next() {
		var b BackupRunSpec
		if err := rows.Scan(&b.RunID, &b.Kind, &b.ResourceID, &b.ServerID, &b.Engine, &b.Database, &b.Username,
			&b.KeepDaily, &b.KeepWeekly, &b.KeepMonthly, &b.ExpectedSha, &b.SnapshotID,
			&b.RestoreResourceID, &b.RestoreDatabase, &b.RestoreUsername, &b.RecoveryTargetTime); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BackupCredential is the per-run material the executing agent fetches: the
// restic repo location/key and the target's S3 credentials. Never persisted
// plaintext; every fetch is audited (the per-run key-release invariant).
type BackupCredential struct {
	Repository     string `json:"repository"` // restic -r value, e.g. s3:https://endpoint/bucket/sigmahub/<res>
	RepoKey        string `json:"repoKey"`
	AccessKey      string `json:"accessKey"`
	SecretKey      string `json:"secretKey"`
	Region         string `json:"region,omitempty"`
	ForcePathStyle bool   `json:"forcePathStyle,omitempty"`
}

// resticRepository renders the restic -r URL for a target + resource repo.
func resticRepository(endpoint, bucket, resourceID string) string {
	host := endpoint
	if host == "" {
		host = "s3.amazonaws.com"
	}
	return "s3:" + strings.TrimSuffix(host, "/") + "/" + bucket + "/sigmahub/" + resourceID
}

// BackupCredentialForRun releases the repo key + target credentials for ONE
// open run to the server it is scheduled on (BOLA scope mirrors the secrets
// resolve path). Audited per fetch.
func (s *Store) BackupCredentialForRun(ctx context.Context, serverID, runID string) (BackupCredential, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BackupCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		orgID, resourceID, policyID      string
		endpoint, bucket, region, access string
		forcePathStyle                   bool
		secretCT, secretNonce            []byte
		repoCT, repoNonce                []byte
		tDekID                           string
		rDekID                           *string
	)
	var targetID string
	err = tx.QueryRow(ctx, `
		SELECT r.org_id, r.resource_id, r.policy_id, t.id,
		       t.endpoint, t.bucket, t.region, t.access_key, t.force_path_style,
		       t.secret_ciphertext, t.secret_nonce, t.dek_id,
		       p.repo_key_ciphertext, p.repo_key_nonce, p.repo_dek_id
		  FROM backup_runs r
		  JOIN backup_policies p ON p.id = r.policy_id
		  JOIN backup_targets t ON t.id = p.target_id
		 WHERE r.id = $1 AND r.server_id = $2 AND r.status IN ('pending', 'running')`,
		runID, serverID).Scan(&orgID, &resourceID, &policyID, &targetID,
		&endpoint, &bucket, &region, &access, &forcePathStyle,
		&secretCT, &secretNonce, &tDekID,
		&repoCT, &repoNonce, &rDekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupCredential{}, ErrNotFound
	}
	if err != nil {
		return BackupCredential{}, err
	}
	if len(repoCT) == 0 || rDekID == nil {
		return BackupCredential{}, fmt.Errorf("backup run %s has no repo key", runID)
	}
	tDek, err := s.dekPlaintext(ctx, tx, tDekID)
	if err != nil {
		return BackupCredential{}, err
	}
	secretKey, err := gcmOpen(tDek, targetAAD(orgID, targetID), secretNonce, secretCT)
	if err != nil {
		return BackupCredential{}, fmt.Errorf("decrypt target secret: %w", err)
	}
	rDek, err := s.dekPlaintext(ctx, tx, *rDekID)
	if err != nil {
		return BackupCredential{}, err
	}
	repoKey, err := gcmOpen(rDek, repoKeyAAD(orgID, policyID), repoNonce, repoCT)
	if err != nil {
		return BackupCredential{}, fmt.Errorf("decrypt repo key: %w", err)
	}
	// Mark running on first fetch so the schedule/timeout sweep sees progress.
	// started_at anchors the sweep's execution budget: without it the 30-minute
	// ceiling covered queue time too, force-failing runs that were never late
	// (SIGMA-163).
	if _, err := tx.Exec(ctx,
		`UPDATE backup_runs SET status = 'running', started_at = now()
		  WHERE id = $1 AND status = 'pending'`, runID); err != nil {
		return BackupCredential{}, err
	}
	if err := auditTx(ctx, tx, orgID, "agent:"+serverID, "Backup repo key unwrapped (agent)", resourceID+" run "+runID); err != nil {
		return BackupCredential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BackupCredential{}, err
	}
	return BackupCredential{
		Repository:     resticRepository(endpoint, bucket, resourceID),
		RepoKey:        string(repoKey),
		AccessKey:      access,
		SecretKey:      string(secretKey),
		Region:         region,
		ForcePathStyle: forcePathStyle,
	}, nil
}

// SetBackupRunResult records a run's terminal outcome, reported by the
// executing agent (BOLA-scoped to the run's server). Audited.
func (s *Store) SetBackupRunResult(ctx context.Context, serverID, runID string, ok bool, snapshotID, dumpSha, detail string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	status := BackupRunFailed
	if ok {
		status = BackupRunSuccess
	}
	var orgID, kind, resourceID string
	err = tx.QueryRow(ctx, `
		UPDATE backup_runs
		   SET status = $3, snapshot_id = $4, dump_sha256 = $5, detail = left($6, 4000), finished_at = now()
		 WHERE id = $1 AND server_id = $2 AND status IN ('pending', 'running')
		 RETURNING org_id, kind, resource_id`,
		runID, serverID, status, snapshotID, dumpSha, detail).Scan(&orgID, &kind, &resourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	action := map[string]string{"backup": "Backup", "verify": "Restore-verify", "restore": "Restore"}[kind]
	if ok {
		action += " succeeded"
	} else {
		action += " failed"
	}
	if err := auditTx(ctx, tx, orgID, "agent:"+serverID, action, resourceID+" run "+runID); err != nil {
		return err
	}
	// P2-6: failed runs alert once each (dedup = run id). Verify failures get
	// their own event — a red verify day blocks the M1 gate, so orgs may route
	// it louder than an ordinary backup failure.
	if !ok {
		event := AlertBackupFailed
		if kind == "verify" {
			event = AlertVerifyFailed
		}
		var resName string
		if err := tx.QueryRow(ctx,
			`SELECT name FROM resources WHERE id = $1`, resourceID).Scan(&resName); err != nil {
			// The resource may have been deleted mid-run; alert with the id.
			resName = resourceID
		}
		body := detail
		if body == "" {
			body = "no failure detail reported"
		}
		if err := enqueueAlertTx(ctx, tx, orgID, event, "bkr:"+runID, 0,
			action+" for "+resName, body); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// BackupRun is one run's user-visible record.
type BackupRun struct {
	ID                string     `json:"id"`
	ResourceID        string     `json:"resourceId"`
	Kind              string     `json:"kind"`
	Status            string     `json:"status"`
	SnapshotID        string     `json:"snapshotId"`
	DumpSha256        string     `json:"dumpSha256"`
	Detail            string     `json:"detail"`
	RestoreResourceID *string    `json:"restoreResourceId,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	FinishedAt        *time.Time `json:"finishedAt"`
}

// ListBackupRuns returns a resource's newest runs (backup history + fire-drill
// records), newest first.
func (s *Store) ListBackupRuns(ctx context.Context, orgID, resourceID string, limit int) ([]BackupRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id, resource_id, kind, status, snapshot_id, dump_sha256, detail, restore_resource_id, created_at, finished_at
		  FROM backup_runs WHERE org_id = $1 AND resource_id = $2
		 ORDER BY created_at DESC LIMIT $3`, orgID, resourceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackupRun{}
	for rows.Next() {
		var r BackupRun
		if err := rows.Scan(&r.ID, &r.ResourceID, &r.Kind, &r.Status, &r.SnapshotID, &r.DumpSha256, &r.Detail, &r.RestoreResourceID, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// VerifyDay is one day's restore-verify outcome for the M1 green-streak gate.
// Green iff at least one verify ran that day AND every verify that day passed;
// zero-run days are not green (SIGMA-50's predicate). Streak computation and
// display belong to P1-13 — this is only the per-day query.
type VerifyDay struct {
	Day    string `json:"day"` // YYYY-MM-DD (UTC)
	Runs   int    `json:"runs"`
	Failed int    `json:"failed"`
	Green  bool   `json:"green"`
}

// VerifyDays returns the org's last N days of verify outcomes, oldest first,
// including zero-run (not green) days.
//
// The day a run belongs to is its CREATED day, via runDayUTC — the same
// expression the scheduler and migration 0030's unique index use. It used to
// bucket on finished_at here, so any run that crossed midnight was counted on
// the wrong day: a Tuesday verify finishing 00:15 Wednesday left Tuesday reading
// zero-run (not green) while crediting Wednesday, breaking the 30-day streak on
// a day nothing was wrong and contradicting the run list (SIGMA-285). The
// status filter keeps still-open runs out, which is what finished_at was really
// providing.
func (s *Store) VerifyDays(ctx context.Context, orgID string, days int) ([]VerifyDay, error) {
	if days <= 0 || days > 366 {
		days = 30
	}
	rows, err := s.Pool.Query(ctx, `
		WITH span AS (
			SELECT generate_series(
				(now() AT TIME ZONE 'utc')::date - ($2::int - 1),
				(now() AT TIME ZONE 'utc')::date,
				'1 day')::date AS day
		)
		SELECT to_char(span.day, 'YYYY-MM-DD'),
		       COUNT(r.id) FILTER (WHERE r.status IN ('success', 'failed'))::int,
		       COUNT(r.id) FILTER (WHERE r.status = 'failed')::int
		  FROM span
		  LEFT JOIN backup_runs r
		    ON r.org_id = $1 AND r.kind = 'verify'
		   AND `+runDayUTC("r")+` = span.day
		 GROUP BY span.day ORDER BY span.day`, orgID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VerifyDay{}
	for rows.Next() {
		var d VerifyDay
		if err := rows.Scan(&d.Day, &d.Runs, &d.Failed); err != nil {
			return nil, err
		}
		d.Green = d.Runs > 0 && d.Failed == 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreateRestoreRun queues the fire-drill flow: restore the source database's
// latest snapshot into a freshly provisioned resource (created by the caller
// via the P1-10 path). The run executes on the NEW resource's server. Audited.
func (s *Store) CreateRestoreRun(ctx context.Context, orgID, sourceResourceID, newResourceID, actor string) (BackupRun, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BackupRun{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var policyID string
	var hasKey bool
	err = tx.QueryRow(ctx, `
		SELECT p.id, p.repo_key_ciphertext IS NOT NULL
		  FROM backup_policies p WHERE p.org_id = $1 AND p.resource_id = $2 AND p.target_id IS NOT NULL`,
		orgID, sourceResourceID).Scan(&policyID, &hasKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupRun{}, ErrInvalid{Msg: "source database has no backup target configured"}
	}
	if err != nil {
		return BackupRun{}, err
	}
	if !hasKey {
		return BackupRun{}, ErrInvalid{Msg: "source database has no backups yet"}
	}
	// Pin the snapshot this restore will load, HERE, at creation time.
	//
	// SIGMA-245: this used to be a bare "does any successful backup exist ever"
	// EXISTS check, and the digest the agent verifies against was resolved much
	// later by a subquery correlated to the RUN's own UTC day. That correlation
	// belongs to verify runs (SIGMA-137 — hold today's verify until today's dump
	// lands); a restore inherited it and so rendered with an empty digest on any
	// day whose scheduled backup had failed. The agent hard-refuses an empty
	// digest on a restore, but only after the CP has already provisioned and
	// billed a fresh database — so Monday's failed backup turned Sunday's
	// perfectly good snapshot into "restore refused: run carries no recorded
	// checksum", on precisely the day the operator needed it.
	//
	// Recording the newest successful backup's snapshot id and digest on the
	// restore row makes the two sides agree by construction: what the run says
	// it will load is what it will load, whatever the calendar says.
	var snapshotID, dumpSha string
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(snapshot_id, ''), COALESCE(dump_sha256, '')
		  FROM backup_runs
		 WHERE policy_id = $1 AND kind = 'backup' AND status = 'success'
		 ORDER BY finished_at DESC NULLS LAST, created_at DESC
		 LIMIT 1`, policyID).Scan(&snapshotID, &dumpSha)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupRun{}, ErrInvalid{Msg: "source database has no successful backup to restore"}
	}
	if err != nil {
		return BackupRun{}, err
	}
	if dumpSha == "" {
		// A success with no recorded digest cannot be checked against the bytes
		// that come back, and an unverifiable load into a fresh database is the
		// silent gate-skip SIGMA-78 exists to prevent. Refuse before anything is
		// provisioned, and say which half is missing.
		return BackupRun{}, ErrInvalid{Msg: "the newest successful backup recorded no checksum; it cannot be restored safely"}
	}
	var newServerID, newEngine string
	err = tx.QueryRow(ctx, `
		SELECT dc.server_id, dc.engine FROM db_credentials dc
		 WHERE dc.org_id = $1 AND dc.resource_id = $2`, orgID, newResourceID).Scan(&newServerID, &newEngine)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupRun{}, ErrInvalid{Msg: "restore target is not a database resource"}
	}
	if err != nil {
		return BackupRun{}, err
	}

	run := BackupRun{
		ID: newID("run"), ResourceID: sourceResourceID, Kind: "restore", Status: BackupRunPending,
		SnapshotID: snapshotID, DumpSha256: dumpSha, RestoreResourceID: &newResourceID,
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO backup_runs (id, org_id, resource_id, policy_id, server_id, kind, restore_resource_id,
		                         snapshot_id, dump_sha256)
		VALUES ($1, $2, $3, $4, $5, 'restore', $6, $7, $8)`,
		run.ID, orgID, sourceResourceID, policyID, newServerID, newResourceID,
		snapshotID, dumpSha); err != nil {
		return BackupRun{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "Restore queued", sourceResourceID+" -> "+newResourceID); err != nil {
		return BackupRun{}, err
	}
	return run, tx.Commit(ctx)
}

// CreateRestoreToTimestampRun queues a PITR restore (P2-5b): recover a freshly
// provisioned resource to targetTime. It validates the recoverable window
// server-side so the agent never starts a recovery that can't reach the target:
// PITR must be on, the repo key must exist, a physical base backup taken BEFORE
// the target must exist (WAL replays forward from it), and the WAL archive must
// already cover the target (last_shipped_at >= target). The run executes on the
// NEW resource's server. Audited. Postgres-only (WAL replay is a postgres path).
func (s *Store) CreateRestoreToTimestampRun(ctx context.Context, orgID, sourceResourceID, newResourceID string, targetTime time.Time, actor string) (BackupRun, error) {
	if targetTime.After(time.Now()) {
		return BackupRun{}, ErrInvalid{Msg: "recovery target time is in the future"}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return BackupRun{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var policyID string
	var pitrEnabled, hasKey bool
	err = tx.QueryRow(ctx, `
		SELECT p.id, p.pitr_enabled, p.repo_key_ciphertext IS NOT NULL
		  FROM backup_policies p WHERE p.org_id = $1 AND p.resource_id = $2 AND p.target_id IS NOT NULL`,
		orgID, sourceResourceID).Scan(&policyID, &pitrEnabled, &hasKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupRun{}, ErrInvalid{Msg: "source database has no backup target configured"}
	}
	if err != nil {
		return BackupRun{}, err
	}
	if !pitrEnabled {
		return BackupRun{}, ErrInvalid{Msg: "point-in-time recovery is not enabled on the source database"}
	}
	if !hasKey {
		return BackupRun{}, ErrInvalid{Msg: "source database has no backups yet"}
	}
	// A base backup that finished at or before the target is the replay start
	// point; without one there is nothing to roll WAL forward from.
	var hasBase bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM backup_runs
		                WHERE policy_id = $1 AND kind = 'basebackup' AND status = 'success'
		                  AND finished_at IS NOT NULL AND finished_at <= $2)`,
		policyID, targetTime).Scan(&hasBase); err != nil {
		return BackupRun{}, err
	}
	if !hasBase {
		return BackupRun{}, ErrInvalid{Msg: "no base backup was taken before the requested time"}
	}
	// The WAL archive must already extend past the target, else replay can't
	// reach it. last_shipped_at is the high-water mark the shipper reports.
	var walCovers bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM wal_archive_status
		                WHERE resource_id = $1 AND last_shipped_at IS NOT NULL AND last_shipped_at >= $2)`,
		sourceResourceID, targetTime).Scan(&walCovers); err != nil {
		return BackupRun{}, err
	}
	if !walCovers {
		return BackupRun{}, ErrInvalid{Msg: "the archived WAL does not yet cover the requested time; pick an earlier time"}
	}

	var newServerID, newEngine string
	err = tx.QueryRow(ctx, `
		SELECT dc.server_id, dc.engine FROM db_credentials dc
		 WHERE dc.org_id = $1 AND dc.resource_id = $2`, orgID, newResourceID).Scan(&newServerID, &newEngine)
	if errors.Is(err, pgx.ErrNoRows) {
		return BackupRun{}, ErrInvalid{Msg: "restore target is not a database resource"}
	}
	if err != nil {
		return BackupRun{}, err
	}
	if newEngine != "postgres" {
		return BackupRun{}, ErrInvalid{Msg: "point-in-time recovery is postgres-only"}
	}

	run := BackupRun{ID: newID("run"), ResourceID: sourceResourceID, Kind: "restore-pitr", Status: BackupRunPending, RestoreResourceID: &newResourceID}
	if _, err := tx.Exec(ctx, `
		INSERT INTO backup_runs (id, org_id, resource_id, policy_id, server_id, kind, restore_resource_id, recovery_target_time)
		VALUES ($1, $2, $3, $4, $5, 'restore-pitr', $6, $7)`,
		run.ID, orgID, sourceResourceID, policyID, newServerID, newResourceID, targetTime); err != nil {
		return BackupRun{}, err
	}
	if err := auditTx(ctx, tx, orgID, actor, "PITR restore queued",
		fmt.Sprintf("%s -> %s @ %s", sourceResourceID, newResourceID, targetTime.UTC().Format(time.RFC3339))); err != nil {
		return BackupRun{}, err
	}
	return run, tx.Commit(ctx)
}

// FailBackupRunFromOpStatus is the DSD status-ingest fallback: an op-level
// failure (policy rejection, unknown kind) lands the run in failed even if the
// agent never reached its dedicated result report.
func (s *Store) FailBackupRunFromOpStatus(ctx context.Context, serverID, runID, errText string) error {
	err := s.SetBackupRunResult(ctx, serverID, runID, false, "", "", errText)
	if errors.Is(err, ErrNotFound) {
		return nil // already terminal via the dedicated report — keep that result
	}
	return err
}

// archiveRepoKeysTx copies the wrapped restic repo key of every backup policy
// about to be cascaded away into backup_repo_key_archive, so the customer's
// offsite snapshots stay decryptable after the resource, project or environment
// that owned them is deleted (SIGMA-170).
//
// scope selects which policies to archive; it is a fragment appended to the
// WHERE clause and comes from the three call sites below, never from user input.
// Rows already archived are left alone (ON CONFLICT DO NOTHING): the first
// archive wins, and a policy id is never reused.
//
// Callers MUST run this BEFORE the DELETE that triggers the cascade — afterwards
// there is nothing left to read.
func archiveRepoKeysTx(ctx context.Context, tx pgx.Tx, orgID, scope, scopeID string) error {
	var join string
	switch scope {
	case "resource":
		join = `p.resource_id = $2`
	case "project":
		join = `r.project_id = $2`
	case "environment":
		join = `r.environment_id = $2`
	default:
		return fmt.Errorf("archiveRepoKeys: unknown scope %q", scope)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO backup_repo_key_archive
		       (policy_id, org_id, resource_id, resource_name, repo_key_ciphertext, repo_key_nonce, repo_dek_id)
		SELECT p.id, p.org_id, p.resource_id, r.name, p.repo_key_ciphertext, p.repo_key_nonce, p.repo_dek_id
		  FROM backup_policies p
		  JOIN resources r ON r.id = p.resource_id
		 WHERE p.org_id = $1 AND p.repo_key_ciphertext IS NOT NULL AND `+join+`
		ON CONFLICT (policy_id) DO NOTHING`, orgID, scopeID)
	return err
}

// ArchivedRepoKey is a retained restic repo password for a deleted resource.
type ArchivedRepoKey struct {
	PolicyID     string    `json:"policyId"`
	ResourceID   string    `json:"resourceId"`
	ResourceName string    `json:"resourceName"`
	ArchivedAt   time.Time `json:"archivedAt"`
}

// ListArchivedRepoKeys returns the org's retained repo keys — the deleted
// databases whose snapshots can still be opened. Metadata only: the password
// itself comes from ExportRepoKey, which is separately authorized and audited.
func (s *Store) ListArchivedRepoKeys(ctx context.Context, orgID string) ([]ArchivedRepoKey, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT policy_id, resource_id, resource_name, archived_at
		  FROM backup_repo_key_archive WHERE org_id = $1
		 ORDER BY archived_at DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ArchivedRepoKey{}
	for rows.Next() {
		var k ArchivedRepoKey
		if err := rows.Scan(&k.PolicyID, &k.ResourceID, &k.ResourceName, &k.ArchivedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ExportRepoKey returns the PLAINTEXT restic repository password for a
// resource — the live policy's key if the resource still exists, otherwise the
// archived one. This is the operator's escape hatch: without it a repo key is
// only ever handed to an agent for a scheduled run, so an org that lost its
// control plane (or deleted the resource) had no way to open snapshots it is
// still paying to store.
//
// Every export is audited by resource, because handing out a decryption key is
// exactly the event an auditor needs to see. Callers must gate it on Org Admin.
func (s *Store) ExportRepoKey(ctx context.Context, orgID, resourceID, actor string) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		policyID string
		ct       []byte
		nonce    []byte
		dekID    string
	)
	err = tx.QueryRow(ctx, `
		SELECT id, repo_key_ciphertext, repo_key_nonce, repo_dek_id
		  FROM backup_policies
		 WHERE org_id = $1 AND resource_id = $2 AND repo_key_ciphertext IS NOT NULL`,
		orgID, resourceID).Scan(&policyID, &ct, &nonce, &dekID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			SELECT policy_id, repo_key_ciphertext, repo_key_nonce, repo_dek_id
			  FROM backup_repo_key_archive
			 WHERE org_id = $1 AND resource_id = $2
			 ORDER BY archived_at DESC LIMIT 1`,
			orgID, resourceID).Scan(&policyID, &ct, &nonce, &dekID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	dek, err := s.dekPlaintext(ctx, tx, dekID)
	if err != nil {
		return "", err
	}
	key, err := gcmOpen(dek, repoKeyAAD(orgID, policyID), nonce, ct)
	if err != nil {
		return "", fmt.Errorf("unwrap repo key: %w", err)
	}
	if err := auditTx(ctx, tx, orgID, actor, "Backup repo key exported", resourceID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return string(key), nil
}
