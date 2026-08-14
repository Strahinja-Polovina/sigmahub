// The rate limiter's counters must survive leaving the process (SIGMA-365).
//
// better-auth's limiter defaulted to an in-process map. That bounds ONE process,
// so with N replicas behind a proxy the effective sign-in limit becomes N × the
// configured one — and it degrades in silence, on a change (scaling `web` out)
// that nobody would connect to authentication. `rateLimit.storage: "database"`
// makes the limit a property of the deployment instead.
//
// Which moves the failure somewhere worse if the table is wrong. The drizzle
// adapter resolves better-auth's `rateLimit` model against our schema object and
// THROWS when it is missing or a field name does not line up — and it does that
// at the first rate-limited request, not at boot. So the first sign-in on the
// first deployment after this change would 500, and every sign-in after it.
//
// A config assertion cannot catch that: `storage: "database"` typechecks against
// any schema at all. This drives better-auth's own adapter against a real
// migrated database and does what the limiter does.

import { describe, expect, it } from "vitest";
import { PGlite } from "@electric-sql/pglite";
import { drizzle } from "drizzle-orm/pglite";
import { migrate } from "drizzle-orm/pglite/migrator";
import { drizzleAdapter } from "better-auth/adapters/drizzle";
import * as authSchema from "./auth-schema";
import * as schema from "./schema";

/** An adapter over a freshly migrated database, built exactly as lib/auth.ts
 *  builds it — same schema object, same provider. */
async function adapterOverMigratedDb() {
  const pglite = new PGlite();
  const db = drizzle(pglite);
  await migrate(db, { migrationsFolder: "drizzle" });
  const factory = drizzleAdapter(db as never, {
    provider: "pg",
    schema: { ...authSchema, ...schema },
  });
  // The options matter: better-auth only registers the `rateLimit` model at all
  // when storage is "database" (@better-auth/core get-tables.mjs), so passing
  // anything else here would test a model the adapter does not know about and
  // pass for the wrong reason.
  const adapter = factory({ rateLimit: { storage: "database" } } as never);
  return { adapter, db, pglite };
}

describe("better-auth's database rate-limit storage", () => {
  it("can write and read the counter row the limiter actually writes", async () => {
    const { adapter, pglite } = await adapterOverMigratedDb();

    // These three field names are better-auth's, not ours — see
    // node_modules/better-auth/dist/api/rate-limiter/index.mjs
    // (createDatabaseStorageWrapper). If the schema drifts from them the
    // adapter throws "The field ... does not exist in the rateLimit Drizzle
    // schema", at the first sign-in of the first deployment that ships it.
    const now = Date.now();
    await adapter.create({
      model: "rateLimit",
      data: { key: "ip-198.51.100.7-/sign-in/email", count: 1, lastRequest: now },
    });

    const rows = await adapter.findMany<{ key: string; count: number; lastRequest: number }>({
      model: "rateLimit",
      where: [{ field: "key", value: "ip-198.51.100.7-/sign-in/email" }],
    });
    expect(rows).toHaveLength(1);
    expect(rows[0].count).toBe(1);

    // lastRequest is epoch MILLISECONDS. The limiter compares it with Date.now()
    // arithmetic to decide whether a window has elapsed, so a column that came
    // back as a Date — which a timestamptz would — would typecheck and then
    // never expire a window, locking a caller out permanently after one burst.
    expect(typeof rows[0].lastRequest === "number" || typeof rows[0].lastRequest === "bigint").toBe(
      true
    );
    expect(Number(rows[0].lastRequest)).toBe(now);

    await pglite.close();
  }, 60_000);

  it("counts a second request against the same key rather than starting over", async () => {
    const { adapter, pglite } = await adapterOverMigratedDb();
    const key = "ip-203.0.113.9-/forget-password";
    await adapter.create({ model: "rateLimit", data: { key, count: 1, lastRequest: Date.now() } });

    await adapter.update({
      model: "rateLimit",
      where: [{ field: "key", value: key }],
      update: { count: 2, lastRequest: Date.now() },
    });

    const rows = await adapter.findMany<{ count: number }>({
      model: "rateLimit",
      where: [{ field: "key", value: key }],
    });
    // One row, count 2 — not two rows of 1, which is what a missing lookup path
    // would produce and would mean the limit never trips.
    expect(rows).toHaveLength(1);
    expect(rows[0].count).toBe(2);

    await pglite.close();
  }, 60_000);

  it("refuses a duplicate key at the database, which is what makes the counter atomic", async () => {
    // better-auth's consume path INSERTS and catches the conflict to re-read the
    // existing row. With only an ordinary index both concurrent inserts succeed,
    // the read takes whichever row comes back first, and the two callers each
    // count against their own copy — so the limit stops tripping exactly under
    // the concurrency an attacker supplies, while every serial test still passes.
    // The constraint is the mechanism, so it is asserted rather than assumed.
    const { pglite } = await adapterOverMigratedDb();
    const key = "ip-192.0.2.4-/sign-in/email";
    await pglite.query(
      `INSERT INTO rate_limit (id, key, count, last_request) VALUES ('a', $1, 1, 0)`,
      [key]
    );
    await expect(
      pglite.query(`INSERT INTO rate_limit (id, key, count, last_request) VALUES ('b', $1, 1, 0)`, [
        key,
      ])
    ).rejects.toThrow(/unique|duplicate/i);

    await pglite.close();
  }, 60_000);
});
