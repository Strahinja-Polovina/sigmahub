// Package mesh owns the agent's WireGuard identity and peer configuration.
// v0 is coordination-plane only: the key never leaves the host, the control
// plane never sees more than the public key, and tunnel bring-up is an
// optional extra on Linux.
package mesh

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const keyFile = "wg.key"

// ConfigFile is the rendered wg-quick config; the basename doubles as the
// interface name (`wg-quick up …/sigma0.conf` → sigma0).
const ConfigFile = "sigma0.conf"

// LoadOrCreateKey returns the host's Curve25519 keypair (base64), generating
// and persisting the private key (0600) on first run. Pure stdlib — no `wg`
// CLI involved.
func LoadOrCreateKey(dataDir string) (privB64, pubB64 string, err error) {
	curve := ecdh.X25519()
	path := filepath.Join(dataDir, keyFile)

	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil {
			return "", "", fmt.Errorf("corrupt mesh key %s: %w", path, err)
		}
		priv, err := curve.NewPrivateKey(raw)
		if err != nil {
			return "", "", fmt.Errorf("corrupt mesh key %s: %w", path, err)
		}
		return strings.TrimSpace(string(b)), base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
	case errors.Is(err, fs.ErrNotExist):
		priv, err := curve.GenerateKey(rand.Reader)
		if err != nil {
			return "", "", err
		}
		privB64 = base64.StdEncoding.EncodeToString(priv.Bytes())
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(path, []byte(privB64+"\n"), 0o600); err != nil {
			return "", "", err
		}
		return privB64, base64.StdEncoding.EncodeToString(priv.PublicKey().Bytes()), nil
	default:
		return "", "", err
	}
}

// Peer mirrors the control plane's mesh peer shape.
type Peer struct {
	ServerID string  `json:"serverId"`
	Name     string  `json:"name"`
	Pubkey   string  `json:"pubkey"`
	MeshIP   string  `json:"meshIp"`
	Endpoint *string `json:"endpoint"`
}

// Validate rejects any control-plane-supplied mesh value that could break out of
// the field it is rendered into.
//
// The mesh-peers channel is authenticated but NOT ed25519-signed the way a DSD
// is, and Self.MeshIP lands in the [Interface] section of a root wg-quick config
// (`Address = %s/16`). A value carrying a newline would inject an interface-level
// `PostUp = …` line that wg-quick runs as root on the next bring-up — a host RCE
// that never touches the DSD signing key (SIGMA-360). So every CP-supplied value
// is checked before any config is written or applied; a rejected set leaves the
// previous config untouched.
func Validate(selfMeshIP string, peers []Peer) error {
	if err := validMeshIP(selfMeshIP); err != nil {
		return fmt.Errorf("self mesh IP: %w", err)
	}
	for _, p := range peers {
		if err := validMeshIP(p.MeshIP); err != nil {
			return fmt.Errorf("peer %s mesh IP: %w", p.ServerID, err)
		}
		if err := validPubkey(p.Pubkey); err != nil {
			return fmt.Errorf("peer %s pubkey: %w", p.ServerID, err)
		}
		if p.Endpoint != nil && *p.Endpoint != "" {
			if err := validEndpoint(*p.Endpoint); err != nil {
				return fmt.Errorf("peer %s endpoint: %w", p.ServerID, err)
			}
		}
		if hasControlChar(p.Name) || hasControlChar(p.ServerID) {
			return fmt.Errorf("peer %s name/id carries a control character", p.ServerID)
		}
	}
	return nil
}

// validMeshIP requires a bare IP literal. netip.ParseAddr rejects a newline, a
// space, or anything that is not exactly an address, which is precisely the
// property the [Interface] Address line needs.
func validMeshIP(s string) error {
	if _, err := netip.ParseAddr(s); err != nil {
		return fmt.Errorf("%q is not a bare IP address", s)
	}
	return nil
}

// validPubkey requires a 32-byte key in strict standard base64 (the WireGuard
// key shape). StdEncoding rejects any non-alphabet byte, so a newline cannot ride
// through.
func validPubkey(s string) error {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("%q is not valid base64", s)
	}
	if len(raw) != 32 {
		return fmt.Errorf("key is %d bytes, want 32", len(raw))
	}
	return nil
}

// validEndpoint requires host:port with a numeric port and a host that is either
// an IP literal or a plain hostname — no whitespace, no config metacharacters.
func validEndpoint(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("%q is not host:port", s)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("%q has a bad port", s)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if !validHostname(host) {
		return fmt.Errorf("%q has an invalid host", s)
	}
	return nil
}

func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' || r < 0x20 {
			return true
		}
	}
	return false
}

// RenderConfig produces a wg-quick style config for this host and its peers.
// Callers MUST run Validate first (syncMesh does): RenderConfig itself only
// formats, so a value that reaches it unvalidated could inject a config line.
func RenderConfig(privB64, selfMeshIP string, peers []Peer) string {
	var b strings.Builder
	b.WriteString("# Generated by sigmad — do not edit; rewritten on peer changes.\n")
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", privB64)
	fmt.Fprintf(&b, "Address = %s/16\n", selfMeshIP)
	fmt.Fprintf(&b, "ListenPort = %d\n", ListenPort)
	for _, p := range peers {
		b.WriteString("\n[Peer]\n")
		fmt.Fprintf(&b, "# %s (%s)\n", p.Name, p.ServerID)
		fmt.Fprintf(&b, "PublicKey = %s\n", p.Pubkey)
		fmt.Fprintf(&b, "AllowedIPs = %s/32\n", p.MeshIP)
		if p.Endpoint != nil && *p.Endpoint != "" {
			fmt.Fprintf(&b, "Endpoint = %s\n", *p.Endpoint)
		}
		b.WriteString("PersistentKeepalive = 25\n")
	}
	return b.String()
}

