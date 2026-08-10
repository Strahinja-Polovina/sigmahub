// SIGMA-325 lives beside project-members.test.ts rather than inside it.
//
// SIGMA-324 wrote a project-members.test.ts of its own in the same wave, and
// the two cannot share a module: this file mocks "@/server/active-org" and
// "@/server/audit", that one mocks "next/headers", "next/navigation" and
// "@/lib/auth", and both register a "@/server/db" factory. Merged into one file
// the later vi.mock silently wins and half the suite asserts against a fixture
// it was not written for.

// What a project grant can and cannot do (SIGMA-325).
//
// Three rules live in this module and nowhere else, and none of them had a
// test. Removing the org-membership check from setProjectRole AND the
// org-scoping subquery from restoreOrgWideAccess left all 753 web tests green
// (verified), so both of these were free to delete:
//
//   * a project grant is a NARROWING of an existing org membership, never a
//     way into the org — grant a project role to a userId from another tenant
//     and they are inside;
//   * restoreOrgWideAccess deletes the member's grants only within THIS org,
//     because a person can legitimately be a member of several, and widening
//     them here must not silently strip their access somewhere else;
//   * the `scoped` flag is explicit state (SIGMA-167) — revoking the last
//     grant leaves a member scoped, seeing nothing, rather than silently
//     re-widening them to every project in the org.
//
// The third one is the reason the flag exists at all: it replaced a live grant
// COUNT that made "revoke their last project" mean "give them everything".

import { beforeEach, describe, expect, it, vi } from "vitest";
import { and, eq } from "drizzle-orm";

import * as s from "@/server/db/schema";
import { user } from "@/server/db/auth-schema";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/audit", () => ({ writeAudit: async () => {} }));
vi.mock("@/server/active-org", () => {
  const actor = { user: { id: "usr_admin", name: "Admin" }, role: "Org Admin" };
  return {
    requireProjectRole: async () => actor,
    requireOrgAdmin: async () => ({ id: "usr_admin", name: "Admin", role: "Org Admin" }),
  };
});

const { db } = await import("@/server/db");
const { setProjectRole, revokeProjectRole, restoreOrgWideAccess } = await import(
  "@/server/actions/project-members"
);

const d = () => db as unknown as DemoDb;

const MEMBER = "usr_contractor";
const OUTSIDER = "usr_outsider";
/** A project in the OTHER org, where the member also holds a grant. */
const RIVAL_PROJECT = "proj_rival";

const membership = async () =>
  (
    await d()
      .select()
      .from(s.memberships)
      .where(and(eq(s.memberships.orgId, FIXTURE.orgId), eq(s.memberships.userId, MEMBER)))
  )[0];

const grants = async (userId = MEMBER) =>
  d().select().from(s.projectMemberships).where(eq(s.projectMemberships.userId, userId));

beforeEach(async () => {
  for (const t of [
    s.projectMemberships,
    s.memberships,
    s.resources,
    s.envServers,
    s.environments,
    s.projects,
    s.servers,
    s.orgs,
    user,
  ]) {
    await d().delete(t);
  }
  await seedDemoFixture(d());
  await d()
    .insert(user)
    .values([
      { id: MEMBER, name: "Contractor", email: "contractor@example.com", emailVerified: true },
      { id: OUTSIDER, name: "Outsider", email: "outsider@example.com", emailVerified: true },
    ]);
  // The contractor belongs to BOTH orgs — the situation the org-scoped delete
  // in restoreOrgWideAccess exists for.
  await d()
    .insert(s.memberships)
    .values([
      { id: "mem_a", orgId: FIXTURE.orgId, userId: MEMBER, role: "Developer" },
      { id: "mem_b", orgId: FIXTURE.rivalOrgId, userId: MEMBER, role: "Developer" },
    ]);
  await d()
    .insert(s.projects)
    .values({ id: RIVAL_PROJECT, orgId: FIXTURE.rivalOrgId, name: "Rival", slug: "rival" });
});

