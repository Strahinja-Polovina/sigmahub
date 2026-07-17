# PostgreSQL point-in-time recovery (P2-5)

Daily restic backups (P1-11) let you restore to yesterday's snapshot.
Point-in-time recovery (PITR) lets a **postgres** resource restore to *any
moment* in a continuous window, by pairing a periodic physical base backup
with continuously archived write-ahead log (WAL) segments.

## What ships in phase A

Enabling PITR on a postgres resource (Backups panel → *Point-in-time
recovery*, or `PATCH …/backup-policy {"pitrEnabled": true}`) turns on:

1. **Continuous WAL archiving.** The reconciler re-renders the postgres
   container with `archive_mode=on`, `wal_level=replica`, and an
   `archive_command` that copies each completed segment into a dedicated
   spool volume using a temp-file-plus-rename, so a half-written segment is
   never visible. (`cp/internal/reconciler/database.go`.)
2. **The WAL shipper** (`agent/internal/backup/wal.go`), a per-minute agent
   loop: it bundles the ready segments (`tar` via `docker exec`, streamed
   into the resource's restic repo under the `wal` tag) and deletes each
   segment **only after** restic confirms the bundle is durable. Credentials
   are released per cycle through the same audited path as backup runs; the
   high-water mark is reported to the CP and shown as the recovery window.
3. **A daily physical base backup** (`backup.base` run → `pg_basebackup` piped
   into restic under the `base` tag) — the starting point WAL segments replay
   from. It joins the existing daily backup/verify schedule
   (`CreateDueBackupRuns`).

The Backups panel shows the live window: *"Recoverable up to `<time>` (last
archived segment `<name>`)."* Until the first segment ships it says so plainly
rather than implying coverage that doesn't exist yet.

### Safety invariants

- A segment is **never deleted before it is durably in the repo** (ship, then
  delete exactly what was shipped).
- The DSD carries no credentials — WAL/base repo keys and S3 secrets are
  released per cycle/run over the audited agent channel and live only in the
  restic child process.
- PITR is postgres-only; enabling it on another engine is a typed error.
- Disabling PITR re-renders the container without archiving and drops the
  resource from the shipper's target list.

## Restore-to-timestamp

The actual "restore to a chosen timestamp" flow — provision a fresh resource,
restore the latest base backup before the target time, then replay WAL to a
`recovery_target_time` — is tracked as a **follow-up ticket**. Phase A lands
the archiving pipeline and window so the data needed for that restore is being
captured continuously from the moment PITR is switched on.

Operationally, until the restore-to-timestamp UI ships, the archived base +
WAL in the restic repo can be restored manually by an operator with the repo
credentials (standard postgres `restore_command` + `recovery_target_time`).
