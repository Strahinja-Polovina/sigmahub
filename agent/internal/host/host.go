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
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Strahinja-Polovina/sigmahub/agent/internal/apply"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/agent/internal/mesh"
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
	// WireguardPort is an OVERRIDE, not a setting the control plane fills in.
	// Zero — which is what the control plane sends — means mesh.ListenPort, the
	// port this same agent actually listens on. See RenderNftables and
	// SIGMA-275: a control plane that restated the number was a second copy
	// that could only ever drift out of step with the socket.
	WireguardPort  int             `json:"wireguardPort,omitempty"`
	MeshInterface  string          `json:"meshInterface"` // e.g. sigma0
	AllowPublicSSH bool            `json:"allowPublicSSH"`
	ProxyRole      bool            `json:"proxyRole"` // opens 80/443
	ExtraPorts     []PortException `json:"extraPorts,omitempty"`
}

// SSHDSpec is the post-enrollment SSH lockdown. By default password + root login
// are off and sshd binds to the mesh IP only; keeping public SSH is the opt-out.
type SSHDSpec struct {
	MeshIP         string `json:"meshIp"`
	ListenMeshOnly bool   `json:"listenMeshOnly"`
}

// CISSpec selects the CIS benchmark level (only 1 in Phase 1).
type CISSpec struct {
	Level int `json:"level"`
}

// Driver applies host-hardening ops. runner is swapped in tests; euid gates the
// real host mutations so a non-root agent fails cleanly rather than silently.
type Driver struct {
	runner    func(ctx context.Context, name string, args ...string) ([]byte, error)
	writeFile func(path string, data []byte, perm os.FileMode) error
	mkdirAll  func(path string, perm os.FileMode) error
	euid      int
}

// NewDriver builds a host driver bound to the real OS.
func NewDriver() *Driver {
	return &Driver{
		runner: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
		writeFile: os.WriteFile,
		mkdirAll:  os.MkdirAll,
		euid:      os.Geteuid(),
	}
}

