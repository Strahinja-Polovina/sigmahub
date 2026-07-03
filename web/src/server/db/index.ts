// DB client. Dev = embedded PGlite (real Postgres semantics, no server).
// Prod = AlloyDB / Cloud SQL for PostgreSQL 18: set DATABASE_URL and swap the
// driver to drizzle-orm/node-postgres (see the branch below).
import { mkdirSync } from "node:fs";
import { PGlite } from "@electric-sql/pglite";
import { drizzle } from "drizzle-orm/pglite";
import * as schema from "./schema";
import * as authSchema from "./auth-schema";

const DATA_DIR = process.env.PGLITE_DIR ?? ".data/pg";

// Singleton across Next dev HMR / repeated imports. PGlite serializes its own
// query/transaction access internally (a shared exclusive lock), so concurrent
// reads from parallel server components are safe on one instance.
const g = globalThis as unknown as { __sigmaPglite?: PGlite };
mkdirSync(DATA_DIR, { recursive: true }); // PGlite's own mkdir isn't recursive
const client = g.__sigmaPglite ?? new PGlite(DATA_DIR);
if (process.env.NODE_ENV !== "production") g.__sigmaPglite = client;

const fullSchema = { ...schema, ...authSchema };
export const db = drizzle(client, {
  schema: fullSchema,
  logger: process.env.SQL_LOG === "1",
});
export { schema, authSchema, client };
