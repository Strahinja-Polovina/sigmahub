package integration

// P2-1 S3 storage integration: provisioning inside CreateResource (mesh port
// shared with the DB space, credentials under the org DEK), the info/reveal
// split with its audit, and the root password riding the agent's audited
// secret-resolve channel.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func TestS3ProvisioningEndToEnd(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_s3"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "store-1", "storage", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "store-1", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID := reg.Server.ID
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "test"); err != nil {
		t.Fatal(err)
	}

	// The availability matrix holds: s3 only lands on storage servers, and a
	// storage server refuses database kinds.
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "pg", Kind: "postgres", Spec: json.RawMessage(`{}`),
	}, "test"); err == nil {
		t.Fatal("postgres on a storage server must be refused")
	}

	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "media", Kind: "s3", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	// Provisioned in the same tx: port from the shared mesh range + creds row.
	info, err := st.GetS3Info(ctx, orgID, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Port < 15000 || info.AccessKey != "sigma" || !info.MeshOnly {
		t.Fatalf("info = %+v", info)
	}
	// Registration allocated the mesh address; the endpoint is derived from it.
	if info.Host == "" || info.Endpoint != "http://"+info.Host+":15000" {
		t.Fatalf("endpoint = %q (host %q), want mesh URL", info.Endpoint, info.Host)
	}
	if !strings.HasPrefix(info.Image, "minio/minio:RELEASE.") {
		t.Fatalf("image = %q", info.Image)
	}
	// A second S3 resource gets the next port in the same space.
	res2, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "backups", Kind: "s3", Spec: json.RawMessage(`{}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	info2, err := st.GetS3Info(ctx, orgID, res2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info2.Port != info.Port+1 {
		t.Fatalf("second port = %d, want %d", info2.Port, info.Port+1)
	}

	// Reveal decrypts the DEK-enveloped secret and audits; cross-org 404s.
	conn, err := st.RevealS3Connection(ctx, orgID, res.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if len(conn.SecretKey) != 32 {
		t.Fatalf("secret key = %q", conn.SecretKey)
	}
	if _, err := st.RevealS3Connection(ctx, "org_other", res.ID, "admin"); err != store.ErrNotS3 {
		t.Fatalf("cross-org reveal err = %v, want ErrNotS3", err)
	}
	var audits int
	if err := st.Pool.QueryRow(ctx, `
		SELECT count(*) FROM cp_audit_log WHERE org_id = $1 AND action = 'S3 credentials revealed'`,
		orgID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("reveal audits = %d, want 1", audits)
	}

	// The root password rides the agent secret-resolve channel as an env var.
	resolved, err := st.ResolveSecretsForResource(ctx, orgID, serverID, res.ID, "agent:"+serverID)
	if err != nil {
		t.Fatal(err)
	}
	var foundRoot bool
	for _, sec := range resolved {
		if sec.Name == "MINIO_ROOT_PASSWORD" {
			foundRoot = true
			if sec.Value != conn.SecretKey || !sec.EnvVar {
				t.Fatalf("resolved root = %+v, want env var matching the revealed key", sec)
			}
		}
	}
	if !foundRoot {
		t.Fatal("MINIO_ROOT_PASSWORD missing from the resolve channel")
	}

	// The reconciler target feed carries no secret; the default engine is MinIO.
	targets, err := st.S3TargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	target, ok := targets[res.ID]
	if !ok || target.Port != info.Port || target.AccessKey != "sigma" || target.ServerType != "storage" || target.Engine != "minio" {
		t.Fatalf("targets = %+v", targets)
	}
	if info.Engine != "minio" {
		t.Fatalf("default engine = %q, want minio", info.Engine)
	}
}

// TestS3SeaweedFSEngine is the P2-2 parity: the same `s3` kind provisions
// SeaweedFS when the spec selects it, the secret rides the SeaweedFS env var,
// and the CP_S3_ENGINES allowlist gates it exactly like CP_DB_ENGINES.
func TestS3SeaweedFSEngine(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	orgID := "org_sw"

	proj, err := st.CreateProject(ctx, orgID, "p", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	env, err := st.CreateEnvironment(ctx, orgID, proj.ID, "prod", true, "test")
	if err != nil {
		t.Fatal(err)
	}
	bootTok, _, _, err := st.IssueBootstrapToken(ctx, orgID, "store-1", "storage", "", "", "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := st.RegisterServer(ctx, bootTok, "store-1", "0.1.0", json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatal(err)
	}
	serverID := reg.Server.ID
	if err := st.AttachServer(ctx, orgID, env.ID, serverID, "test"); err != nil {
		t.Fatal(err)
	}

	// An unknown engine fails create loudly rather than provisioning nothing.
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "bad", Kind: "s3",
		Spec: json.RawMessage(`{"engine":"garbage"}`),
	}, "test"); err == nil {
		t.Fatal("unknown s3 engine must be refused")
	}

	// Allowlist gate: with only MinIO enabled, SeaweedFS creation is refused.
	st.SetEnabledS3Engines([]string{"minio"})
	if _, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "sw-off", Kind: "s3",
		Spec: json.RawMessage(`{"engine":"seaweedfs"}`),
	}, "test"); err == nil {
		t.Fatal("disabled s3 engine must be refused")
	}
	st.SetEnabledS3Engines([]string{"minio", "seaweedfs"})

	res, err := st.CreateResource(ctx, orgID, store.CreateResourceInput{
		EnvironmentID: env.ID, ServerID: serverID, Name: "media", Kind: "s3",
		Spec: json.RawMessage(`{"engine":"seaweedfs"}`),
	}, "test")
	if err != nil {
		t.Fatal(err)
	}

	info, err := st.GetS3Info(ctx, orgID, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Engine != "seaweedfs" {
		t.Fatalf("engine = %q, want seaweedfs", info.Engine)
	}
	if !strings.HasPrefix(info.Image, "chrislusf/seaweedfs:") || !strings.Contains(info.Image, "@sha256:") {
		t.Fatalf("image = %q, want digest-pinned seaweedfs", info.Image)
	}
	// The SeaweedFS S3 port shares the same mesh range as MinIO/databases.
	if info.Port < 15000 || info.AccessKey != "sigma" || !info.MeshOnly {
		t.Fatalf("info = %+v", info)
	}

	// The root secret rides the SeaweedFS env var, not MinIO's.
	conn, err := st.RevealS3Connection(ctx, orgID, res.ID, "admin")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := st.ResolveSecretsForResource(ctx, orgID, serverID, res.ID, "agent:"+serverID)
	if err != nil {
		t.Fatal(err)
	}
	var foundSecret bool
	for _, sec := range resolved {
		if sec.Name == "MINIO_ROOT_PASSWORD" {
			t.Fatal("SeaweedFS resource must not carry a MINIO_ROOT_PASSWORD secret")
		}
		if sec.Name == "AWS_SECRET_ACCESS_KEY" {
			foundSecret = true
			if sec.Value != conn.SecretKey || !sec.EnvVar {
				t.Fatalf("resolved secret = %+v, want env var matching the revealed key", sec)
			}
		}
	}
	if !foundSecret {
		t.Fatal("AWS_SECRET_ACCESS_KEY missing from the resolve channel")
	}

	// The reconciler target records the SeaweedFS engine so render picks it.
	targets, err := st.S3TargetsForServer(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	if targets[res.ID].Engine != "seaweedfs" {
		t.Fatalf("target engine = %q, want seaweedfs", targets[res.ID].Engine)
	}
}