describe("setProjectRole", () => {
  it("refuses a user who is not a member of the organization", async () => {
    await expect(
      setProjectRole({
        orgId: FIXTURE.orgId,
        projectId: FIXTURE.projectId,
        userId: OUTSIDER,
        role: "Developer",
      })
    ).rejects.toThrow(/not a member of this organization/);
    expect(await grants(OUTSIDER)).toHaveLength(0);
  });

  it("refuses a role that is not a project role", async () => {
    await expect(
      setProjectRole({
        orgId: FIXTURE.orgId,
        projectId: FIXTURE.projectId,
        userId: MEMBER,
        // Org Admin is an ORG role; granting it here would read as a promotion
        // the project gate never authorized.
        role: "Org Admin",
      })
    ).rejects.toThrow(/Project Admin or Developer/);
    expect(await grants()).toHaveLength(0);
  });

  it("grants the role and scopes the member", async () => {
    await setProjectRole({
      orgId: FIXTURE.orgId,
      projectId: FIXTURE.projectId,
      userId: MEMBER,
      role: "Project Admin",
    });
    const g = await grants();
    expect(g).toHaveLength(1);
    expect(g[0].role).toBe("Project Admin");
    expect((await membership()).scoped).toBe(true);
  });

  it("changes an existing grant in place rather than duplicating it", async () => {
    for (const role of ["Project Admin", "Developer"]) {
      await setProjectRole({
        orgId: FIXTURE.orgId,
        projectId: FIXTURE.projectId,
        userId: MEMBER,
        role,
      });
    }
    const g = await grants();
    expect(g).toHaveLength(1);
    expect(g[0].role).toBe("Developer");
  });
});

describe("revokeProjectRole", () => {
  it("leaves the member scoped after their LAST grant is revoked", async () => {
    await setProjectRole({
      orgId: FIXTURE.orgId,
      projectId: FIXTURE.projectId,
      userId: MEMBER,
      role: "Developer",
    });
    await revokeProjectRole({
      orgId: FIXTURE.orgId,
      projectId: FIXTURE.projectId,
      userId: MEMBER,
    });

    expect(await grants()).toHaveLength(0);
    // SIGMA-167 in one assertion: narrowing to zero projects means zero, not
    // "back to every project in the org".
    expect((await membership()).scoped).toBe(true);
  });
});

describe("restoreOrgWideAccess", () => {
  it("clears scoping and removes this org's grants", async () => {
    await setProjectRole({
      orgId: FIXTURE.orgId,
      projectId: FIXTURE.projectId,
      userId: MEMBER,
      role: "Project Admin",
    });

    await restoreOrgWideAccess({ orgId: FIXTURE.orgId, userId: MEMBER });

    expect((await membership()).scoped).toBe(false);
    expect(
      (await grants()).filter((g) => g.projectId === FIXTURE.projectId)
    ).toHaveLength(0);
  });

  it("does not touch the member's grants in another organization", async () => {
    await d().insert(s.projectMemberships).values({
      id: "pm_rival",
      projectId: RIVAL_PROJECT,
      userId: MEMBER,
      role: "Project Admin",
    });
    await setProjectRole({
      orgId: FIXTURE.orgId,
      projectId: FIXTURE.projectId,
      userId: MEMBER,
      role: "Developer",
    });

    await restoreOrgWideAccess({ orgId: FIXTURE.orgId, userId: MEMBER });

    // Widening someone inside one org must not quietly revoke them in another:
    // the delete is keyed on userId and is only safe because it is filtered to
    // this org's projects.
    const rival = (await grants()).filter((g) => g.projectId === RIVAL_PROJECT);
    expect(rival).toHaveLength(1);
    expect(rival[0].role).toBe("Project Admin");
    // …and the other org's membership keeps its own scoping state.
    const rivalMem = (
      await d()
        .select()
        .from(s.memberships)
        .where(
          and(eq(s.memberships.orgId, FIXTURE.rivalOrgId), eq(s.memberships.userId, MEMBER))
        )
    )[0];
    expect(rivalMem.scoped).toBe(false);
  });
});
