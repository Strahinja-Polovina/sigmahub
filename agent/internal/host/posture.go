package host

import (
	"context"
	"strings"
)

// Posture is the agent's self-assessment of its host hardening, recomputed on a
// schedule (the daily drift re-check) and reported to the control plane over the
// heartbeat so the dashboard can show a score + disk-encryption status.
type Posture struct {
	Score         int  `json:"score"` // 0..100, fraction of controls in the desired state
	DiskEncrypted bool `json:"diskEncrypted"`
	SSHLocked     bool `json:"sshLocked"` // password auth off
	CISApplied    bool `json:"cisApplied"`
	FirewallOn    bool `json:"firewallOn"`
	AuditdOn      bool `json:"auditdOn"`
}

// Posture inspects the live host and scores it. Each probe is best-effort: a
// missing tool or a non-root agent simply scores that control as not-met rather
// than failing, so the report is always producible.
func (d *Driver) Posture(ctx context.Context) Posture {
	p := Posture{
		DiskEncrypted: d.diskEncrypted(ctx),
		SSHLocked:     d.sshLocked(ctx),
		CISApplied:    d.cisApplied(ctx),
		FirewallOn:    d.firewallOn(ctx),
		AuditdOn:      d.auditdActive(ctx),
	}
	// Weight each control equally; disk encryption is host-level (not something a
	// DSD op sets) so it counts toward the score but never blocks it.
	checks := []bool{p.SSHLocked, p.CISApplied, p.FirewallOn, p.AuditdOn, p.DiskEncrypted}
	met := 0
	for _, ok := range checks {
		if ok {
			met++
		}
	}
	p.Score = met * 100 / len(checks)
	return p
}

// diskEncrypted reports whether any block device is LUKS/crypt-backed. Surfaced
// in the dashboard so operators can see hosts without disk encryption (where
// env-var secrets are unsafe).
func (d *Driver) diskEncrypted(ctx context.Context) bool {
	out, err := d.runner(ctx, "lsblk", "-o", "TYPE", "-n")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "crypt" {
			return true
		}
	}
	return false
}

// sshLocked reports whether sshd's effective config disables password auth.
func (d *Driver) sshLocked(ctx context.Context) bool {
	out, err := d.runner(ctx, "sshd", "-T")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), "passwordauthentication no")
}

// cisApplied reports whether a representative CIS sysctl is in the desired state.
func (d *Driver) cisApplied(ctx context.Context) bool {
	out, err := d.runner(ctx, "sysctl", "-n", "kernel.randomize_va_space")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "2"
}

// firewallOn reports whether our nft table is loaded.
func (d *Driver) firewallOn(ctx context.Context) bool {
	out, err := d.runner(ctx, "nft", "list", "tables")
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "sigmahub")
}

func (d *Driver) auditdActive(ctx context.Context) bool {
	out, err := d.runner(ctx, "systemctl", "is-active", "auditd")
	if err != nil {
		// is-active exits non-zero when inactive; the stdout still says the state.
		return strings.TrimSpace(string(out)) == "active"
	}
	return strings.TrimSpace(string(out)) == "active"
}
