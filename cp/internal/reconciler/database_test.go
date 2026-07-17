package reconciler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

func dbSpecs(kind string) []store.ResourceSpec {
	return []store.ResourceSpec{
		{ResourceID: "res_db", ProjectID: "proj_x", Kind: kind, Spec: json.RawMessage(`{}`)},
	}
}

func dbTargets(engine string, serverType string) map[string]store.DBTarget {
	return map[string]store.DBTarget{
		"res_db": {Engine: engine, Username: "sigma", Database: "shop", Port: 15000, ServerType: serverType},
	}
}

func TestRenderDatabaseFansIntoContainerOps(t *testing.T) {
	ops, _ := renderOps("srv_t", dbSpecs("postgres"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets("postgres", "database"), nil, nil, ACMEConfig{})

	if _, ok := opByID(ops, "net:proj_x"); !ok {
		t.Fatal("missing network op")
	}
	if _, ok := opByID(ops, "img:res_db"); !ok {
		t.Fatal("missing image op")
	}
	if _, ok := opByID(ops, "vol:res_db:data"); !ok {
		t.Fatal("missing data volume op")
	}
	ctr, ok := opByID(ops, "res:res_db")
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
	if spec.Image != "postgres:16.6" {
		t.Fatalf("image = %q, want pinned postgres", spec.Image)
	}
	// Mesh-only exposure: the host binding must be pinned to the mesh address.
	if len(spec.Ports) != 1 || spec.Ports[0].HostIP != "10.8.0.5" || spec.Ports[0].Host != 15000 || spec.Ports[0].Container != 5432 {
		t.Fatalf("ports = %+v, want mesh-bound 10.8.0.5:15000->5432", spec.Ports)
	}
	// Credentials ride as references only; the plain env carries identifiers.
	if spec.Env["POSTGRES_USER"] != "sigma" || spec.Env["POSTGRES_DB"] != "shop" {
		t.Fatalf("plain env = %v", spec.Env)
	}
	if len(spec.SecretRefs) != 1 || spec.SecretRefs[0].Name != "POSTGRES_PASSWORD" || !spec.SecretRefs[0].EnvVar {
		t.Fatalf("secret refs = %+v", spec.SecretRefs)
	}
	// database-type server gets the production tuning profile.
	joined := strings.Join(spec.Command, " ")
	if !strings.Contains(joined, "shared_buffers=512MB") {
		t.Fatalf("command %q missing prod tuning", joined)
	}
}

func TestRenderDatabaseTuningFollowsServerType(t *testing.T) {
	ops, _ := renderOps("srv_t", dbSpecs("postgres"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets("postgres", "general"), nil, nil, ACMEConfig{})
	ctr, _ := opByID(ops, "res:res_db")
	if !strings.Contains(string(ctr.Spec), "shared_buffers=128MB") {
		t.Fatalf("general server must get dev-grade tuning, got %s", ctr.Spec)
	}
}

func TestRenderDatabaseWithoutMeshFallsBackToStub(t *testing.T) {
	// Before mesh enrollment there is no address to bind to; the resource must
	// stay a no-op stub rather than publish on an undefined interface.
	ops, _ := renderOps("srv_t", dbSpecs("postgres"), nil, nil,
		store.HostHardening{}, nil, nil, dbTargets("postgres", "database"), nil, nil, ACMEConfig{})
	op, ok := opByID(ops, "res:res_db")
	if !ok || op.Kind != dsd.KindResourceSync {
		t.Fatalf("want resource.sync stub without mesh IP, got %+v", op)
	}
}

func TestRenderDatabaseDSDCarriesNoSecret(t *testing.T) {
	// The DSD is signed but not encrypted: a captured document must never
	// contain a credential value for any engine.
	for _, engine := range []string{"postgres", "mysql", "redis", "mongodb"} {
		ops, _ := renderOps("srv_t", dbSpecs(engine), nil, nil,
			store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets(engine, "database"), nil, nil, ACMEConfig{})
		raw, _ := json.Marshal(ops)
		if strings.Contains(string(raw), "password=") || strings.Contains(string(raw), `"password"`) {
			t.Fatalf("%s DSD leaks a password: %s", engine, raw)
		}
	}
}

func TestRenderPostgresPITRAddsWALArchiving(t *testing.T) {
	target := dbTargets("postgres", "database")
	t0 := target["res_db"]
	t0.PITR = true
	target["res_db"] = t0
	ops, _ := renderOps("srv_t", dbSpecs("postgres"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, target, nil, nil, ACMEConfig{})

	// A dedicated WAL spool volume is ensured and mounted.
	if _, ok := opByID(ops, "vol:res_db:wal"); !ok {
		t.Fatal("PITR postgres must ensure a wal spool volume")
	}
	ctr, ok := opByID(ops, "res:res_db")
	if !ok {
		t.Fatal("missing container op")
	}
	var spec struct {
		Command []string      `json:"command"`
		Volumes []struct{ MountPath string `json:"mountPath"` } `json:"volumes"`
	}
	if err := json.Unmarshal(ctr.Spec, &spec); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(spec.Command, " ")
	if !strings.Contains(joined, "archive_mode=on") || !strings.Contains(joined, "wal_level=replica") {
		t.Fatalf("WAL archiving flags missing: %q", joined)
	}
	// The archive_command writes tmp+rename so a half-written segment never ships.
	if !strings.Contains(joined, ".tmp && mv") {
		t.Fatalf("archive_command must be atomic (tmp+rename): %q", joined)
	}
	var mounts []string
	for _, v := range spec.Volumes {
		mounts = append(mounts, v.MountPath)
	}
	if !strings.Contains(strings.Join(mounts, " "), "/var/lib/postgresql/wal-archive") {
		t.Fatalf("spool mount missing: %v", mounts)
	}
}

func TestRenderPostgresWithoutPITRHasNoWAL(t *testing.T) {
	ops, _ := renderOps("srv_t", dbSpecs("postgres"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets("postgres", "database"), nil, nil, ACMEConfig{})
	if _, ok := opByID(ops, "vol:res_db:wal"); ok {
		t.Fatal("PITR-off postgres must not ensure a wal volume")
	}
	ctr, _ := opByID(ops, "res:res_db")
	if strings.Contains(string(ctr.Spec), "archive_mode") {
		t.Fatalf("PITR-off postgres must not carry archive flags: %s", ctr.Spec)
	}
}

func TestRenderMySQLIncludesRootPasswordRef(t *testing.T) {
	ops, _ := renderOps("srv_t", dbSpecs("mysql"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets("mysql", "database"), nil, nil, ACMEConfig{})
	ctr, _ := opByID(ops, "res:res_db")
	s := string(ctr.Spec)
	if !strings.Contains(s, "MYSQL_PASSWORD") || !strings.Contains(s, "MYSQL_ROOT_PASSWORD") {
		t.Fatalf("mysql spec missing credential refs: %s", s)
	}
}

func TestRenderRedisTakesPasswordFromEnvNotArgs(t *testing.T) {
	ops, _ := renderOps("srv_t", dbSpecs("redis"), nil, nil,
		store.HostHardening{MeshIP: "10.8.0.5"}, nil, nil, dbTargets("redis", "general"), nil, nil, ACMEConfig{})
	ctr, _ := opByID(ops, "res:res_db")
	var spec struct {
		Command []string `json:"command"`
	}
	_ = json.Unmarshal(ctr.Spec, &spec)
	joined := strings.Join(spec.Command, " ")
	// The command must reference the injected env var, never a literal value.
	if !strings.Contains(joined, `--requirepass "$REDIS_PASSWORD"`) {
		t.Fatalf("redis command must take the password from env: %q", joined)
	}
}
