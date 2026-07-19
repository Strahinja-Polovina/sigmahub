package store

// SIGMA-65 S3 bucket/key CRUD + quotas + storage metering. In-dashboard bucket
// and per-bucket access-key management for a provisioned MinIO/SeaweedFS engine.
// The CP is the sole authority: a dashboard mutation records a bucket row and a
// pending_s3_ops row in one transaction; the reconciler renders open ops as
// typed s3.configure DSD ops; the agent executes and reports the terminal
// result. Per-bucket key secrets ride envelope-encrypted under the org DEK and
// are released to the executing agent only through the audited op-credential
// path — a captured DSD leaks nothing (identifiers only ride the wire).

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
)

// Bucket is the Developer-visible bucket record (never the key secret).
type Bucket struct {
	ID         string `json:"id"`
	ResourceID string `json:"resourceId"`
	Name       string `json:"name"`
	QuotaBytes int64  `json:"quotaBytes"`
	AccessKey  string `json:"accessKey"`
	Status     string `json:"status"`
}

// S3OpSpec is the reconciler's render input for one open pending s3 op. The
// endpoint + container are resolved here so the render stays a pure mapping; no
// secret ever rides this type (the agent fetches credentials per-op).
type S3OpSpec struct {
	OpID       string
	ResourceID string
	Engine     string
	Container  string
	Endpoint   string
	Action     string
	Bucket     string
	AccessKey  string
	QuotaBytes int64
}

// S3OpCredential is the per-op material the executing agent fetches (audited
// release): the root credential to authenticate against the engine, plus the
// new per-bucket secret for a create-key op (empty otherwise).
type S3OpCredential struct {
	RootAccessKey string `json:"rootAccessKey"`
	RootSecretKey string `json:"rootSecretKey"`
	NewSecretKey  string `json:"newSecretKey"`
}

// bucketNamePattern is the DNS-ish S3 bucket name rule: lowercase alphanumerics,
// dots and hyphens, starting and ending alphanumeric, 3–63 chars total.
var bucketNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// validateBucketName enforces the S3 bucket naming rules the engines share.
func validateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 || !bucketNamePattern.MatchString(name) {
		return ErrInvalid{Msg: "bucket name must be 3–63 chars, lowercase letters, digits, dots or hyphens (dns-style)"}
	}
	return nil
}

// randomBucketKeyID mints a per-bucket access key id.
func randomBucketKeyID() string {
	return "bk_" + hex.EncodeToString(randBytes(8))
}

// s3ResourceCredsTx returns an s3 resource's server id + engine, or ErrNotS3
// when the resource carries no s3_credentials row (wrong kind / wrong org).
func s3ResourceCredsTx(ctx context.Context, tx pgx.Tx, orgID, resourceID string) (serverID, engine string, err error) {
	err = tx.QueryRow(ctx,
		`SELECT server_id, engine FROM s3_credentials WHERE org_id = $1 AND resource_id = $2`,
		orgID, resourceID).Scan(&serverID, &engine)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotS3
	}
	return serverID, engine, err
}

