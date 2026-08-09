// Repairs a poisoned drizzle migration ledger before the migrator reads it.
//
// # The defect this exists to undo
//
// Drizzle does not apply migrations in file order. Its migrator asks the
// database for ONE row —
//
//     select id, hash, created_at from drizzle.__drizzle_migrations
//      order by created_at desc limit 1
//
// — and then applies every journal entry whose `when` is GREATER than that
// number. File order, the `idx` field and the 0006/0007 prefixes have no say in
// it. The ledger is a high-water mark, not a list of what ran.
//
// `drizzle/meta/_journal.json` entry 0005 was written by hand with a `when` of
// 1786530000000 — 2026-08-12, several days after the migration was authored and
// after the two that follow it (0006: 2026-08-08, 0007: 2026-08-09). The moment
// 0005 was applied, that future timestamp became the high-water mark, and every
// subsequent migration with an honest timestamp compared LOWER and was silently
// skipped. Not failed — skipped, on every boot, with no output.
//
// So `servers` never gained `facts`, `incompatible_reasons`, `name_auto`,
// `decommission_started_at` or `decommission_purge_volumes`, and the first
// write that referenced them — the control-plane mirror's upsert — failed. What
// the operator saw was "Control plane unreachable / showing the last synced
// state", because a failed mirror write is indistinguishable, from the banner's
// point of view, from a control plane that is actually down. The control plane
// was fine. The schema was three columns short.
//
// # Why this cannot be a migration
//
// The obvious fix — correct the journal — is necessary but not sufficient, and
// on its own it fixes nothing at all. The bad number is not only in the file;
// it is a ROW in `__drizzle_migrations`, written when 0005 was applied. Lower
// the journal entry and the high-water mark in the database is still
// 1786530000000, so 0006 and 0007 stay skipped forever.
//
// And the repair cannot itself be a migration, because a migration is exactly
// the thing that no longer runs. It has to happen BEFORE the migrator looks, on
// a connection the migrator has not touched yet. Hence this module, called
// first by both entry points (instrumentation → migrate-prod for a real
// Postgres, and the seed script for the PGlite demo).
//
// # Safety
//
// Idempotent by construction: it rewrites rows carrying one specific wrong
// value, so a second run matches nothing. On a fresh database the ledger table
// does not exist yet and the whole thing is a no-op — the guard is a
// `to_regclass` check rather than a swallowed exception, because a repair that
// hides errors is how the next one of these goes unnoticed for four merges.

/** The hand-written timestamp that poisoned the ledger (2026-08-12T10:20:00Z). */
export const POISONED_WHEN = 1786530000000;

/**
 * The value entry 0005 should always have carried: after 0004
 * (1786096469965) and before 0006 (1786231188225). It must stay in step with
 * `drizzle/meta/_journal.json`, and journal.test.ts asserts that it does.
 */
export const CORRECTED_WHEN = 1786100000000;

/** Runs one SQL string. Both drivers expose this; the caller supplies it so
 *  this module stays free of a driver import and can be unit-tested. */
export type SqlExec = (sql: string) => Promise<unknown>;

/**
 * Rewrite the poisoned high-water mark, if this database has one.
 *
 * Returns whether a repair statement was issued, so callers can log it — a
 * silent schema repair is only marginally better than a silent schema
 * omission.
 */
export async function repairMigrationLedger(exec: SqlExec): Promise<boolean> {
  // `to_regclass` answers NULL instead of raising for an absent relation, which
  // is what makes the fresh-database case a no-op rather than an error to
  // swallow. The DO block keeps it to a single round trip.
  await exec(`
    DO $$
    BEGIN
      IF to_regclass('drizzle.__drizzle_migrations') IS NOT NULL THEN
        UPDATE drizzle."__drizzle_migrations"
           SET created_at = ${CORRECTED_WHEN}
         WHERE created_at = ${POISONED_WHEN};
      END IF;
    END
    $$;
  `);
  return true;
}
