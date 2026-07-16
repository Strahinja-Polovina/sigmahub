// Package host applies the P1-5 opt-out hardening pass — nftables firewall, SSH
// lockdown, and CIS Level-1 controls — as typed DSD ops, the ONLY channel for
// post-enrollment host changes (no parallel side channel). Every mutation is
// idempotent so a resync re-applies cleanly, and the firewall/sshd rules are
// generated so they can never sever the agent's own outbound channel or the
// WireGuard mesh.
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
)

// Op kinds — MUST match the control plane's dsd.KindHost* strings byte-for-byte
// (the two modules can't share Go types, so the wire names are duplicated).
const (
	KindHostNftables = "host.nftables"
	KindHostSSHD     = "host.sshd"
	KindHostCIS      = "host.cis"
)

// PortException is a customer-declared inbound allowance.
type PortException struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"` // tcp|udp
}

// NftablesSpec is the declarative firewall: default-drop inbound, always allow
// the WireGuard mesh + the loopback + established/related + the mesh interface,
// conditionally allow public SSH (opt-out) and the proxy ports, plus any
// customer exceptions.
type NftablesSpec struct {
	WireguardPort  int             `json:"wireguardPort"` // usually 51820
	MeshInterface  string          `json:"meshInterface"` // e.g. sigma0
	AllowPublicSSH bool            `json:"allowPublicSSH"`
	ProxyRole      bool            `json:"proxyRole"` // opens 80/443
	ExtraPorts     []PortException `json:"extraPorts,omitempty"`
}

// SSHDSpec is the post-enrollment SSH lockdown. By default password + root login
// are off and sshd binds to the mesh IP only; keeping public SSH is the opt-out.
type SSHDSpec struct {
	MeshIP        string `json:"meshIp"`
	ListenMeshOnly bool  `json:"listenMeshOnly"`
}

// CISSpec selects the CIS benchmark level (only 1 in Phase 1).
type CISSpec struct {
	Level int `json:"level"`
}

// Driver applies host-hardening ops. runner is swapped in tests; euid gates the
// real host mutations so a non-root agent fails cleanly rather than silently.
type Driver struct {
	runner func(ctx context.Context, name string, args ...string) ([]byte, error)
	writeFile func(path string, data []byte, perm os.FileMode) error
	euid   int
}

// NewDriver builds a host driver bound to the real OS.
func NewDriver() *Driver {
	return &Driver{
		runner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		writeFile: os.WriteFile,
		euid:      os.Geteuid(),
	}
}

// Register wires the host op kinds into the apply registry. Like the container
// driver's Register, this is the single place these host capabilities come into
// existence — an unregistered kind is rejected by Apply.
func (d *Driver) Register(r *apply.Registry) {
	r.Register(KindHostNftables, d.opNftables)
	r.Register(KindHostSSHD, d.opSSHD)
	r.Register(KindHostCIS, d.opCIS)
}

const (
	nftRulesetPath = "/etc/sigmahub/nftables.conf"
	sshdDropinPath = "/etc/ssh/sshd_config.d/50-sigmahub.conf"
	cisSysctlPath  = "/etc/sysctl.d/60-sigmahub-cis.conf"
)

func (d *Driver) requireRoot() error {
	if d.euid != 0 {
		return fmt.Errorf("host hardening requires root (euid=%d); install sigmad as a root service", d.euid)
	}
	return nil
}

