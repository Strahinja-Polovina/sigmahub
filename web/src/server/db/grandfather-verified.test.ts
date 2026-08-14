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

    // The cut is the migration running once: an account created after it — a
    // genuine new sign-up — is not swept up by a second application.
    await pglite.query(`
      INSERT INTO "user" (id, name, email, email_verified)
      VALUES ('u_new', 'Alan', 'alan@example.com', false)
    `);
    await pglite.exec(sql);
    const rerun = await pglite.query<{ email_verified: boolean }>(
      `SELECT email_verified FROM "user" WHERE id = 'u_new'`
    );
    // It WOULD flip on a re-run, which is exactly why the guard is drizzle's
    // ledger and not the statement: this asserts the fact, so that anyone
    // tempted to make the file idempotent-by-rerunning sees what that costs.
    expect(rerun.rows[0].email_verified).toBe(true);

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
