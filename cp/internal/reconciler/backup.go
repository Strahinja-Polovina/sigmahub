package reconciler

import (
	"encoding/json"
	"time"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/dsd"
	"github.com/Strahinja-Polovina/sigmahub/cp/internal/store"
)

// backupOpSpec is the wire payload of a backup.run / backup.verify /
// backup.restore op. It carries identifiers and policy knobs ONLY — the restic
// repo key and S3 credentials are fetched by the agent per run through the
// audited /v1/agent/backup-credential path, so a captured DSD leaks nothing.
// The dump/load commands are NOT carried here: the agent derives them from the
// engine name via its own catalog, preserving the no-generic-run-shell
// invariant (a DSD cannot smuggle a shell command into a backup op).
type backupOpSpec struct {
	RunID      string `json:"runId"`
	ResourceID string `json:"resourceId"`
	Container  string `json:"container"`
	Engine     string `json:"engine"`
	Image      string `json:"image"` // pinned engine image for scratch/verify containers
	Database   string `json:"database,omitempty"`
	Username   string `json:"username,omitempty"`
	// GFS retention applied via restic forget --prune after a successful backup.
	KeepDaily   int `json:"keepDaily,omitempty"`
	KeepWeekly  int `json:"keepWeekly,omitempty"`
	KeepMonthly int `json:"keepMonthly,omitempty"`
	// Verify: the last successful backup's dump sha256; the restored stream
	// must hash to exactly this before the scratch-load probe even starts.
	ExpectedSha string `json:"expectedSha,omitempty"`
	// Restore: the restic snapshot the run was pinned to when it was created
	// (SIGMA-245). The agent dumps THIS snapshot rather than `latest`, so the
	// bytes it loads are the ones whose digest the CP recorded. Empty falls back
	// to `latest` — older runs queued before the pin existed.
	SnapshotID string `json:"snapshotId,omitempty"`
	// Restore: the freshly provisioned resource the snapshot loads into.
	TargetContainer string `json:"targetContainer,omitempty"`
	TargetDatabase  string `json:"targetDatabase,omitempty"`
	TargetUsername  string `json:"targetUsername,omitempty"`
	// Restore-to-timestamp (P2-5b): the point in time WAL is replayed up to,
	// RFC3339. The agent selects the base snapshot ≤ this time and stops replay
	// at it (recovery_target_time). Empty for every other op.
	RecoveryTargetTime string `json:"recoveryTargetTime,omitempty"`
}

// renderBackupOps renders a server's open backup runs (P1-11). Each run is one
// op with id "bkr:<runId>" so status ingest and the dedicated result report
// map back to the backup_runs row. A backup op depends on its database's
// container op when that container is rendered in the same document (never
// dump a database the DSD is still creating); a verify op additionally depends
// on the same-document backup op for its resource so first-day verify runs
// after the first backup.
func renderBackupOps(runs []store.BackupRunSpec, rendered map[string]bool) []dsd.Op {
	// Map resource -> same-document backup op id, for verify/restore ordering.
	backupOp := map[string]string{}
	for _, r := range runs {
		if r.Kind == "backup" {
			backupOp[r.ResourceID] = "bkr:" + r.RunID
		}
	}
	var ops []dsd.Op
	for _, r := range runs {
		def, ok := store.DBEngine(r.Engine)
		if !ok {
			continue
		}
		spec := backupOpSpec{
			RunID:      r.RunID,
			ResourceID: r.ResourceID,
			Container:  dsd.ContainerName(r.ResourceID),
			Engine:     r.Engine,
			Image:      def.Image,
			Database:   r.Database,
			Username:   r.Username,
		}
		var kind string
		var deps []string
		switch r.Kind {
		case "backup":
			kind = dsd.KindBackupRun
			spec.KeepDaily, spec.KeepWeekly, spec.KeepMonthly = r.KeepDaily, r.KeepWeekly, r.KeepMonthly
			if rendered["res:"+r.ResourceID] {
				deps = append(deps, "res:"+r.ResourceID)
			}
		case "basebackup":
			// P2-5: physical base backup (pg_basebackup → restic), the PITR
			// starting point. Needs the live container, like a dump backup.
			kind = dsd.KindBackupBase
			spec.KeepDaily, spec.KeepWeekly, spec.KeepMonthly = r.KeepDaily, r.KeepWeekly, r.KeepMonthly
			if rendered["res:"+r.ResourceID] {
				deps = append(deps, "res:"+r.ResourceID)
			}
		case "verify":
			// Hold the verify until THIS day's backup has succeeded and its dump
			// sha is known (BackupRunsForServer resolves ExpectedSha from the same
			// UTC day's successful backup). Rendering it alongside a still-pending
			// backup pinned the PREVIOUS day's sha and then hashed `latest` (this
			// day's fresh dump), a guaranteed checksum mismatch whenever the data
			// changed (SIGMA-137). Once the backup succeeds, a resync renders the
			// verify against the correct sha.
			if r.ExpectedSha == "" {
				continue
			}
			kind = dsd.KindBackupVerify
			spec.ExpectedSha = r.ExpectedSha
			if dep, ok := backupOp[r.ResourceID]; ok {
				deps = append(deps, dep)
			}
		case "restore":
			kind = dsd.KindBackupRestore
			spec.ExpectedSha = r.ExpectedSha
			spec.SnapshotID = r.SnapshotID
			spec.TargetContainer = dsd.ContainerName(r.RestoreResourceID)
			spec.TargetDatabase = r.RestoreDatabase
			spec.TargetUsername = r.RestoreUsername
			if rendered["res:"+r.RestoreResourceID] {
				deps = append(deps, "res:"+r.RestoreResourceID)
			}
		case "restore-pitr":
			// P2-5b: recover the fresh resource to a chosen time. The agent
			// replays WAL from the newest base backup up to recoveryTargetTime,
			// then loads the recovered state into the target container.
			if r.RecoveryTargetTime == nil {
				continue
			}
			kind = dsd.KindBackupRestorePITR
			spec.TargetContainer = dsd.ContainerName(r.RestoreResourceID)
			spec.TargetDatabase = r.RestoreDatabase
			spec.TargetUsername = r.RestoreUsername
			spec.RecoveryTargetTime = r.RecoveryTargetTime.UTC().Format(time.RFC3339)
			if rendered["res:"+r.RestoreResourceID] {
				deps = append(deps, "res:"+r.RestoreResourceID)
			}
		default:
			continue
		}
		b, _ := json.Marshal(spec)
		ops = append(ops, dsd.Op{ID: "bkr:" + r.RunID, Kind: kind, DependsOn: deps, Spec: b})
	}
	return ops
}
