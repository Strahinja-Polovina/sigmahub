// Production boot migration: when DATABASE_URL is set, apply the generated
// drizzle migrations against the node-postgres pool before the first request.
// Idempotent (drizzle tracks applied migrations); the PGlite demo path keeps
// its seed-script migration and never reaches this module.
//
// The ledger repair runs FIRST and must keep running first — see
// migrate-repair.ts for what it undoes and why it cannot be a migration itself.
//
// The whole thing runs under a session-level advisory lock (SIGMA-290). This is
// called from instrumentation.ts, i.e. once per Next.js boot, so the number of
// processes racing it is the number of dashboard replicas — and two replicas
// starting milliseconds apart both read the same migration ledger, both decide
// the same file is unapplied, and the second one's DDL fails against objects
// the first has already created. Boot then throws, the container restarts, and
// the operator watches a crash loop whose only clue is a `relation already
// exists` from a migration that plainly did apply.
//
// Blocking is the right behaviour: a replica that arrives second should WAIT
// and then find the ledger already up to date. The lock is taken on a DEDICATED
// connection rather than through `db`, because `db` is a pool — consecutive
// statements can land on different backends and a session-level lock taken on
// one of them protects nothing.
import path from "node:path";
import { sql } from "drizzle-orm";
import { migrate } from "drizzle-orm/node-postgres/migrator";
import type { NodePgDatabase } from "drizzle-orm/node-postgres";
import { Client } from "pg";
import { db } from "./index";
import { repairMigrationLedger } from "./migrate-repair";

// MIGRATE_LOCK_KEY is hashed by Postgres into the advisory-lock id. It is a
// name, not a magic number, so the control plane can take the SAME lock by
// writing the same string — see migrateLockKey in cp/internal/store/store.go.
// The two halves keep separate migration ledgers in one database; the mutual
// exclusion between the processes touching it is shared.
export const MIGRATE_LOCK_KEY = "sigmahub:migrate";

async function applyMigrations(): Promise<void> {
  await repairMigrationLedger((stmt) => db.execute(sql.raw(stmt)));
  await migrate(db as NodePgDatabase<Record<string, unknown>>, {
    migrationsFolder: path.join(process.cwd(), "drizzle"),
  });
}

export async function migrateProd(): Promise<void> {
  const connectionString = process.env.DATABASE_URL;
  if (!connectionString) {
    // Only reachable if a caller bypasses instrumentation.ts's own guard. No
    // DATABASE_URL means no shared database and therefore nothing to serialise.
    await applyMigrations();
    return;
  }
  const lock = new Client({ connectionString });
  await lock.connect();
  try {
    await lock.query("SELECT pg_advisory_lock(hashtext($1))", [MIGRATE_LOCK_KEY]);
    await applyMigrations();
  } finally {
    // end() drops the session and with it the lock, but unlocking explicitly
    // keeps the release visible in pg_locks the moment the work is done rather
    // than whenever the socket closes.
    try {
      await lock.query("SELECT pg_advisory_unlock(hashtext($1))", [MIGRATE_LOCK_KEY]);
    } catch {
      // The connection is going away regardless; a failed unlock is not a
      // reason to fail a boot whose migrations succeeded.
    }
    await lock.end();
  }
}
