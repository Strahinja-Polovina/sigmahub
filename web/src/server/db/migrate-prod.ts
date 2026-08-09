// Production boot migration: when DATABASE_URL is set, apply the generated
// drizzle migrations against the node-postgres pool before the first request.
// Idempotent (drizzle tracks applied migrations); the PGlite demo path keeps
// its seed-script migration and never reaches this module.
//
// The ledger repair runs FIRST and must keep running first — see
// migrate-repair.ts for what it undoes and why it cannot be a migration itself.
import path from "node:path";
import { sql } from "drizzle-orm";
import { migrate } from "drizzle-orm/node-postgres/migrator";
import type { NodePgDatabase } from "drizzle-orm/node-postgres";
import { db } from "./index";
import { repairMigrationLedger } from "./migrate-repair";

export async function migrateProd(): Promise<void> {
  await repairMigrationLedger((stmt) => db.execute(sql.raw(stmt)));
  await migrate(db as NodePgDatabase<Record<string, unknown>>, {
    migrationsFolder: path.join(process.cwd(), "drizzle"),
  });
}