// insertPendingS3OpTx records a pending op inside the caller's transaction.
func insertPendingS3OpTx(ctx context.Context, tx pgx.Tx, orgID, serverID, resourceID, engine, action, bucket, accessKey string, quotaBytes int64, secretCT, secretNonce []byte, secretDekID string) error {
	var dekID *string
	if secretDekID != "" {
		dekID = &secretDekID
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pending_s3_ops
			(id, org_id, server_id, resource_id, engine, action, bucket, access_key, quota_bytes,
			 new_secret_ciphertext, new_secret_nonce, new_secret_dek_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		newID("s3op"), orgID, serverID, resourceID, engine, action, bucket, accessKey, quotaBytes,
		secretCT, secretNonce, dekID, "agent:"+serverID); err != nil {
		return fmt.Errorf("insert pending s3 op: %w", err)
	}
	return nil
}

// ListBuckets returns a resource's buckets (metadata only), oldest first.
func (s *Store) ListBuckets(ctx context.Context, orgID, resourceID string) ([]Bucket, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, resource_id, name, quota_bytes, access_key, status
		  FROM s3_buckets WHERE org_id = $1 AND resource_id = $2 ORDER BY created_at, id`,
		orgID, resourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.ID, &b.ResourceID, &b.Name, &b.QuotaBytes, &b.AccessKey, &b.Status); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateBucket records a provisioning bucket + a create-bucket pending op in one
// transaction, so the reconciler renders the op on the next tick. Returns the
// bucket and its host server id (for the caller's re-render). A duplicate name
// is ErrConflict; a non-s3 resource is ErrNotS3. Audited.
func (s *Store) CreateBucket(ctx context.Context, orgID, resourceID, name, actor string) (Bucket, string, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if err := validateBucketName(name); err != nil {
		return Bucket{}, "", err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Bucket{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	serverID, engine, err := s3ResourceCredsTx(ctx, tx, orgID, resourceID)
	if err != nil {
		return Bucket{}, "", err
	}
	b := Bucket{ID: newID("s3b"), ResourceID: resourceID, Name: name, Status: "provisioning"}
	_, err = tx.Exec(ctx, `
		INSERT INTO s3_buckets (id, resource_id, org_id, name, created_by)
		VALUES ($1, $2, $3, $4, $5)`, b.ID, resourceID, orgID, name, actor)
	if isUniqueViolation(err) {
		return Bucket{}, "", fmt.Errorf("%w: a bucket named %q already exists", ErrConflict, name)
	}
	if err != nil {
		return Bucket{}, "", fmt.Errorf("insert bucket: %w", err)
	}
	if err := insertPendingS3OpTx(ctx, tx, orgID, serverID, resourceID, engine, "create-bucket", name, "", 0, nil, nil, ""); err != nil {
		return Bucket{}, "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "S3 bucket create queued", resourceID+" "+name); err != nil {
		return Bucket{}, "", err
	}
	return b, serverID, tx.Commit(ctx)
}

// DeleteBucket flips the bucket to 'deleting' and queues a delete-bucket op. The
// row is removed only once the agent confirms the delete. Audited.
func (s *Store) DeleteBucket(ctx context.Context, orgID, resourceID, name, actor string) (string, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	serverID, engine, err := s3ResourceCredsTx(ctx, tx, orgID, resourceID)
	if err != nil {
		return "", err
	}
	ct, err := tx.Exec(ctx, `
		UPDATE s3_buckets SET status = 'deleting'
		 WHERE org_id = $1 AND resource_id = $2 AND name = $3`, orgID, resourceID, name)
	if err != nil {
		return "", err
	}
	if ct.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	if err := insertPendingS3OpTx(ctx, tx, orgID, serverID, resourceID, engine, "delete-bucket", name, "", 0, nil, nil, ""); err != nil {
		return "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "S3 bucket delete queued", resourceID+" "+name); err != nil {
		return "", err
	}
	return serverID, tx.Commit(ctx)
}

// SetBucketQuota records the new quota and queues a set-quota op. Audited.
func (s *Store) SetBucketQuota(ctx context.Context, orgID, resourceID, name string, quotaBytes int64, actor string) (string, error) {
	if quotaBytes < 0 {
		return "", ErrInvalid{Msg: "quota must be non-negative"}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	serverID, engine, err := s3ResourceCredsTx(ctx, tx, orgID, resourceID)
	if err != nil {
		return "", err
	}
	ct, err := tx.Exec(ctx, `
		UPDATE s3_buckets SET quota_bytes = $4
		 WHERE org_id = $1 AND resource_id = $2 AND name = $3`, orgID, resourceID, name, quotaBytes)
	if err != nil {
		return "", err
	}
	if ct.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	if err := insertPendingS3OpTx(ctx, tx, orgID, serverID, resourceID, engine, "set-quota", name, "", quotaBytes, nil, nil, ""); err != nil {
		return "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "S3 bucket quota queued", fmt.Sprintf("%s %s = %d", resourceID, name, quotaBytes)); err != nil {
		return "", err
	}
	return serverID, tx.Commit(ctx)
}

// CreateBucketKey mints a least-privilege per-bucket access key. The secret is
// sealed under the org DEK (bound to the resource by AAD) and stored on both the
// bucket row and the create-key pending op; the plaintext is NEVER returned or
// persisted — only the access key id is returned. Audited.
func (s *Store) CreateBucketKey(ctx context.Context, orgID, resourceID, name, actor string) (accessKey string, serverID string, err error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	serverID, engine, err := s3ResourceCredsTx(ctx, tx, orgID, resourceID)
	if err != nil {
		return "", "", err
	}
	var bucketID string
	err = tx.QueryRow(ctx,
		`SELECT id FROM s3_buckets WHERE org_id = $1 AND resource_id = $2 AND name = $3 FOR UPDATE`,
		orgID, resourceID, name).Scan(&bucketID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}

	accessKey = randomBucketKeyID()
	secret := randomDBSecret()
	dekID, dek, err := s.activeDEKTx(ctx, tx, orgID)
	if err != nil {
		return "", "", err
	}
	nonce, ciphertext, err := gcmSeal(dek, s3AAD(orgID, resourceID), []byte(secret))
	if err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE s3_buckets SET access_key = $2, key_ciphertext = $3, key_nonce = $4, key_dek_id = $5
		 WHERE id = $1`, bucketID, accessKey, ciphertext, nonce, dekID); err != nil {
		return "", "", fmt.Errorf("store bucket key: %w", err)
	}
	if err := insertPendingS3OpTx(ctx, tx, orgID, serverID, resourceID, engine, "create-key", name, accessKey, 0, ciphertext, nonce, dekID); err != nil {
		return "", "", err
	}
	if err := auditTx(ctx, tx, orgID, actor, "S3 bucket key create queued", resourceID+" "+name+" "+accessKey); err != nil {
		return "", "", err
	}
	return accessKey, serverID, tx.Commit(ctx)
}