// ensureDir creates path (and parents) if missing, via the injected mkdirAll
// when set (tests) or os.MkdirAll otherwise.
func (d *Driver) ensureDir(path string, perm os.FileMode) error {
	if d.mkdirAll != nil {
		return d.mkdirAll(path, perm)
	}
	return os.MkdirAll(path, perm)
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

// defaultMeshInterface is the WireGuard link name every agent uses unless the
// spec names another.
const defaultMeshInterface = "sigma0"

// ifnameRe is the kernel's interface-name shape: at most IFNAMSIZ-1 (15) bytes
// from a conservative charset. Anything outside it is either not a name the
// kernel would accept or not a token nft would parse — and, crucially, cannot
// contain a quote, whitespace, newline, brace or semicolon.
var ifnameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`)

// validIfname gates every DSD-supplied interface name before it is interpolated
// into the ruleset (SIGMA-342).
//
// RenderNftables writes the value straight into a quoted nft token
// (`iifname "%s" accept`). A value carrying a quote and a newline closes the
// rule and appends attacker-chosen nft statements — `policy accept` on the very
// input chain the rest of this file works to keep default-drop — and opNftables
// then loads the result with `nft -f` as root, reports the op applied, and
// leaves the dashboard green. The mesh-only guarantee that databases, object
// storage and the k3s API server depend on would evaporate fleet-wide with the
// file on disk as the only evidence. The threat model is the same one the
// container policy is written against: a compromised control plane whose DSD
// still signs correctly. A merely buggy CP that emits a malformed value is the
// other half — it produces `nft -f` syntax errors and leaves the firewall
// unloaded, which is just as silent.
//
// "." and ".." pass the charset but are rejected by the kernel's own
// dev_valid_name, so they are refused here too.
func validIfname(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	return ifnameRe.MatchString(name)
}

func (d *Driver) opNftables(ctx context.Context, op dsd.Op) error {
	var spec NftablesSpec
	if err := json.Unmarshal(op.Spec, &spec); err != nil {
		return fmt.Errorf("decode nftables spec: %w", err)
	}
	// Refuse a signed-but-forbidden value the way the container driver refuses a
	// forbidden container spec: before anything is written or loaded (SIGMA-342).
	// An empty name means "use the default", not "anything goes".
	if spec.MeshInterface != "" && !validIfname(spec.MeshInterface) {
		return fmt.Errorf("policy: meshInterface %q is not a valid interface name (at most 15 characters of A-Za-z0-9_.-); refusing to load a ruleset built from it", spec.MeshInterface)
	}
	if err := d.requireRoot(); err != nil {
		return err
	}
	ruleset := RenderNftables(spec)
	// os.WriteFile does not create parent dirs and nothing else ever creates
	// /etc/sigmahub, so on a fresh host this write failed with ENOENT on every
	// apply — the firewall (and the mesh-only-SSH guarantee that depends on it)
	// never loaded (SIGMA-143). Create the dir first.
	if err := d.ensureDir(filepath.Dir(nftRulesetPath), 0o755); err != nil {
		return fmt.Errorf("create nft ruleset dir: %w", err)
	}
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
	// Apply ONLY our file with -e (ignore errors about unknown keys): on hosts
	// with IPv6 disabled the net.ipv6.* keys don't exist under /proc, and
	// `sysctl --system` would exit non-zero and (wrongly) fail the whole op.
	if out, err := d.runner(ctx, "sysctl", "-e", "-p", cisSysctlPath); err != nil {
		return fmt.Errorf("sysctl -p %s: %w: %s", cisSysctlPath, err, strings.TrimSpace(string(out)))
	}
	// auditd is a CIS L1 control, but the package isn't on the stock base images.
	// Enable it best-effort: if it isn't installed the op still converges (the
	// installer apt-installs it, and the posture score reflects whether auditd
	// ended up active). Never block convergence on it.
	_, _ = d.runner(ctx, "systemctl", "enable", "--now", "auditd")
	return nil
}

// RenderNftables produces a complete, atomic nft ruleset. The generation itself
// is the safety guard: output policy is accept and established/related are always
// accepted, so the agent's OUTBOUND control-plane channel is never severed; the
// WireGuard port and mesh interface are always open so the fleet stays meshed.
func RenderNftables(spec NftablesSpec) string {
	wg := spec.WireguardPort
	if wg == 0 {
		// mesh.ListenPort, not a literal (SIGMA-275). This rule and the
		// `ListenPort = ` line the mesh package writes into sigma0.conf are the
		// same UDP port seen from two sides: one opens the socket, the other is
		// what lets a handshake reach it. As two independent literals they
		// agreed only by luck, and the day they stopped agreeing the agent would
		// report a healthy config while the kernel dropped every handshake.
		wg = mesh.ListenPort
	}
	mesh := spec.MeshInterface
	// opNftables refuses an invalid name outright; falling back here as well
	// means the renderer can never emit a ruleset carrying injected statements
	// even if a future caller renders without going through the op (SIGMA-342).
	if !validIfname(mesh) {
		mesh = defaultMeshInterface
	}
	var b strings.Builder
	b.WriteString("#!/usr/sbin/nft -f\n")
	b.WriteString("# Managed by sigmahub (host.nftables). Do not edit by hand.\n")
	// Replace ONLY our own table, atomically — never `flush ruleset`, which would
	// wipe Docker's ip/ip6 NAT + filter chains and break container networking. The
	// ensure-then-delete-then-define idiom leaves every other table untouched.
	b.WriteString("table inet sigmahub {}\n")
	b.WriteString("delete table inet sigmahub\n\n")
	b.WriteString("table inet sigmahub {\n")
	// Only an INPUT chain (host protection). We deliberately do NOT manage the
	// forward hook — that is Docker's domain; a drop here would be traversed
	// alongside Docker's forward chain and kill bridge forwarding. Output is left
	// unfiltered (default accept) so the agent's outbound channel always survives.
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority 0; policy drop;\n")
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\tct state invalid drop\n")
	b.WriteString("\t\tiif \"lo\" accept\n")
	b.WriteString("\t\tip protocol icmp accept\n")
	b.WriteString("\t\tip6 nexthdr ipv6-icmp accept\n")
	fmt.Fprintf(&b, "\t\tudp dport %d accept\n", wg)     // WireGuard mesh — always
	fmt.Fprintf(&b, "\t\tiifname \"%s\" accept\n", mesh) // intra-fleet + mesh SSH
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
// off (SIGMA-A-5). We deliberately do NOT pin ListenAddress to the mesh IP:
// sshd would fail to start after any reboot where the WireGuard interface has
// not yet come up (the mesh IP is unassigned), and `sshd -t` cannot detect an
// unbindable address — a full remote-SSH lockout with no rollback. Instead sshd
// keeps its default wildcard bind (so it always starts) and mesh-only access is
// enforced at the firewall (host.nftables drops public tcp/22 unless
// AllowPublicSSH, and accepts 22 arriving on the mesh interface).
func RenderSSHDConfig(spec SSHDSpec) string {
	var b strings.Builder
	b.WriteString("# Managed by sigmahub (host.sshd). Do not edit by hand.\n")
	b.WriteString("PasswordAuthentication no\n")
	b.WriteString("KbdInteractiveAuthentication no\n")
	b.WriteString("PermitRootLogin no\n")
	_ = spec // MeshIP/ListenMeshOnly retained on the wire; access is firewall-enforced.
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
	"net.ipv4.conf.default.accept_redirects": "0",
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
