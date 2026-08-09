package uninstall

// The production implementations of the host-level teardown steps. They are
// deliberately separate from Handle: Handle owns the ORDER (which is the part
// that was getting this wrong), these own the mechanics, and cmd/sigmad only
// wires the two together.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Units the installer drops in (agent/packaging/install.sh). Kept here as
// constants next to the code that removes them so the install/uninstall pair is
// greppable from either end.
const (
	UnitName        = "sigmad.service"
	NftablesUnit    = "sigmahub-nftables.service"
	unitDir         = "/etc/systemd/system"
	envDir          = "/etc/sigmad"
	nftablesRuleset = "/etc/sigmahub/nftables.conf"
)

// RemoveSystemdUnits disables and deletes the units the installer created, plus
// the environment file that holds this host's control-plane endpoint and its
// (already-redeemed) bootstrap token.
//
// It uses `systemctl disable`, NOT `disable --now`: --now stops the unit, which
// means SIGTERM to the process running this very function, killing the teardown
// halfway through with the binary still on disk. The unit file is unlinked and
// systemd reloaded instead, so the currently-running service simply has no unit
// to restart when it exits a few steps later.
//
// The live nftables ruleset is deliberately left ALONE while its loader unit
// and config file go. Flushing a host's firewall as a side effect of returning
// the machine would be a security change nobody asked for; what we remove is
// our claim to re-apply it on the next boot.
func RemoveSystemdUnits(ctx context.Context, log *slog.Logger) error {
	var errs []error
	if _, err := exec.LookPath("systemctl"); err == nil {
		for _, unit := range []string{UnitName, NftablesUnit} {
			if out, err := exec.CommandContext(ctx, "systemctl", "disable", unit).CombinedOutput(); err != nil {
				// A unit that was never enabled is not a failure worth reporting
				// to the operator; log it and keep going to the unlink, which is
				// what actually matters.
				log.Warn("uninstall: systemctl disable", "unit", unit, "err", err,
					"output", strings.TrimSpace(string(out)))
			}
		}
	}
	for _, path := range []string{
		filepath.Join(unitDir, UnitName),
		filepath.Join(unitDir, NftablesUnit),
		filepath.Join(envDir, "env"),
		nftablesRuleset,
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
	}
	_ = os.Remove(envDir) // empty now; a non-empty dir is left as-is
	if _, err := exec.LookPath("systemctl"); err == nil {
		if out, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("systemctl daemon-reload: %w (%s)", err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}

// RemoveDataDir deletes the agent's data directory: identity, DSD journal,
// desired-state store, mesh key, build contexts. Runs after the ack, because
// the identity in it is what authenticated the ack.
func RemoveDataDir(dir string) error {
	if dir == "" {
		return errors.New("no data dir configured")
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove data dir %s: %w", dir, err)
	}
	return nil
}

// RemoveSelfBinary deletes the running sigmad executable.
//
// Unlinking a running binary is safe on Linux — the kernel keeps the inode
// alive for the mapped process — so the remaining steps and the exit still run
// from a file that no longer has a name. Symlinks are resolved first so a
// packaged install that points /usr/local/bin/sigmad at a versioned path
// removes the real thing rather than only the pointer.
func RemoveSelfBinary() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if err := os.Remove(self); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", self, err)
	}
	return nil
}

// k3sUninstallScripts are the self-uninstallers k3s writes at install time —
// one name for a server (control plane), another for an agent (worker). The
// agent installs k3s via https://get.k3s.io, and that installer is what puts
// these here.
var k3sUninstallScripts = []string{
	"/usr/local/bin/k3s-uninstall.sh",
	"/usr/local/bin/k3s-agent-uninstall.sh",
}

// RemoveK3s uninstalls k3s if this host ever joined a cluster.
//
// Cluster workloads run under k3s's own containerd, not Docker, so the managed-
// container sweep cannot see them: without this, a decommissioned node kept
// running k3s and every pod the scheduler had placed on it, on a machine the
// dashboard reported as removed. A host that never joined a cluster has no
// script here and this returns nil — absence is the normal case, not a failure.
func RemoveK3s(ctx context.Context, log *slog.Logger) error {
	var ran bool
	for _, script := range k3sUninstallScripts {
		if _, err := os.Stat(script); err != nil {
			continue
		}
		ran = true
		log.Info("agent.uninstall: removing k3s", "script", script)
		out, err := exec.CommandContext(ctx, script).CombinedOutput()
		if err != nil {
			// Report it rather than aborting: the rest of the teardown still
			// has to happen, and the ack carries this detail so the operator
			// learns the host needs a hand instead of believing it is clean.
			return fmt.Errorf("%s: %w (%s)", script, err, strings.TrimSpace(string(out)))
		}
	}
	if !ran {
		log.Debug("agent.uninstall: no k3s on this host")
	}
	return nil
}
