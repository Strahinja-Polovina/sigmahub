package reconciler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func s3Specs() []store.ResourceSpec {
	return []store.ResourceSpec{
		{ResourceID: "res_s3", ProjectID: "proj_x", Kind: "s3", Spec: json.RawMessage(`{}`)},
	}
}

func s3Targets() map[string]store.S3Target {
	return map[string]store.S3Target{
		"res_s3": {Engine: "s3", AccessKey: "sigma", Port: 15001, ServerType: "storage"},
	}
}

// TestRenderS3FansIntoContainerOps is the P2-1 render contract: pinned MinIO
// image, data volume, mesh-bound S3 API port, access key in plain env, root
// password strictly as a secret reference.
func TestRenderS3FansIntoContainerOps(t *testing.T) {
	ops, _ := renderOps("srv_t", s3Specs(), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, nil, s3Targets(), nil, ACMEConfig{})

	if _, ok := opByID(ops, "net:proj_x"); !ok {
		t.Fatal("missing network op")
	}
	if _, ok := opByID(ops, "img:res_s3"); !ok {
		t.Fatal("missing image op")
	}
	if _, ok := opByID(ops, "vol:res_s3:data"); !ok {
		t.Fatal("missing data volume op")
	}
	ctr, ok := opByID(ops, "res:res_s3")
	if !ok {
		t.Fatal("missing container op (must keep res: id for status write-back)")
	}
	if ctr.Kind != dsd.KindContainerApply {
		t.Fatalf("container op kind = %q", ctr.Kind)
	}

	var spec struct {
		Image string            `json:"image"`
		Env   map[string]string `json:"env"`
		Ports []struct {
			Container int    `json:"container"`
			Host      int    `json:"host"`
			HostIP    string `json:"hostIp"`
		} `json:"ports"`
		Command    []string `json:"command"`
		SecretRefs []struct {
			Name   string `json:"name"`
			EnvVar bool   `json:"envVar"`
		} `json:"secretRefs"`
	}
	if err := json.Unmarshal(ctr.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(spec.Image, "minio/minio:RELEASE.") {
		t.Fatalf("image = %q, want pinned minio release", spec.Image)
	}
	if len(spec.Ports) != 1 || spec.Ports[0].HostIP != "10.8.0.5" || spec.Ports[0].Host != 15001 || spec.Ports[0].Container != 9000 {
		t.Fatalf("ports = %+v, want mesh-bound 10.8.0.5:15001->9000", spec.Ports)
	}
	if spec.Env["MINIO_ROOT_USER"] != "sigma" || spec.Env["MINIO_BROWSER"] != "off" {
		t.Fatalf("plain env = %v", spec.Env)
	}
	if len(spec.SecretRefs) != 1 || spec.SecretRefs[0].Name != "MINIO_ROOT_PASSWORD" || !spec.SecretRefs[0].EnvVar {
		t.Fatalf("secret refs = %+v", spec.SecretRefs)
	}
	if strings.Join(spec.Command, " ") != "server /data --address :9000" {
		t.Fatalf("command = %v", spec.Command)
	}
}

func TestRenderS3WithoutMeshFallsBackToStub(t *testing.T) {
	ops, _ := renderOps("srv_t", s3Specs(), nil, nil,
		store.HostHardening{}, nil, nil, nil, s3Targets(), nil, ACMEConfig{})
	op, ok := opByID(ops, "res:res_s3")
	if !ok || op.Kind != dsd.KindResourceSync {
		t.Fatalf("want resource.sync stub without mesh IP, got %+v", op)
	}
}
