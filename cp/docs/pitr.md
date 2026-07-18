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

## Restore-to-timestamp (phase B, P2-5b)

Restoring to a chosen moment provisions a **fresh** postgres resource (the
source is never touched) and recovers it to the target time. A Project Admin
picks the time on the database's Backups panel — the picker is bounded by the
live recovery window (the newest archived WAL).

**Server-side window validation.** Before a run is queued the CP checks the
target is reachable, so the agent never starts a recovery that can't finish:

- PITR is enabled and the repo key exists.
- A **base backup finished at or before** the target — the replay start point.
- The **WAL archive already covers** the target (`last_shipped_at >= target`).
- The target is not in the future.

**How the agent recovers.** The `restore-pitr` run carries only the target time
(the restic repo key + S3 credentials are fetched per run over the audited
credential path — the DSD leaks nothing). The agent:

1. selects the newest `base` snapshot taken ≤ the target and every `wal` bundle
   up to the first one past it;
2. runs recovery in a **throwaway container** (network-isolated, no ports):
   untar the base into `PGDATA`, stage the WAL, write `recovery.signal` +
   `restore_command` + `recovery_target_time` + `recovery_target_action =
   promote`, and start postgres — it replays WAL to the target, then promotes;
3. `pg_dump`s the recovered state and loads it into the fresh target resource
   (the same load+probe as the fire-drill restore), then tears the recovery
   container down.

Recovery happens in the scratch container rather than the reconciler-managed
target so the two never fight over the container; the target ends up with a
logical copy of the point-in-time state.

> **Staging note.** The recovery orchestration (snapshot selection, staging,
> recovery-container lifecycle, load) is covered by unit tests against a stub
> restic + fake docker. The actual postgres WAL replay to a timestamp is a
> standard-postgres path validated end-to-end on staging, not in CI.

Operators can still restore manually from the archived base + WAL with the repo
credentials (standard postgres `restore_command` + `recovery_target_time`).