// WriteConfig persists the rendered config (0600 — it embeds the private
// key), returning its path and whether the content actually changed.
func WriteConfig(dataDir, content string) (path string, changed bool, err error) {
	path = filepath.Join(dataDir, ConfigFile)
	if prev, err := os.ReadFile(path); err == nil && string(prev) == content {
		return path, false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return "", false, err
	}
	return path, true, os.Rename(tmp, path)
}

// ifaceName is the WireGuard interface name, derived from the config basename
// (sigma0.conf → sigma0), matching wg-quick's convention.
func ifaceName() string { return strings.TrimSuffix(ConfigFile, ".conf") }

// Apply reconciles the WireGuard interface to the rendered config. v1: the
// first bring-up uses `wg-quick up`, but subsequent peer changes are applied
// incrementally with `wg syncconf` — adding/removing peers WITHOUT tearing the
// tunnel down, so an existing connection survives a peer-list change (P1-4).
// Linux only; a missing tool is reported, not fatal, so coordination keeps
// running.
func Apply(ctx context.Context, log *slog.Logger, configPath string) {
	if runtime.GOOS != "linux" {
		log.Info("mesh: config-only mode (tunnel bring-up is Linux-only)", "config", configPath)
		return
	}
	if _, err := exec.LookPath("wg-quick"); err != nil {
		log.Warn("mesh: wg-quick not found; config rendered but not applied", "config", configPath)
		return
	}
	iface := ifaceName()
	if interfaceExists(ctx, iface) {
		// Incremental peer sync — no disruptive down/up.
		if err := syncConf(ctx, iface, configPath); err != nil {
			log.Warn("mesh: incremental syncconf failed; falling back to full re-up", "err", err)
			_ = exec.CommandContext(ctx, "wg-quick", "down", configPath).Run()
			if out, err := exec.CommandContext(ctx, "wg-quick", "up", configPath).CombinedOutput(); err != nil {
				log.Warn("mesh: wg-quick up failed", "err", err, "output", strings.TrimSpace(string(out)))
			}
			return
		}
		log.Info("mesh: peers synced incrementally", "iface", iface)
		return
	}
	// First bring-up creates the interface, address and link.
	if out, err := exec.CommandContext(ctx, "wg-quick", "up", configPath).CombinedOutput(); err != nil {
		log.Warn("mesh: wg-quick up failed", "err", err, "output", strings.TrimSpace(string(out)))
		return
	}
	log.Info("mesh: WireGuard interface applied", "config", configPath)
}

// TearDown brings the WireGuard interface down and deletes the rendered config
// and the private key — the mesh half of a decommission (SIGMA-204).
//
// Callers must have finished talking to the control plane before this runs.
// `wg-quick down` tears out the routes, and on a fleet whose control plane is
// itself reachable over the mesh that is the last packet this host can send;
// removing the key afterwards means even a restarted agent could not re-form
// the tunnel. Both are fine — that is the point — but only after the ack.
//
// Best-effort and idempotent: an interface that is already down, a config that
// is already gone and a host with no wg-quick all count as torn down, because
// the state this function promises is "no mesh here", not "a command ran".
func TearDown(ctx context.Context, log *slog.Logger, dataDir string) error {
	configPath := filepath.Join(dataDir, ConfigFile)
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("wg-quick"); err == nil && interfaceExists(ctx, ifaceName()) {
			if out, err := exec.CommandContext(ctx, "wg-quick", "down", configPath).CombinedOutput(); err != nil {
				// Reported, not fatal: the config and key still go, and the
				// operator gets the manual cleanup script naming the interface.
				log.Warn("mesh: wg-quick down failed", "err", err, "output", strings.TrimSpace(string(out)))
				return fmt.Errorf("wg-quick down: %w", err)
			}
			log.Info("mesh: interface torn down", "iface", ifaceName())
		}
	}
	var errs []error
	for _, p := range []string{configPath, filepath.Join(dataDir, keyFile)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", filepath.Base(p), err))
		}
	}
	return errors.Join(errs...)
}

// InterfaceUp reports whether the mesh tunnel is actually up. On non-Linux the
// agent runs config-only (nothing to bring up), so it is "up" by definition;
// on Linux it checks the real WireGuard device. Callers use this to report an
// honest MeshApplied to the CP instead of assuming success (SIGMA-144).
func InterfaceUp(ctx context.Context) bool {
	if runtime.GOOS != "linux" {
		return true
	}
	return interfaceExists(ctx, ifaceName())
}

// interfaceExists reports whether the WireGuard device is already up (`wg show`
// exits non-zero for an absent device).
func interfaceExists(ctx context.Context, iface string) bool {
	if _, err := exec.LookPath("wg"); err != nil {
		return false
	}
	return exec.CommandContext(ctx, "wg", "show", iface).Run() == nil
}

// syncConf applies peer changes to a live interface without a teardown.
// `wg-quick strip` yields a wg-native config (drops the Address/wg-quick keys)
// which `wg syncconf` reconciles peer-by-peer. The stripped file embeds the
// private key, so it is written 0600 and removed after.
func syncConf(ctx context.Context, iface, configPath string) error {
	stripped, err := exec.CommandContext(ctx, "wg-quick", "strip", configPath).Output()
	if err != nil {
		return fmt.Errorf("wg-quick strip: %w", err)
	}
	tmp := configPath + ".stripped"
	if err := os.WriteFile(tmp, stripped, 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if out, err := exec.CommandContext(ctx, "wg", "syncconf", iface, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("wg syncconf: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
