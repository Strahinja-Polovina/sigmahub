package store

import (
	"strings"
	"testing"
)

// TestSetBackupRunResult_LabelsEveryKind is the SIGMA-286 regression.
//
// backup_runs.kind has five values, and the sentence SetBackupRunResult writes
// into the audit log — and reuses as the alert title — came from a three-entry
// map literal. `basebackup` and `restore-pitr` missed it and mapped to "", so a
// PITR-enabled database gained one audit row per day whose Action read
// " succeeded": a leading space and no subject. On failure the alert reaching
// Slack was titled " failed for orders-db", which tells on-call nothing about
// which of the five operations broke, and the audit log — the artifact used to
// reconstruct what happened to a database — could not tell a failed base backup
// from a failed restore.
//
// The rule the table must keep: every kind gets a label, and an unknown kind
// falls back to the raw kind string, so a newly added kind is at worst ugly and
// never anonymous.
func TestSetBackupRunResult_LabelsEveryKind(t *testing.T) {
	kinds := []string{"backup", "verify", "basebackup", "restore", "restore-pitr"}
	for _, kind := range kinds {
		label := backupRunAction(kind)
		if label == "" {
			t.Errorf("kind %q has no label: the audit row reads %q and the alert title "+
				"loses its subject entirely (SIGMA-286)", kind, " succeeded")
			continue
		}
		for _, outcome := range []string{" succeeded", " failed"} {
			sentence := label + outcome
			if strings.HasPrefix(sentence, " ") {
				t.Errorf("kind %q renders %q — a leading space and no subject", kind, sentence)
			}
		}
	}

	// A kind nobody has taught the table about is ugly, not anonymous.
	if got := backupRunAction("teleport"); got != "teleport" {
		t.Errorf("backupRunAction(unknown) = %q, want the raw kind back", got)
	}

	// The timeout sweep builds its own titles in SQL. It must read from the same
	// table, or the two sentences drift the way they already did once.
	sql := backupRunActionCaseSQL("f.kind")
	for _, kind := range kinds {
		if !strings.Contains(sql, "'"+kind+"'") || !strings.Contains(sql, "'"+backupRunAction(kind)+"'") {
			t.Errorf("timeout-sweep CASE does not label %q: %s", kind, sql)
		}
	}
	if !strings.Contains(sql, "ELSE f.kind") {
		t.Errorf("timeout-sweep CASE must fall back to the raw kind, got: %s", sql)
	}
}
