// DB client. Demo/dev = embedded PGlite (real Postgres semantics, no server).
// Production = ANY PostgreSQL: set DATABASE_URL and the node-postgres driver
// takes over behind the same drizzle surface — the PGlite demo path is
// untouched (the dual-mode contract).
import { mkdirSync } from "node:fs";
import { PGlite } from "@electric-sql/pglite";
import { drizzle as drizzlePglite } from "drizzle-orm/pglite";
import { drizzle as drizzleNodePg, type NodePgDatabase } from "drizzle-orm/node-postgres";
import { Pool } from "pg";
import * as schema from "./schema";
import * as authSchema from "./auth-schema";

const DATABASE_URL = process.env.DATABASE_URL;
const DATA_DIR = process.env.PGLITE_DIR ?? ".data/pg";

const fullSchema = { ...schema, ...authSchema };
type DB = NodePgDatabase<typeof fullSchema>;

// RawClient is the least-common raw-SQL surface the app uses beside drizzle
// (cp.ts token cache, demo secrets, the seed script); both PGlite and pg.Pool
// satisfy it.
export type RawClient = {
  query<T = Record<string, unknown>>(sql: string, params?: unknown[]): Promise<{ rows: T[] }>;
  close(): Promise<void>;
};

// Singletons across Next dev HMR / repeated imports — and across route chunks
// in `next start`, where each compiled route would otherwise open its own
// client over the same backend and (for PGlite) read a stale snapshot of the
// others' writes. PGlite serializes its own query/transaction access
// internally; pg.Pool pools per process.
const g = globalThis as unknown as {
  __sigmaPglite?: PGlite;
  __sigmaPgPool?: Pool;
};

let dbInstance: DB;
let rawClient: RawClient;

if (DATABASE_URL) {
  const pool = (g.__sigmaPgPool ??= new Pool({ connectionString: DATABASE_URL }));
  dbInstance = drizzleNodePg(pool, {
    schema: fullSchema,
    logger: process.env.SQL_LOG === "1",
  });
  rawClient = {
    query: async <T,>(sql: string, params?: unknown[]) => {
      const res = await pool.query(sql, params as unknown[]);
      return { rows: res.rows as T[] };
    },
    close: () => pool.end(),
  };
} else {
  mkdirSync(DATA_DIR, { recursive: true }); // PGlite's own mkdir isn't recursive
  const pglite = (g.__sigmaPglite ??= new PGlite(DATA_DIR));
  // The PGlite drizzle instance is structurally identical at every call site;
  // the node-postgres type is the common denominator we export.
  dbInstance = drizzlePglite(pglite, {
    schema: fullSchema,
    logger: process.env.SQL_LOG === "1",
  }) as unknown as DB;
  rawClient = {
    query: async <T,>(sql: string, params?: unknown[]) => {
      const res = await pglite.query(sql, params);
      return { rows: res.rows as T[] };
    },
    close: () => pglite.close(),
  };
}

export const db = dbInstance;
export const client = rawClient;
export { schema, authSchema };
