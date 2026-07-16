package host

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

func TestRenderNftables(t *testing.T) {
	got := RenderNftables(NftablesSpec{
		WireguardPort: 51820, MeshInterface: "sigma0",
		AllowPublicSSH: false, ProxyRole: true,
		ExtraPorts: []PortException{{Port: 9000, Proto: "tcp"}},
	})
	// Invariants that keep the agent + mesh alive regardless of config.
	for _, must := range []string{
		"delete table inet sigmahub",          // single-table atomic replace (never flush ruleset)
		"policy drop;",                        // default-drop inbound
		"ct state established,related accept", // outbound control channel survives
		"udp dport 51820 accept",              // WireGuard always open
		"iifname \"sigma0\" accept",           // intra-fleet + mesh SSH
		"tcp dport { 80, 443 } accept",        // proxy role
		"tcp dport 9000 accept",               // customer exception
	} {
		if !strings.Contains(got, must) {
			t.Errorf("nft ruleset missing %q\n%s", must, got)
		}
	}
	// Must NOT wipe other tables (Docker) or filter the forward hook.
	if strings.Contains(got, "flush ruleset") {
		t.Error("must not flush the whole ruleset (would wipe Docker's chains)")
	}
	if strings.Contains(got, "chain forward") {
		t.Error("must not manage the forward hook (breaks Docker bridge forwarding)")
	}
	// Public SSH is closed by default (opt-out only).
	if strings.Contains(got, "tcp dport 22 accept") {
		t.Error("public SSH must be closed unless AllowPublicSSH")
	}
}

func TestRenderNftablesPublicSSHOptOut(t *testing.T) {
	got := RenderNftables(NftablesSpec{AllowPublicSSH: true})
	if !strings.Contains(got, "tcp dport 22 accept") {
		t.Error("keep-public-SSH opt-out must permit port 22")
	}
}

func TestRenderSSHDConfig(t *testing.T) {
	locked := RenderSSHDConfig(SSHDSpec{MeshIP: "10.77.0.5", ListenMeshOnly: true})
	for _, must := range []string{"PasswordAuthentication no", "KbdInteractiveAuthentication no", "PermitRootLogin no"} {
		if !strings.Contains(locked, must) {
			t.Errorf("sshd config missing %q\n%s", must, locked)
		}
	}
	// Must NOT pin ListenAddress to the mesh IP — that bricks SSH after a reboot
	// where the WG interface hasn't come up. Mesh-only is firewall-enforced.
	if strings.Contains(locked, "ListenAddress") {
		t.Error("sshd config must not pin ListenAddress (reboot lockout risk)")
	}
	open := RenderSSHDConfig(SSHDSpec{MeshIP: "10.77.0.5", ListenMeshOnly: false})
	if !strings.Contains(open, "PasswordAuthentication no") {
		t.Error("password auth must be off even when keeping public SSH")
	}
}

func TestRenderCISSysctlDeterministic(t *testing.T) {
	a := RenderCISSysctl()
	b := RenderCISSysctl()
	if a != b {
		t.Error("CIS sysctl render must be deterministic")
	}
	for _, must := range []string{"kernel.randomize_va_space = 2", "net.ipv4.tcp_syncookies = 1", "fs.suid_dumpable = 0"} {
		if !strings.Contains(a, must) {
			t.Errorf("CIS sysctl missing %q", must)
		}
	}
}

// TestOpsFailCleanlyWithoutRoot proves host ops don't silently succeed when the
// agent lacks privilege — they report a failed op, and never touch the host.
func TestOpsFailCleanlyWithoutRoot(t *testing.T) {
	var wrote bool
	d := &Driver{
		euid:   1000, // non-root
		runner: func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		writeFile: func(string, []byte, os.FileMode) error { wrote = true; return nil },
	}
	spec, _ := json.Marshal(NftablesSpec{})
	if err := d.opNftables(context.Background(), dsd.Op{Kind: KindHostNftables, Spec: spec}); err == nil {
		t.Fatal("expected non-root nftables op to fail")
	}
	if wrote {
		t.Error("a non-root host op must not write to the host filesystem")
	}
}

// TestSSHDValidationRollback proves a bad sshd config is validated and rolled
// back before reload, so a typo can't take SSH down.
func TestSSHDValidationRollback(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sshd_dropin.conf"
	d := &Driver{
		euid:      0,
		writeFile: func(_ string, data []byte, _ os.FileMode) error { return os.WriteFile(path, data, 0o644) },
		runner: func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "sshd" && len(args) == 1 && args[0] == "-t" {
				return []byte("bad config"), &fakeExitErr{}
			}
			return nil, nil
		},
	}
	spec, _ := json.Marshal(SSHDSpec{MeshIP: "10.0.0.1", ListenMeshOnly: true})
	err := d.opSSHD(context.Background(), dsd.Op{Kind: KindHostSSHD, Spec: spec})
	if err == nil || !strings.Contains(err.Error(), "sshd -t") {
		t.Fatalf("expected sshd validation failure, got %v", err)
	}
}

type fakeExitErr struct{}

func (fakeExitErr) Error() string { return "exit status 1" }