func (d *Driver) opNftables(ctx context.Context, op dsd.Op) error {
	var spec NftablesSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode nftables spec: %w", err)
	}
	if err := d.requireRoot(); err != nil {
		return err
	}
	ruleset := RenderNftables(spec)
	if err := d.writeFile(nftRulesetPath, []byte(ruleset), 0o600); err != nil {
		return fmt.Errorf("write nft ruleset: %w", err)
	}
	// `nft -f` is atomic: the whole ruleset loads or nothing changes, so a bad
	// rule can't leave the host half-firewalled.
	if out, err := d.runner(ctx, "nft", "-f", nftRulesetPath); err != nil {
		return fmt.Errorf("nft -f: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (d *Driver) opSSHD(ctx context.Context, op dsd.Op) error {
	var spec SSHDSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode sshd spec: %w", err)
	}
	if err := d.requireRoot(); err != nil {
		return err
	}
	conf := RenderSSHDConfig(spec)
	if err := d.writeFile(sshdDropinPath, []byte(conf), 0o644); err != nil {
		return fmt.Errorf("write sshd dropin: %w", err)
	}
	// Validate BEFORE reload: a syntactically bad sshd_config that reloaded would
	// take the SSH daemon down. sshd -t returns non-zero on any error.
	if out, err := d.runner(ctx, "sshd", "-t"); err != nil {
		_ = os.Remove(sshdDropinPath) // roll back the bad drop-in
		return fmt.Errorf("sshd -t rejected config: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := d.runner(ctx, "systemctl", "reload", "ssh"); err != nil {
		// Some distros name the unit sshd; try that before giving up.
		if out2, err2 := d.runner(ctx, "systemctl", "reload", "sshd"); err2 != nil {
			return fmt.Errorf("reload ssh: %w: %s / %s", err, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	return nil
}

func (d *Driver) opCIS(ctx context.Context, op dsd.Op) error {
	var spec CISSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode cis spec: %w", err)
	}
	if err := d.requireRoot(); err != nil {
		return err
	}
	if err := d.writeFile(cisSysctlPath, []byte(RenderCISSysctl()), 0o644); err != nil {
		return fmt.Errorf("write cis sysctl: %w", err)
	}
	if out, err := d.runner(ctx, "sysctl", "--system"); err != nil {
		return fmt.Errorf("sysctl --system: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// auditd is a CIS L1 control; enable + start idempotently (already-enabled is
	// a no-op that returns 0).
	if out, err := d.runner(ctx, "systemctl", "enable", "--now", "auditd"); err != nil {
		return fmt.Errorf("enable auditd: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RenderNftables produces a complete, atomic nft ruleset. The generation itself
// is the safety guard: output policy is accept and established/related are always
// accepted, so the agent's OUTBOUND control-plane channel is never severed; the
// WireGuard port and mesh interface are always open so the fleet stays meshed.
func RenderNftables(spec NftablesSpec) string {
	wg := spec.WireguardPort
	if wg == 0 {
		wg = 51820
	}
	mesh := spec.MeshInterface
	if mesh == "" {
		mesh = "sigma0"
	}
	var b strings.Builder
	b.WriteString("#!/usr/sbin/nft -f\n")
	b.WriteString("# Managed by sigmahub (host.nftables). Do not edit by hand.\n")
	b.WriteString("flush ruleset\n\n")
	b.WriteString("table inet sigmahub {\n")
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority 0; policy drop;\n")
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\tct state invalid drop\n")
	b.WriteString("\t\tiif \"lo\" accept\n")
	b.WriteString("\t\tip protocol icmp accept\n")
	b.WriteString("\t\tip6 nexthdr ipv6-icmp accept\n")
	fmt.Fprintf(&b, "\t\tudp dport %d accept\n", wg) // WireGuard mesh — always
	fmt.Fprintf(&b, "\t\tiifname \"%s\" accept\n", mesh) // intra-fleet over the mesh
	if spec.AllowPublicSSH {
		b.WriteString("\t\ttcp dport 22 accept\n")
	}
	if spec.ProxyRole {
		b.WriteString("\t\ttcp dport { 80, 443 } accept\n")
	}
	for _, p := range sortedPorts(spec.ExtraPorts) {
		proto := p.Proto
		if proto != "udp" {
			proto = "tcp"
		}
		fmt.Fprintf(&b, "\t\t%s dport %d accept\n", proto, p.Port)
	}
	b.WriteString("\t}\n")
	b.WriteString("\tchain forward {\n\t\ttype filter hook forward priority 0; policy drop;\n\t}\n")
	b.WriteString("\tchain output {\n\t\ttype filter hook output priority 0; policy accept;\n\t}\n")
	b.WriteString("}\n")
	return b.String()
}

func sortedPorts(ports []PortException) []PortException {
	out := append([]PortException(nil), ports...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Port != out[j].Port {
			return out[i].Port < out[j].Port
		}
		return out[i].Proto < out[j].Proto
	})
	return out
}

// RenderSSHDConfig produces the sshd drop-in. Password + root login are always
// off (SIGMA-A-5); binding to the mesh IP is the default, and keeping public SSH
// (ListenMeshOnly false) is the explicit opt-out.
func RenderSSHDConfig(spec SSHDSpec) string {
	var b strings.Builder
	b.WriteString("# Managed by sigmahub (host.sshd). Do not edit by hand.\n")
	b.WriteString("PasswordAuthentication no\n")
	b.WriteString("KbdInteractiveAuthentication no\n")
	b.WriteString("PermitRootLogin no\n")
	if spec.ListenMeshOnly && spec.MeshIP != "" {
		// Bind sshd to the mesh IP only — public SSH is closed; access is over the
		// WireGuard tunnel. (ListenAddress is additive, so we pin exactly one.)
		fmt.Fprintf(&b, "ListenAddress %s\n", spec.MeshIP)
	}
	return b.String()
}

// RenderCISSysctl is the CIS Ubuntu/Debian Level-1 network + kernel sysctl set
// (idempotent; a resync rewrites the same file). Deterministic ordering keeps the
// rendered content stable.
func RenderCISSysctl() string {
	var b strings.Builder
	b.WriteString("# Managed by sigmahub (host.cis, CIS L1). Do not edit by hand.\n")
	keys := make([]string, 0, len(cisSysctls))
	for k := range cisSysctls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, cisSysctls[k])
	}
	return b.String()
}

// cisSysctls is the CIS L1 network-hardening sysctl set (§3 of the benchmark).
var cisSysctls = map[string]string{
	"net.ipv4.conf.all.rp_filter":            "1",
	"net.ipv4.conf.default.rp_filter":        "1",
	"net.ipv4.conf.all.accept_redirects":     "0",
	"net.ipv4.conf.default.accept_redirects":  "0",
	"net.ipv4.conf.all.secure_redirects":     "0",
	"net.ipv4.conf.all.send_redirects":       "0",
	"net.ipv4.conf.default.send_redirects":   "0",
	"net.ipv4.conf.all.accept_source_route":  "0",
	"net.ipv4.conf.all.log_martians":         "1",
	"net.ipv4.icmp_echo_ignore_broadcasts":   "1",
	"net.ipv4.tcp_syncookies":                "1",
	"net.ipv6.conf.all.accept_redirects":     "0",
	"net.ipv6.conf.all.accept_source_route":  "0",
	"kernel.randomize_va_space":              "2",
	"kernel.kptr_restrict":                   "2",
	"kernel.dmesg_restrict":                  "1",
	"fs.suid_dumpable":                       "0",
	"fs.protected_hardlinks":                 "1",
	"fs.protected_symlinks":                  "1",
}
