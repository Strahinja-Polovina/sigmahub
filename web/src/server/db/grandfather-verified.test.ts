// The upgrade must not lock the install out of itself (SIGMA-365).
//
// Email verification used to be opt-in. It now defaults ON wherever mail can be
// delivered, which means an existing deployment acquires the requirement on the
// day its operator sets SMTP_HOST + SMTP_FROM — a change made to FIX mail, with
// no hint that it touches sign-in. Every existing row carries email_verified =
// false at that moment, because with verification off nothing ever set it, so
// better-auth refuses every account at /sign-in/email. Including the operator's.
//
// 0010_grandfather_verified_emails.sql is what stops that. This asserts it does
// the thing it is named for against the real migrated schema, rather than
// trusting a statement nobody runs until an upgrade morning.

import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { PGlite } from "@electric-sql/pglite";
import { drizzle } from "drizzle-orm/pglite";
import { migrate } from "drizzle-orm/pglite/migrator";

const MIGRATION = "drizzle/0010_grandfather_verified_emails.sql";

describe("the grandfathering migration", () => {
  it("verifies accounts that predate the requirement, and is a no-op afterwards", async () => {
    const pglite = new PGlite();
    await migrate(drizzle(pglite), { migrationsFolder: "drizzle" });

    // Two accounts as an upgrading deployment has them: created under rules that
    // never asked for verification, so both sit at false.
    await pglite.query(`
      INSERT INTO "user" (id, name, email, email_verified)
      VALUES ('u_admin', 'Ada', 'ada@example.com', false),
             ('u_member', 'Grace', 'grace@example.com', false)
    `);

    const sql = fs.readFileSync(path.join(process.cwd(), MIGRATION), "utf8");
    await pglite.exec(sql);

    const after = await pglite.query<{ email: string; email_verified: boolean }>(
      `SELECT email, email_verified FROM "user" ORDER BY email`
    );
    expect(after.rows).toEqual([
      { email: "ada@example.com", email_verified: true },
      { email: "grace@example.com", email_verified: true },
    ]);

    await pglite.close();
  }, 60_000);

  // The back door this would have been without its guard.
  //
  // On a deployment that had ALREADY turned verification on, an unverified row
  // does not mean "we never asked" — it means "this person never proved the
  // address". `email_verified` is load-bearing for exactly one control, the
  // invite email-match, and that control exists so someone holding a leaked
  // invite link cannot register the invited address and walk into the org.
  // Verifying those rows hands them precisely what it withholds.
  it("leaves genuinely unverified accounts alone where verification was already on", async () => {
    const pglite = new PGlite();
    await migrate(drizzle(pglite), { migrationsFolder: "drizzle" });

    // The signature of a deployment that has been verifying: someone completed
    // it. The one who did not is unverified on purpose.
    await pglite.query(`
      INSERT INTO "user" (id, name, email, email_verified)
      VALUES ('u_ok', 'Ada', 'ada@example.com', true),
             ('u_pending', 'Mallory', 'invited@example.com', false)
    `);

    const sql = fs.readFileSync(path.join(process.cwd(), MIGRATION), "utf8");
    await pglite.exec(sql);

    const after = await pglite.query<{ id: string; email_verified: boolean }>(
      `SELECT id, email_verified FROM "user" ORDER BY id`
    );
    expect(after.rows).toEqual([
      { id: "u_ok", email_verified: true },
      { id: "u_pending", email_verified: false },
    ]);

    await pglite.close();
  }, 60_000);

  it("is registered in the journal, or it never runs at all", async () => {
    const journal = JSON.parse(
      fs.readFileSync(path.join(process.cwd(), "drizzle/meta/_journal.json"), "utf8")
    ) as { entries: { tag: string }[] };
    expect(journal.entries.map((e) => e.tag)).toContain(
      "0010_grandfather_verified_emails"
    );
  });
});
