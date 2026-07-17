package container

import (
	"testing"
)

// TestBuildCreateBodyBindsHostIP verifies the P1-10 mesh-only contract at the
// Docker-body level: a HostIP-carrying mapping publishes on exactly that
// address, and a plain mapping keeps the default (all-interfaces) binding.
func TestBuildCreateBodyBindsHostIP(t *testing.T) {
	d := &Driver{}
	spec := ContainerSpec{
		ResourceID: "res_db",
		Name:       "sigmahub-res_db",
		Image:      "postgres:16.6",
		Network:    "sigmahub-proj",
		Ports: []PortMapping{
			{Container: 5432, Host: 15000, HostIP: "10.8.0.5"},
			{Container: 9000, Host: 9000},
		},
	}
	body := d.buildCreateBody(spec, spec.SpecHash())
	hostCfg := body["HostConfig"].(map[string]any)
	bindings := hostCfg["PortBindings"].(map[string]any)

	db := bindings["5432/tcp"].([]map[string]string)
	if len(db) != 1 || db[0]["HostIp"] != "10.8.0.5" || db[0]["HostPort"] != "15000" {
		t.Fatalf("mesh-bound port binding = %+v", db)
	}
	plain := bindings["9000/tcp"].([]map[string]string)
	if _, has := plain[0]["HostIp"]; has {
		t.Fatalf("plain mapping must not carry HostIp: %+v", plain)
	}
}
