package reconciler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// TestRenderDBOps pins the P1-10 database render: pinned image pull, named data
// volume, mesh-only port bind, server-type tuning, and the password as a secret
// REFERENCE (never a value) — env-mode for Postgres, file-mode (+tmpfs) for Redis.
func TestRenderDBOps(t *testing.T) {
	rs := store.ResourceSpec{ResourceID: "res_pg", ProjectID: "proj_x", Kind: "postgres", Spec: json.RawMessage(`{}`)}
	hardening := store.HostHardening{MeshIP: "10.100.0.7", ServerType: "database"}

	ops, netID, ok := renderDBOps(rs, hardening)
	if !ok || netID != "net:proj_x" {
		t.Fatalf("render failed: ok=%v net=%s", ok, netID)
	}
	byID := map[string]dsd.Op{}
	for _, op := range ops {
		byID[op.ID] = op
	}
	if img, ok := byID["img:res_pg"]; !ok || img.Kind != dsd.KindImagePull {
		t.Fatal("missing pinned image.pull")
	}
	if vol, ok := byID["vol:res_pg:data"]; !ok || vol.Kind != dsd.KindVolumeEnsure {
		t.Fatal("missing data volume.ensure")
	}
	ct, ok := byID["res:res_pg"]
	if !ok || ct.Kind != dsd.KindContainerApply {
		t.Fatal("missing container.apply")
	}

	var cs struct {
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
	_ = json.Unmarshal(ct.Spec, &cs)

	if !strings.HasPrefix(cs.Image, "postgres:") {
		t.Fatalf("image = %q, want pinned postgres", cs.Image)
	}
	// Mesh-only: the listener binds the mesh address, never 0.0.0.0.
	if len(cs.Ports) != 1 || cs.Ports[0].HostIP != "10.100.0.7" || cs.Ports[0].Host != 5432 {
		t.Fatalf("mesh-only bind wrong: %+v", cs.Ports)
	}
	// database-type server → prod tuning knobs (the acceptance inspection probe).
	if !strings.Contains(strings.Join(cs.Command, " "), "shared_buffers=1GB") {
		t.Fatalf("database profile should set shared_buffers, got %v", cs.Command)
	}
	// The password is a REFERENCE; the plaintext never appears in the DSD.
	if len(cs.SecretRefs) != 1 || cs.SecretRefs[0].Name != "POSTGRES_PASSWORD" || !cs.SecretRefs[0].EnvVar {
		t.Fatalf("secret ref wrong: %+v", cs.SecretRefs)
	}
	if strings.Contains(string(ct.Spec), "password") {
		t.Fatal("rendered spec must not carry a credential value")
	}
	// Username/database are the deterministic identity in plain env.
	if cs.Env["POSTGRES_USER"] == "" || cs.Env["POSTGRES_DB"] != "app" {
		t.Fatalf("env identity wrong: %v", cs.Env)
	}

	// A general-type server gets dev-grade defaults (no tuning args).
	ops2, _, _ := renderDBOps(rs, store.HostHardening{MeshIP: "10.100.0.7", ServerType: "general"})
	for _, op := range ops2 {
		if op.ID == "res:res_pg" {
			var cs2 struct {
				Command []string `json:"command"`
			}
			_ = json.Unmarshal(op.Spec, &cs2)
			if strings.Contains(strings.Join(cs2.Command, " "), "shared_buffers") {
				t.Fatalf("general profile must not carry prod tuning: %v", cs2.Command)
			}
		}
	}

	// No mesh IP yet → no published port at all (project-network only).
	ops3, _, _ := renderDBOps(rs, store.HostHardening{ServerType: "database"})
	for _, op := range ops3 {
		if op.ID == "res:res_pg" && strings.Contains(string(op.Spec), "hostIp") {
			t.Fatal("without a mesh IP the port must not be published")
		}
	}
}

// TestRenderDBOpsRedisFileSecret pins Redis's file-mode credential: the conf
// file reference plus the tmpfs secrets mount, and a command that waits for the
// seeded conf — no credential in the DSD.
func TestRenderDBOpsRedisFileSecret(t *testing.T) {
	rs := store.ResourceSpec{ResourceID: "res_rd", ProjectID: "proj_x", Kind: "redis", Spec: json.RawMessage(`{}`)}
	ops, _, ok := renderDBOps(rs, store.HostHardening{MeshIP: "10.100.0.7", ServerType: "general"})
	if !ok {
		t.Fatal("render failed")
	}
	for _, op := range ops {
		if op.ID != "res:res_rd" {
			continue
		}
		var cs struct {
			Tmpfs      []string `json:"tmpfs"`
			Command    []string `json:"command"`
			SecretRefs []struct {
				Name   string `json:"name"`
				EnvVar bool   `json:"envVar"`
			} `json:"secretRefs"`
		}
		_ = json.Unmarshal(op.Spec, &cs)
		if len(cs.SecretRefs) != 1 || cs.SecretRefs[0].Name != "REDIS_CONF" || cs.SecretRefs[0].EnvVar {
			t.Fatalf("redis secret ref wrong: %+v", cs.SecretRefs)
		}
		hasTmpfs := false
		for _, tp := range cs.Tmpfs {
			if tp == secretsMountDir {
				hasTmpfs = true
			}
		}
		if !hasTmpfs {
			t.Fatal("redis needs the tmpfs secrets mount for the seeded conf")
		}
		if !strings.Contains(strings.Join(cs.Command, " "), "/run/secrets/REDIS_CONF") {
			t.Fatalf("redis command must exec on the seeded conf: %v", cs.Command)
		}
		if strings.Contains(string(op.Spec), "requirepass") {
			t.Fatal("the DSD must not carry the redis credential material")
		}
		return
	}
	t.Fatal("res:res_rd container.apply missing")
}
