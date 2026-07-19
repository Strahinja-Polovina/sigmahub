package reconciler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestRenderS3ConfigureOps is the SIGMA-65 render contract: each open s3 op
// becomes one s3.configure op with id "s3cfg:<opId>" and the exact wire fields
// the agent unmarshals — and NO secret ever appears in the rendered JSON.
func TestRenderS3ConfigureOps(t *testing.T) {
	ops := renderS3ConfigureOps([]store.S3OpSpec{
		{
			OpID: "s3op_1", ResourceID: "res_s3", Engine: "minio",
			Container: dsd.ContainerName("res_s3"), Endpoint: "http://10.8.0.5:15001",
			Action: "create-bucket", Bucket: "media",
		},
		{
			OpID: "s3op_2", ResourceID: "res_s3", Engine: "seaweedfs",
			Container: dsd.ContainerName("res_s3"), Endpoint: "http://10.8.0.6:15002",
			Action: "create-key", Bucket: "media", AccessKey: "bk_abc123",
		},
		{
			OpID: "s3op_3", ResourceID: "res_s3", Engine: "minio",
			Container: dsd.ContainerName("res_s3"), Endpoint: "http://10.8.0.5:15001",
			Action: "set-quota", Bucket: "media", QuotaBytes: 1 << 30,
		},
	})
	if len(ops) != 3 {
		t.Fatalf("got %d ops, want 3", len(ops))
	}

	create, ok := opByID(ops, "s3cfg:s3op_1")
	if !ok {
		t.Fatal("missing create-bucket op (id must be s3cfg:<opId>)")
	}
	if create.Kind != dsd.KindS3Configure {
		t.Fatalf("kind = %q, want %q", create.Kind, dsd.KindS3Configure)
	}
	if create.Kind != "s3.configure" {
		t.Fatalf("wire kind = %q, want s3.configure", create.Kind)
	}

	var spec struct {
		OpID       string `json:"opId"`
		ResourceID string `json:"resourceId"`
		Engine     string `json:"engine"`
		Container  string `json:"container"`
		Endpoint   string `json:"endpoint"`
		Action     string `json:"action"`
		Bucket     string `json:"bucket"`
		AccessKey  string `json:"accessKey"`
		QuotaBytes int64  `json:"quotaBytes"`
	}
	if err := json.Unmarshal(create.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpID != "s3op_1" || spec.ResourceID != "res_s3" || spec.Engine != "minio" ||
		spec.Container != "sigmahub-res_s3" || spec.Endpoint != "http://10.8.0.5:15001" ||
		spec.Action != "create-bucket" || spec.Bucket != "media" {
		t.Fatalf("create-bucket spec = %+v", spec)
	}

	// The create-key op carries the CP-generated access key id (not a secret).
	key, _ := opByID(ops, "s3cfg:s3op_2")
	_ = json.Unmarshal(key.Spec, &spec)
	if spec.Action != "create-key" || spec.AccessKey != "bk_abc123" || spec.Engine != "seaweedfs" {
		t.Fatalf("create-key spec = %+v", spec)
	}

	// The set-quota op carries the quota as an int64 under the exact tag.
	quota, _ := opByID(ops, "s3cfg:s3op_3")
	_ = json.Unmarshal(quota.Spec, &spec)
	if spec.Action != "set-quota" || spec.QuotaBytes != 1<<30 {
		t.Fatalf("set-quota spec = %+v", spec)
	}

	// No secret material may ride the DSD — only identifiers. Assert on the raw
	// JSON of every op.
	for _, op := range ops {
		raw := strings.ToLower(string(op.Spec))
		for _, banned := range []string{"secret", "secretkey", "password", "rootsecret"} {
			if strings.Contains(raw, banned) {
				t.Fatalf("op %s spec leaks %q: %s", op.ID, banned, op.Spec)
			}
		}
	}
}
