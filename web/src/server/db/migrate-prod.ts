// Production boot migration: when DATABASE_URL is set, apply the generated
// drizzle migrations against the node-postgres pool before the first request.
// Idempotent (drizzle tracks applied migrations); the PGlite demo path keeps
// its seed-script migration and never reaches this module.
import path from "node:path";
import { migrate } from "drizzle-orm/node-postgres/migrator";
import type { NodePgDatabase } from "drizzle-orm/node-postgres";
import { db } from "./index";

export async function migrateProd(): Promise<void> {
  await migrate(db as NodePgDatabase<Record<string, unknown>>, {
    migrationsFolder: path.join(process.cwd(), "drizzle"),
  });
}
