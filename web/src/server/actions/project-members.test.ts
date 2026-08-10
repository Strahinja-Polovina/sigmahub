// SIGMA-324: the code that SETS memberships.scoped, tested end to end.
//
// lib/rbac.test.ts covers effectiveProjectRole six ways, but every one of them
// passes `scoped` in as a literal — the argument is the thing under test, so
// the tests agree with whatever the caller decides to pass. Nothing covered the
// callers, and the callers are where the flag's real value is decided:
// setProjectRole writes `scoped: true` on the first grant, and
// restoreOrgWideAccess is the only path that clears it.
//
// Deleting the `.update(s.memberships).set({ scoped: true })` block from
// setProjectRole left both suites green while producing exactly the SIGMA-167
// regression the flag exists to prevent: grant a contractor Project Admin on
// one project, revoke it, and visibleProjects sees scoped=false with no grants,
// which it reads as "unscoped" — org-wide — so the contractor whose access was
// just revoked can see every project in the org.
//
// So the assertions here are made on visibleProjects and requireProjectRole,
// against the real migrated schema (PGlite, see @/server/testing/demo-db), and
// never on the column: the column is storage, the security outcome is what a
// revoked contractor can still reach.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { and, eq } from "drizzle-orm";

import * as s from "@/server/db/schema";
import { user as authUser } from "@/server/db/auth-schema";
import type { DemoDb } from "@/server/testing/demo-db";

const ORG = "org_acme";
const SHOP = "proj_shop";
const BILLING = "proj_billing";

const ADA = { id: "usr_ada", name: "Ada Admin", email: "ada@acme.example" };
/** The contractor: Project Admin at ORG level, so the org ceiling never hides
 *  what the project grants do. */
const CARL = { id: "usr_carl", name: "Carl Contractor", email: "carl@partner.example" };

/** Who the session belongs to. The actions take no actor argument — they read
 *  it from the session — so switching identity is how the test alternates
 *  between the admin doing the granting and the contractor being narrowed. */
let sessionUser: { id: string; name: string; email: string } = ADA;

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
// active-org itself is NOT mocked: requireProjectRole and visibleProjects are
// the assertions. Only the identity underneath it is faked, so the membership
// and scoping reads run against the real rows the actions wrote.
vi.mock("next/headers", () => ({
  headers: async () => new Headers(),
  cookies: async () => ({ get: () => undefined, set: () => {} }),
}));
vi.mock("next/navigation", () => ({
  redirect: () => {
    throw new Error("redirected to /login");
  },
}));
vi.mock("@/lib/auth", () => ({
  auth: { api: { getSession: async () => ({ user: sessionUser }) } },
}));

const { db } = (await import("@/server/db")) as unknown as { db: DemoDb };
const { visibleProjects, requireProjectRole } = await import("@/server/active-org");
const { setProjectRole, revokeProjectRole, restoreOrgWideAccess } = await import(
  "@/server/actions/project-members"
);

/** Carl's membership row, straight from the database. */
async function carlMembership() {
  const [m] = await db
    .select()
    .from(s.memberships)
    .where(and(eq(s.memberships.orgId, ORG), eq(s.memberships.userId, CARL.id)));
  return m;
}

beforeEach(async () => {
  sessionUser = ADA;
  await db.delete(s.projectMemberships);
  await db.delete(s.memberships);
  await db.delete(s.projects);
  await db.delete(s.orgs);
  await db.delete(authUser);

  const now = new Date();
  await db.insert(authUser).values(
    [ADA, CARL].map((u) => ({
      id: u.id,
      name: u.name,
      email: u.email,
      emailVerified: true,
      createdAt: now,
      updatedAt: now,
    }))
  );
  await db.insert(s.orgs).values({ id: ORG, name: "Acme", slug: "acme" });
  await db.insert(s.projects).values([
    { id: SHOP, orgId: ORG, name: "Shop", slug: "shop" },
    { id: BILLING, orgId: ORG, name: "Billing", slug: "billing" },
  ]);
  await db.insert(s.memberships).values([
    { id: "mem_ada", orgId: ORG, userId: ADA.id, role: "Org Admin" },
    { id: "mem_carl", orgId: ORG, userId: CARL.id, role: "Project Admin" },
  ]);
});

describe("project grant lifecycle", () => {
  it("granting a project role scopes the member to that project alone", async () => {
    await setProjectRole({ orgId: ORG, projectId: SHOP, userId: CARL.id, role: "Project Admin" });

    // Before the grant Carl was unscoped (org-wide). After it he is scoped, and
    // visibleProjects answers with a SET rather than the "everything" null.
    expect(await visibleProjects(CARL.id, ORG, "Project Admin")).toEqual(new Set([SHOP]));

    sessionUser = CARL;
    await expect(requireProjectRole(ORG, SHOP, "Project Admin")).resolves.toMatchObject({
      role: "Project Admin",
    });
    await expect(requireProjectRole(ORG, BILLING, "Developer")).rejects.toThrow(
      /do not have access/i
    );
  });

  it("revoking the last project grant leaves the member with no projects, not org-wide access", async () => {
    await setProjectRole({ orgId: ORG, projectId: SHOP, userId: CARL.id, role: "Project Admin" });
    await revokeProjectRole({ orgId: ORG, projectId: SHOP, userId: CARL.id });

    // The whole point of the explicit flag: zero grants and still scoped. A
    // null here would mean "every project in the org" — the SIGMA-167
    // regression, handed to somebody whose access was just taken away.
    const visible = await visibleProjects(CARL.id, ORG, "Project Admin");
    expect(visible).not.toBeNull();
    expect(visible).toEqual(new Set());
    expect((await carlMembership())?.scoped).toBe(true);

    sessionUser = CARL;
    await expect(requireProjectRole(ORG, SHOP, "Developer")).rejects.toThrow(/do not have access/i);
    await expect(requireProjectRole(ORG, BILLING, "Developer")).rejects.toThrow(
      /do not have access/i
    );
  });

  it("restoreOrgWideAccess is the one path that widens again", async () => {
    await setProjectRole({ orgId: ORG, projectId: SHOP, userId: CARL.id, role: "Project Admin" });
    await revokeProjectRole({ orgId: ORG, projectId: SHOP, userId: CARL.id });
    await restoreOrgWideAccess({ orgId: ORG, userId: CARL.id });

    expect(await visibleProjects(CARL.id, ORG, "Project Admin")).toBeNull();
    expect((await carlMembership())?.scoped).toBe(false);

    sessionUser = CARL;
    await expect(requireProjectRole(ORG, BILLING, "Project Admin")).resolves.toMatchObject({
      role: "Project Admin",
    });
  });

  it("a grant on one project does not widen the member onto the others", async () => {
    await setProjectRole({ orgId: ORG, projectId: SHOP, userId: CARL.id, role: "Developer" });

    // The grant narrows within the org ceiling too: Project Admin org-wide,
    // Developer here, so the Project Admin gate on the granted project refuses.
    sessionUser = CARL;
    await expect(requireProjectRole(ORG, SHOP, "Developer")).resolves.toMatchObject({
      role: "Developer",
    });
    await expect(requireProjectRole(ORG, SHOP, "Project Admin")).rejects.toThrow(
      /requires the Project Admin role/i
    );
  });
});