// PendingS3OpsForServer returns a server's open s3 ops the reconciler renders,
// oldest first for deterministic op ordering. Ops on a server with no mesh_ip
// yet are skipped (the endpoint is unrenderable until enrollment completes).
func (s *Store) PendingS3OpsForServer(ctx context.Context, serverID string) ([]S3OpSpec, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id, p.resource_id, c.engine, p.action, p.bucket, p.access_key, p.quota_bytes,
		       c.port, sv.mesh_ip
		  FROM pending_s3_ops p
		  JOIN s3_credentials c ON c.resource_id = p.resource_id
		  JOIN servers sv ON sv.id = p.server_id
		 WHERE p.server_id = $1 AND p.applied_at IS NULL AND p.failed_at IS NULL
		   AND sv.mesh_ip IS NOT NULL
		 ORDER BY p.created_at, p.id`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []S3OpSpec{}
	for rows.Next() {
		var op S3OpSpec
		var port int
		var meshIP string
		if err := rows.Scan(&op.OpID, &op.ResourceID, &op.Engine, &op.Action, &op.Bucket, &op.AccessKey, &op.QuotaBytes, &port, &meshIP); err != nil {
			return nil, err
		}
		op.Container = dsd.ContainerName(op.ResourceID)
		op.Endpoint = fmt.Sprintf("http://%s:%d", meshIP, port)
		out = append(out, op)
	}
	return out, rows.Err()
}

// S3OpCredentialForOp releases the root credential (and, for a create-key op,
// the new per-bucket secret) for ONE open op to the server it is scheduled on.
// BOLA-scoped: the op must belong to the caller's server and still be open.
// Audited per fetch. The op stays open until the agent posts its status.
func (s *Store) S3OpCredentialForOp(ctx context.Context, serverID, opID string) (S3OpCredential, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return S3OpCredential{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID, resourceID, action string
	var newCT, newNonce []byte
	var newDekID *string
	err = tx.QueryRow(ctx, `
		SELECT org_id, resource_id, action, new_secret_ciphertext, new_secret_nonce, new_secret_dek_id
		  FROM pending_s3_ops
		 WHERE id = $1 AND server_id = $2 AND applied_at IS NULL AND failed_at IS NULL`,
		opID, serverID).Scan(&orgID, &resourceID, &action, &newCT, &newNonce, &newDekID)
	if errors.Is(err, pgx.ErrNoRows) {
		return S3OpCredential{}, ErrNotFound
	}
	if err != nil {
		return S3OpCredential{}, err
	}

	// The engine's root credential lives on the resource's s3_credentials row.
	var accessKey, rootDekID string
	var rootCT, rootNonce []byte
	if err := tx.QueryRow(ctx, `
		SELECT access_key, dek_id, ciphertext, nonce FROM s3_credentials
		 WHERE org_id = $1 AND resource_id = $2`, orgID, resourceID).Scan(&accessKey, &rootDekID, &rootCT, &rootNonce); err != nil {
		return S3OpCredential{}, err
	}
	rootDek, err := s.dekPlaintext(ctx, tx, rootDekID)
	if err != nil {
		return S3OpCredential{}, err
	}
	rootPlain, err := gcmOpen(rootDek, s3AAD(orgID, resourceID), rootNonce, rootCT)
	if err != nil {
		return S3OpCredential{}, fmt.Errorf("decrypt s3 root credentials: %w", err)
	}
	var rootCreds s3CredentialsJSON
	if err := json.Unmarshal(rootPlain, &rootCreds); err != nil {
		return S3OpCredential{}, err
	}
	cred := S3OpCredential{RootAccessKey: accessKey, RootSecretKey: rootCreds.SecretKey}

	// A create-key op additionally carries the new per-bucket secret, sealed
	// under the same resource AAD when the op was recorded.
	if len(newCT) > 0 && newDekID != nil {
		newDek, err := s.dekPlaintext(ctx, tx, *newDekID)
		if err != nil {
			return S3OpCredential{}, err
		}
		newPlain, err := gcmOpen(newDek, s3AAD(orgID, resourceID), newNonce, newCT)
		if err != nil {
			return S3OpCredential{}, fmt.Errorf("decrypt s3 bucket secret: %w", err)
		}
		cred.NewSecretKey = string(newPlain)
	}
	if err := auditTx(ctx, tx, orgID, "agent:"+serverID, "S3 op credential unwrapped (agent)", resourceID+" op "+opID); err != nil {
		return S3OpCredential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return S3OpCredential{}, err
	}
	return cred, nil
}

// MarkS3OpApplied records an op's success and transitions its bucket per the
// action: create-bucket → 'active', delete-bucket → drop the row, create-key /
// set-quota / measure → no bucket-status change. BOLA-scoped to the op's server.
func (s *Store) MarkS3OpApplied(ctx context.Context, serverID, opID, detail string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID, resourceID, action, bucket string
	err = tx.QueryRow(ctx, `
		UPDATE pending_s3_ops SET applied_at = now(), detail = left($3, 4000)
		 WHERE id = $1 AND server_id = $2 AND applied_at IS NULL AND failed_at IS NULL
		 RETURNING org_id, resource_id, action, bucket`,
		opID, serverID, detail).Scan(&orgID, &resourceID, &action, &bucket)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	switch action {
	case "create-bucket":
		if _, err := tx.Exec(ctx, `
			UPDATE s3_buckets SET status = 'active'
			 WHERE org_id = $1 AND resource_id = $2 AND name = $3`, orgID, resourceID, bucket); err != nil {
			return err
		}
	case "delete-bucket":
		if _, err := tx.Exec(ctx, `
			DELETE FROM s3_buckets WHERE org_id = $1 AND resource_id = $2 AND name = $3`,
			orgID, resourceID, bucket); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// MarkS3OpFailed records an op's terminal failure with the agent's detail.
// BOLA-scoped to the op's server. ErrNotFound when the op is missing or already
// settled.
func (s *Store) MarkS3OpFailed(ctx context.Context, serverID, opID, detail string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var orgID, resourceID, action, bucket string
	err = tx.QueryRow(ctx, `
		UPDATE pending_s3_ops SET failed_at = now(), detail = left($3, 4000)
		 WHERE id = $1 AND server_id = $2 AND applied_at IS NULL AND failed_at IS NULL
		 RETURNING org_id, resource_id, action, bucket`,
		opID, serverID, detail).Scan(&orgID, &resourceID, &action, &bucket)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	// Unstick the bucket (SIGMA-73): a failed create leaves the row in
	// 'provisioning' forever with no retry, so surface it as 'failed' — the UI
	// can then show the error and the user can delete + recreate. A failed
	// key/quota op leaves the (already-created) bucket untouched.
	if action == "create-bucket" {
		if _, err := tx.Exec(ctx, `
			UPDATE s3_buckets SET status = 'failed'
			 WHERE org_id = $1 AND resource_id = $2 AND name = $3 AND status = 'provisioning'`,
			orgID, resourceID, bucket); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// FailS3OpFromOpStatus is the DSD status-ingest fallback: an op-level failure
// (dependency skip, unknown action) lands the op failed even if the agent never
// reached its dedicated s3-op-status report. A no-op when the op is already
// terminal (the dedicated report is authoritative).
func (s *Store) FailS3OpFromOpStatus(ctx context.Context, serverID, opID, errText string) error {
	err := s.MarkS3OpFailed(ctx, serverID, opID, errText)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// RecordStorageBytes records a measure op's result into the daily storage table
// for the op's (resource, bucket, today). Idempotent per day (upsert). The op is
// resolved BOLA-scoped by server; ErrNotFound when it does not belong here.
func (s *Store) RecordStorageBytes(ctx context.Context, serverID, opID string, bytes int64, now time.Time) error {
	// Scope the row to `action = 'measure'` so this is a genuine no-op for every
	// other applied op (create-bucket/key, set-quota, delete) — the handler can
	// call it for any applied op without gating on the byte count, which is what
	// lets a genuinely-empty (0-byte) bucket still record its daily row instead
	// of leaving a metering gap (SIGMA-81). A 0-row result therefore means "not a
	// measure op" (or an already-gone op), which is expected, not a fault.
	// Stamp `day` from the op's own created_at in UTC (SIGMA-96), NOT the agent's
	// report time: a measure scheduled late on UTC day D but reported after
	// midnight must still land on D, and the boundary must be UTC (matching the
	// sweep's dedup key `(created_at AT TIME ZONE 'UTC')::date`, migration 0030)
	// rather than the session TZ. now is retained for signature stability.
	_ = now
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO s3_storage_bytes (resource_id, org_id, bucket, day, bytes)
		SELECT resource_id, org_id, bucket,
		       (date_trunc('day', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'), $3
		  FROM pending_s3_ops WHERE id = $1 AND server_id = $2 AND action = 'measure'
		ON CONFLICT (resource_id, bucket, day) DO UPDATE SET bytes = EXCLUDED.bytes`,
		opID, serverID, bytes)
	return err
}

