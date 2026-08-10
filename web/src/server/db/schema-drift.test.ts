// schema.ts and drizzle/*.sql must describe the same database (SIGMA-276).
//
// Two files state one fact here: schema.ts is what drizzle builds every query
// from, and drizzle/*.sql is what the deployed Postgres actually has. Nothing
// held them together. Adding a column to `memberships` without running
// `pnpm db:generate` passes tsc (the type comes from schema.ts), passes eslint,
// and passes all 648 tests — the demo-mode suites migrate a PGlite from
// drizzle/, but they only see the drift if some test happens to SELECT the
// table that moved, and for memberships and invitations none does. The failure
// then lands on staging, in front of a real user, as `column does not exist`
// from a query the type system was happy with.
//
// The control plane already fails its build when generated output goes stale
// (TestGeneratedTypeScriptIsUpToDate), and journal.test.ts already guards the
// migrator's timestamp ordering after that class of bug cost production once.
// This is the same guard for the other half of the schema.
//
// It is deliberately a test rather than `drizzle-kit generate --check` in CI:
// the question worth asking is not "would the generator emit a file?" but
// "does the database these migrations produce match the tables the app
// queries?", and that is answered by migrating one and looking.

import { describe, expect, it } from "vitest";
import { PGlite } from "@electric-sql/pglite";
import { drizzle } from "drizzle-orm/pglite";
import { migrate } from "drizzle-orm/pglite/migrator";
import { is } from "drizzle-orm";
import { PgTable, getTableConfig } from "drizzle-orm/pg-core";
import * as schema from "./schema";
import * as authSchema from "./auth-schema";

type ColumnFact = { table: string; column: string; notNull: boolean };

/** What schema.ts + auth-schema.ts declare — i.e. what drizzle will name in
 *  the SQL it builds at runtime. */
function declaredColumns(): ColumnFact[] {
  const out: ColumnFact[] = [];
  for (const value of Object.values({ ...schema, ...authSchema })) {
    if (!is(value, PgTable)) continue;
    const cfg = getTableConfig(value);
    for (const column of cfg.columns) {
      out.push({ table: cfg.name, column: column.name, notNull: column.notNull });
    }
  }
  return out;
}

/** What the checked-in migrations actually create. Migrated once and shared:
 *  standing up a PGlite and replaying every migration is the expensive part of
 *  this file, and all three assertions ask about the same database. */
let migrated: Promise<ColumnFact[]> | null = null;
function migratedColumns(): Promise<ColumnFact[]> {
  migrated ??= migrateAndIntrospect();
  return migrated;
}

async function migrateAndIntrospect(): Promise<ColumnFact[]> {
  const pglite = new PGlite();
  await migrate(drizzle(pglite), { migrationsFolder: "drizzle" });
  const res = await pglite.query<{
    table_name: string;
    column_name: string;
    is_nullable: string;
  }>(
    `SELECT table_name, column_name, is_nullable
       FROM information_schema.columns
      WHERE table_schema = 'public'
        AND table_name <> '__drizzle_migrations'`
  );
  await pglite.close();
  return res.rows.map((r) => ({
    table: r.table_name,
    column: r.column_name,
    notNull: r.is_nullable === "NO",
  }));
}

const key = (c: ColumnFact) => `${c.table}.${c.column}`;

describe("schema.ts against the checked-in drizzle migrations", () => {
  it("declares no table or column the migrations never create", async () => {
    const migrated = new Set((await migratedColumns()).map(key));
    const missing = declaredColumns()
      .filter((c) => !migrated.has(key(c)))
      .map(
        (c) =>
          `${key(c)} is declared in schema.ts but no migration creates it — every query ` +
          `drizzle builds for ${c.table} will name a column Postgres does not have. ` +
          `Run \`pnpm db:generate\` and commit the migration.`
      );
    expect(missing).toEqual([]);
  }, 30_000);

  it("declares every column the migrations create", async () => {
    // The other direction. A column dropped from schema.ts without a migration
    // is not a crash, but it is a column the app can no longer read and nobody
    // decided to lose — and if it is NOT NULL without a default, every insert
    // drizzle builds fails.
    const declared = new Set(declaredColumns().map(key));
    const orphans = (await migratedColumns())
      .filter((c) => !declared.has(key(c)))
      .map(
        (c) =>
          `${key(c)} exists in the migrations but is absent from schema.ts` +
          (c.notNull ? " — and it is NOT NULL, so inserts will fail" : "")
      );
    expect(orphans).toEqual([]);
  }, 30_000);

  it("agrees with the migrations about which columns are NOT NULL", async () => {
    // Nullability is compared because it is the difference that turns a
    // successful-looking insert into a constraint violation at runtime. Types
    // are not: drizzle's SQL type names and information_schema's spellings
    // ("timestamp" vs "timestamp without time zone") differ in ways that would
    // make this test about the mapping rather than about the schema.
    const migrated = new Map((await migratedColumns()).map((c) => [key(c), c.notNull]));
    const mismatched = declaredColumns()
      .filter((c) => migrated.has(key(c)) && migrated.get(key(c)) !== c.notNull)
      .map(
        (c) =>
          `${key(c)}: schema.ts says ${c.notNull ? "NOT NULL" : "nullable"}, the migrations say ` +
          `${migrated.get(key(c)) ? "NOT NULL" : "nullable"}`
      );
    expect(mismatched).toEqual([]);
  }, 30_000);
});
