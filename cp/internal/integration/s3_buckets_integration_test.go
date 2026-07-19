package integration

// SIGMA-65 S3 bucket/key CRUD + quotas + storage metering integration: the
// dashboard mutation records a bucket + a pending op; the reconciler feed
// (PendingS3OpsForServer) renders it with the mesh endpoint + engine; the
// audited op-credential release hands the agent the root secret (and, for a
// key op, a fresh per-bucket secret); the terminal status flips the bucket
// active; and a measure result lands in the daily storage table.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestS3BucketsEndToEnd(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_s3buckets"
	envID, serverID := dbTestFixture(t, st, orgID, true, "storage")

	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: envID, ServerID: serverID, Name: "media", Kind: "s3", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatalf("create s3 resource: %v", err)
	}
	info, err := st.GetS3Info(ctx, orgID, res.ID)
	if err != nil {
		t.Fatal(err)
	}

	// ── CreateBucket → a create-bucket pending op renders for the server ──────
	bucket, bServerID, err := st.CreateBucket(ctx, orgID, res.ID, "photos", "admin")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	if bServerID != serverID || bucket.Status != "provisioning" || bucket.Name != "photos" {
		t.Fatalf("bucket = %+v (server %s)", bucket, bServerID)
	}
	// A duplicate name is a conflict, not a second row.
	if _, _, err := st.CreateBucket(ctx, orgID, res.ID, "photos", "admin"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate bucket err = %v, want conflict", err)
	}
	// A bad (non-dns) name is rejected.
	if _, _, err := st.CreateBucket(ctx, orgID, res.ID, "AB", "admin"); err == nil {
		t.Fatal("invalid bucket name must be rejected")
	}

	ops, err := st.PendingS3OpsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	createOp, ok := findOp(ops, "create-bucket", "photos")
	if !ok {
		t.Fatalf("no create-bucket op in %+v", ops)
	}
	if createOp.Engine != "minio" || createOp.Container != "sigmahub-"+res.ID {
		t.Fatalf("op render = %+v, want minio + sigmahub-<res> container", createOp)
	}
	wantEndpoint := "http://" + info.Host + ":" + itoa(info.Port)
	if createOp.Endpoint != wantEndpoint {
		t.Fatalf("op endpoint = %q, want %q", createOp.Endpoint, wantEndpoint)
	}

	// ── S3OpCredentialForOp releases the root secret (audited), BOLA-scoped ───
	conn, err := st.RevealS3Connection(ctx, orgID, res.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := st.S3OpCredentialForOp(ctx, serverID, createOp.OpID)
	if err != nil {
		t.Fatalf("op credential: %v", err)
	}
	if cred.RootAccessKey != "sigma" || cred.RootSecretKey != conn.SecretKey {
		t.Fatalf("cred = %+v, want root sigma/%s", cred, conn.SecretKey)
	}
	if cred.NewSecretKey != "" {
		t.Fatalf("create-bucket op must carry no new secret, got %q", cred.NewSecretKey)
	}
	// A different server may not fetch this op's credential.
	if _, err := st.S3OpCredentialForOp(ctx, "srv_other", createOp.OpID); err != store.ErrNotFound {
		t.Fatalf("cross-server credential err = %v, want ErrNotFound", err)
	}
	var credAudits int
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(*) FROM cp_audit_log WHERE org_id = $1 AND action = 'S3 op credential unwrapped (agent)'`,
		orgID).Scan(&credAudits); err != nil {
		t.Fatal(err)
	}
	if credAudits != 1 {
		t.Fatalf("credential audits = %d, want 1", credAudits)
	}

	// ── MarkS3OpApplied flips the bucket to active ───────────────────────────
	if err := st.MarkS3OpApplied(ctx, serverID, createOp.OpID, "bucket created"); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	buckets, err := st.ListBuckets(ctx, orgID, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || buckets[0].Status != "active" {
		t.Fatalf("buckets after apply = %+v, want one active", buckets)
	}
	// The op left the open set.
	if remaining, _ := st.PendingS3OpsForServer(ctx, serverID); len(remaining) != 0 {
		t.Fatalf("open ops after apply = %+v, want none", remaining)
	}

	// ── CreateBucketKey stores an access key + releases a fresh per-bucket secret
	accessKey, kServerID, err := st.CreateBucketKey(ctx, orgID, res.ID, "photos", "admin")
	if err != nil {
		t.Fatalf("create bucket key: %v", err)
	}
	if kServerID != serverID || !strings.HasPrefix(accessKey, "bk_") {
		t.Fatalf("key = %q (server %s), want bk_ prefix", accessKey, kServerID)
	}
	buckets, _ = st.ListBuckets(ctx, orgID, res.ID)
	if buckets[0].AccessKey != accessKey {
		t.Fatalf("bucket access key = %q, want %q", buckets[0].AccessKey, accessKey)
	}
	keyOps, _ := st.PendingS3OpsForServer(ctx, serverID)
	keyOp, ok := findOp(keyOps, "create-key", "photos")
	if !ok {
		t.Fatalf("no create-key op in %+v", keyOps)
	}
	if keyOp.AccessKey != accessKey {
		t.Fatalf("create-key op access key = %q, want %q", keyOp.AccessKey, accessKey)
	}
	keyCred, err := st.S3OpCredentialForOp(ctx, serverID, keyOp.OpID)
	if err != nil {
		t.Fatalf("key op credential: %v", err)
	}
	if keyCred.NewSecretKey == "" || keyCred.NewSecretKey == keyCred.RootSecretKey {
		t.Fatalf("create-key cred = %+v, want a distinct non-empty new secret", keyCred)
	}
	if err := st.MarkS3OpApplied(ctx, serverID, keyOp.OpID, "key created"); err != nil {
		t.Fatal(err)
	}

	// ── SetBucketQuota queues a set-quota op ─────────────────────────────────
	if _, err := st.SetBucketQuota(ctx, orgID, res.ID, "photos", 5<<30, "admin"); err != nil {
		t.Fatalf("set quota: %v", err)
	}
	qOps, _ := st.PendingS3OpsForServer(ctx, serverID)
	quotaOp, ok := findOp(qOps, "set-quota", "photos")
	if !ok || quotaOp.QuotaBytes != 5<<30 {
		t.Fatalf("set-quota op = %+v, want quota 5GiB", quotaOp)
	}
	if err := st.MarkS3OpApplied(ctx, serverID, quotaOp.OpID, "quota set"); err != nil {
		t.Fatal(err)
	}

	// ── SweepS3Measure enqueues a measure op; RecordStorageBytes lands the bytes
	n, err := st.SweepS3Measure(ctx, time.Now())
	if err != nil {
		t.Fatalf("sweep measure: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep enqueued %d measure ops, want 1", n)
	}
	// Idempotent: a second sweep the same day enqueues nothing.
	if n2, _ := st.SweepS3Measure(ctx, time.Now()); n2 != 0 {
		t.Fatalf("second sweep enqueued %d, want 0 (idempotent)", n2)
	}
	measureOps, _ := st.PendingS3OpsForServer(ctx, serverID)
	measureOp, ok := findOp(measureOps, "measure", "photos")
	if !ok {
		t.Fatalf("no measure op in %+v", measureOps)
	}
	const measured = int64(1234567)
	now := time.Now()
	if err := st.RecordStorageBytes(ctx, serverID, measureOp.OpID, measured, now); err != nil {
		t.Fatalf("record storage bytes: %v", err)
	}
	if err := st.MarkS3OpApplied(ctx, serverID, measureOp.OpID, "measured"); err != nil {
		t.Fatal(err)
	}
	var gotBytes int64
	if err := st.Pool.QueryRow(ctx, `
		SELECT bytes FROM s3_storage_bytes
		 WHERE resource_id = $1 AND bucket = $2 AND day = date_trunc('day', $3::timestamptz)`,
		res.ID, "photos", now.UTC()).Scan(&gotBytes); err != nil {
		t.Fatalf("query storage bytes: %v", err)
	}
	if gotBytes != measured {
		t.Fatalf("recorded bytes = %d, want %d", gotBytes, measured)
	}

	// ── DeleteBucket → delete op → applied removes the row ───────────────────
	if _, err := st.DeleteBucket(ctx, orgID, res.ID, "photos", "admin"); err != nil {
		t.Fatalf("delete bucket: %v", err)
	}
	buckets, _ = st.ListBuckets(ctx, orgID, res.ID)
	if len(buckets) != 1 || buckets[0].Status != "deleting" {
		t.Fatalf("bucket after delete-queue = %+v, want deleting", buckets)
	}
	delOps, _ := st.PendingS3OpsForServer(ctx, serverID)
	delOp, ok := findOp(delOps, "delete-bucket", "photos")
	if !ok {
		t.Fatalf("no delete-bucket op in %+v", delOps)
	}
	if err := st.MarkS3OpApplied(ctx, serverID, delOp.OpID, "bucket deleted"); err != nil {
		t.Fatal(err)
	}
	if buckets, _ = st.ListBuckets(ctx, orgID, res.ID); len(buckets) != 0 {
		t.Fatalf("buckets after delete-apply = %+v, want none", buckets)
	}
}

// findOp returns the first op matching an action + bucket.
func findOp(ops []store.S3OpSpec, action, bucket string) (store.S3OpSpec, bool) {
	for _, op := range ops {
		if op.Action == action && op.Bucket == bucket {
			return op, true
		}
	}
	return store.S3OpSpec{}, false
}

func itoa(i int) string {
	return strconv.Itoa(i)
}