// SweepS3Measure enqueues (at most) one measure op per active bucket per day, so
// the daily storage table stays current for billing. Idempotent: a bucket that
// already has a measure op created today is skipped. Only buckets whose resource
// runs on a live, mesh-enrolled server are measured — the same readiness signal
// PendingS3OpsForServer uses to decide the op is renderable. Returns the ops
// enqueued.
func (s *Store) SweepS3Measure(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT b.org_id, c.server_id, b.resource_id, c.engine, b.name
		  FROM s3_buckets b
		  JOIN s3_credentials c ON c.resource_id = b.resource_id
		  JOIN servers sv ON sv.id = c.server_id
		 WHERE b.status = 'active' AND sv.mesh_ip IS NOT NULL AND sv.deleted_at IS NULL
		   AND NOT EXISTS (
			SELECT 1 FROM pending_s3_ops p
			 WHERE p.resource_id = b.resource_id AND p.bucket = b.name
			   AND p.action = 'measure' AND p.created_at >= date_trunc('day', $1::timestamptz))`,
		now.UTC())
	if err != nil {
		return 0, err
	}
	type measure struct{ orgID, serverID, resourceID, engine, bucket string }
	var work []measure
	for rows.Next() {
		var m measure
		if err := rows.Scan(&m.orgID, &m.serverID, &m.resourceID, &m.engine, &m.bucket); err != nil {
			rows.Close()
			return 0, err
		}
		work = append(work, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, m := range work {
		if _, err := tx.Exec(ctx, `
			INSERT INTO pending_s3_ops (id, org_id, server_id, resource_id, engine, action, bucket, created_by)
			VALUES ($1, $2, $3, $4, $5, 'measure', $6, 'system')
			ON CONFLICT (resource_id, bucket, ((created_at AT TIME ZONE 'UTC')::date)) WHERE action = 'measure' DO NOTHING`,
			newID("s3op"), m.orgID, m.serverID, m.resourceID, m.engine, m.bucket); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(work), nil
}
