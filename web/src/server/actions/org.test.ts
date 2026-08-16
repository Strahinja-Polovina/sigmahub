// SIGMA-306: creating a second organization from inside the product.
//
// `orgs` had exactly two writers before this: the seed script, and
// ensurePersonalOrg, which fires once at first login. A consultant with a
// second client had no way to get a second org — the switcher's
// "New organization" item linked to the CURRENT org's settings page, whose
// name field renames the org they already had.
//
// The action is tested against the real migrated schema (PGlite, see
// @/server/testing/demo-db) because what matters is the rows: an org row AND an
// Org Admin membership for the caller, in one transaction, with a unique slug.
// A create that lands the org but not the membership produces an org nobody —
// including its creator — can see.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { and, eq } from "drizzle-orm";

import * as s from "@/server/db/schema";
import { user as authUser } from "@/server/db/auth-schema";
import type { DemoDb } from "@/server/testing/demo-db";
import { unwrap } from "@/lib/action-result";
import { MAX_ORGS_PER_USER } from "@/lib/org-limits";

const ADA = { id: "usr_ada", name: "Ada Admin", email: "ada@acme.example" };

// "@/server/db" and "../db" are the same module here — vitest keys mocks by
// resolved path, so registering the factory once covers both specifiers.
vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));

/** The cookie jar the action writes the active org into. */
const cookieJar = new Map<string, string>();
vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (k: string) => (cookieJar.has(k) ? { name: k, value: cookieJar.get(k) } : undefined),
    set: (k: string, v: string) => void cookieJar.set(k, v),
  }),
}));

// active-org pulls in better-auth through lib/auth; identity is the only thing
// createOrg needs from it (there is no membership to check — the org does not
// exist yet).
vi.mock("@/server/active-org", () => ({
  ORG_COOKIE: "sh_org",
  getSessionUser: async () => ADA,
}));

const { db } = (await import("@/server/db")) as unknown as { db: DemoDb };
const { createOrg } = await import("@/server/actions/org");

beforeEach(async () => {
  cookieJar.clear();
  await db.delete(s.memberships);
  await db.delete(s.orgs);
  await db.delete(authUser);
  await db.insert(authUser).values({
    id: ADA.id,
    name: ADA.name,
    email: ADA.email,
    emailVerified: true,
    createdAt: new Date(),
    updatedAt: new Date(),
  });
});

describe("createOrg", () => {
  it("inserts an org and an Org Admin membership for the caller", async () => {
    const { orgId } = unwrap(await createOrg({ name: "Beta Client" }));

    const [org] = await db.select().from(s.orgs).where(eq(s.orgs.id, orgId));
    expect(org?.name).toBe("Beta Client");
    expect(org?.slug).toBeTruthy();

    const [member] = await db
      .select()
      .from(s.memberships)
      .where(and(eq(s.memberships.orgId, orgId), eq(s.memberships.userId, ADA.id)));
    // Without the membership the creator cannot even see the org they made.
    expect(member?.role).toBe("Org Admin");
    // Org-wide, not project-scoped: there are no projects to be scoped to yet.
    expect(member?.scoped).toBe(false);

    // And the new org is the active one, so the switcher lands inside it.
    expect(cookieJar.get("sh_org")).toBe(orgId);
  });

  it("rejects a blank name instead of creating a nameless org", async () => {
    const res = await createOrg({ name: "   " });
    expect(res.ok).toBe(false);
    expect(res.ok ? "" : res.error).toMatch(/name is required/i);
    expect(await db.select().from(s.orgs)).toHaveLength(0);
  });

  it("gives a second org with the same name its own slug", async () => {
    const a = unwrap(await createOrg({ name: "Beta Client" }));
    const b = unwrap(await createOrg({ name: "Beta Client" }));
    expect(b.orgId).not.toBe(a.orgId);

    const rows = await db.select().from(s.orgs);
    expect(rows).toHaveLength(2);
    expect(new Set(rows.map((r) => r.slug)).size).toBe(2);
  });

  // SIGMA-365. The ceiling that makes the OTHER ceilings mean something.
  //
  // Every abuse limit added for the public launch is scoped per organization:
  // the free tier bounds units per org, and the outbound-mail budget bounds
  // invite email per org. Both were designed and tested against a fixed org, and
  // neither noticed that this action mints orgs for the price of a session. A
  // loop here is unlimited free capacity and an unlimited mail cannon at the
  // same time — the exact two things those limits were written to prevent.
  it("stops one account minting organizations without bound", async () => {
    for (let i = 0; i < MAX_ORGS_PER_USER; i++) {
      const res = await createOrg({ name: `Client ${i}` });
      expect(res.ok, `org ${i} should be allowed — the limit is ${MAX_ORGS_PER_USER}`).toBe(true);
    }
    const res = await createOrg({ name: "One too many" });
    expect(res.ok).toBe(false);
    // The refusal has to name a route forward, like every other limit here.
    expect(res.ok ? "" : res.error).toMatch(/invite you|contact support/i);
    expect(await db.select().from(s.orgs)).toHaveLength(MAX_ORGS_PER_USER);
  });
});
