// SIGMA-284: erasure and export for customer personal data.
//
// Before this file there was no deleteUser, no deleteOrganization and no export
// anywhere in the tree. The closest thing, removeMember, drops a membership and
// its project grants and leaves the person: running it and then scanning every
// text column in the schema for their address still found it in user.email,
// account.account_id, verification.identifier and invitations.email — four
// tables, in two of which nothing cascades from anything.
//
// So the assertions here are made against the SCHEMA rather than against a list
// of tables somebody remembered to update: information_schema enumerates every
// text-ish column in the database and each one is checked for the address. A
// table added next quarter that stores an email fails this test, which is the
// only way an inventory stays true.
//
// The database is the real migrated one (PGlite, see @/server/testing/demo-db),
// because what broke here is not a helper — it is which rows a delete reaches.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { eq, sql } from "drizzle-orm";

import * as s from "@/server/db/schema";
import * as auth from "@/server/db/auth-schema";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";

const DANA = { id: "usr_dana", name: "Dana Reeve", email: "dana@partner.example" };
const ADMIN = { id: "usr_admin", name: "Ada Admin", email: "ada@acme.example" };

// Who the session belongs to. deleteUser is self-service, so the test drives it
// by changing this rather than by passing a user id the action does not accept.
let sessionUser: { id: string; name: string; email: string } = DANA;

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/active-org", () => ({
  getSessionUser: async () => sessionUser,
  requireOrgAdmin: async () => sessionUser,
  requireMembership: async () => ({ user: sessionUser, role: "Org Admin", scoped: false }),
}));
// No control plane in these tests: cpPurgeOrg throwing is the assertion that
// the web path never reaches for it when SIGMAHUB_CP_URL is unset.
vi.mock("@/server/cp", () => ({
  cpEnabled: () => false,
  cpPurgeOrg: async () => {
    throw new Error("the CP client must not be called in demo mode");
  },
}));

const { db } = (await import("@/server/db")) as unknown as { db: DemoDb };
const { deleteUser, deleteOrganization, exportUserData, exportOrganization } =
  await import("@/server/actions/privacy");

/** Every text-ish column in the schema whose value equals `needle`, as
 *  "table.column". The inventory check: nothing is exempt because nobody
 *  thought of it. */
async function columnsHolding(needle: string): Promise<string[]> {
  const cols = await db.execute<{ table_name: string; column_name: string }>(sql`
    SELECT table_name, column_name FROM information_schema.columns
     WHERE table_schema = 'public'
       AND data_type IN ('text', 'character varying', 'character')
     ORDER BY table_name, column_name
  `);
  const hits: string[] = [];
  for (const c of rowsOf<{ table_name: string; column_name: string }>(cols)) {
    const q = await db.execute(
      sql.raw(
        `SELECT count(*)::int AS n FROM "${c.table_name}" WHERE "${c.column_name}" = '${needle}'`
      )
    );
    if (Number(rowsOf<{ n: number }>(q)[0]?.n ?? 0) > 0) {
      hits.push(`${c.table_name}.${c.column_name}`);
    }
  }
  return hits;
}

/** drizzle's execute returns a driver result whose shape differs between
 *  node-postgres and PGlite; both carry the tuples on `.rows`. */
function rowsOf<T>(result: unknown): T[] {
  const r = result as { rows?: T[] };
  return Array.isArray(r?.rows) ? r.rows : ((result as T[]) ?? []);
}

async function seed() {
  // Wipe between tests: PGlite is created once for the module.
  for (const t of [
    "audit_log",
    "invitations",
    "project_memberships",
    "memberships",
    "verification",
    "two_factor",
    "account",
    "session",
    "cluster_nodes",
    "clusters",
    "deployments",
    "resources",
    "env_servers",
    "environments",
    "projects",
    "servers",
    "orgs",
    '"user"',
  ]) {
    await db.execute(sql.raw(`DELETE FROM ${t}`));
  }
  await seedDemoFixture(db);
  await db.insert(auth.user).values([DANA, ADMIN]);
  await db.insert(auth.session).values({
    id: "ses_1",
    userId: DANA.id,
    token: "tok_1",
    expiresAt: new Date(Date.now() + 3_600_000),
    updatedAt: new Date(),
    ipAddress: "203.0.113.7",
    userAgent: "Mozilla/5.0 (X11; Linux x86_64)",
  });
  await db.insert(auth.account).values({
    id: "acc_1",
    accountId: DANA.email,
    providerId: "credential",
    userId: DANA.id,
    password: "argon2id$v=19$m=65536$hash",
    updatedAt: new Date(),
  });
  await db.insert(auth.twoFactor).values({
    id: "tf_1",
    userId: DANA.id,
    secret: "JBSWY3DPEHPK3PXP",
    backupCodes: "aaaa-bbbb,cccc-dddd",
  });
  await db.insert(auth.verification).values({
    id: "ver_1",
    identifier: DANA.email,
    value: "reset-token",
    expiresAt: new Date(Date.now() + 3_600_000),
    updatedAt: new Date(),
  });
  await db.insert(s.memberships).values([
    { id: "mem_dana", orgId: FIXTURE.orgId, userId: DANA.id, role: "Developer" },
    { id: "mem_admin", orgId: FIXTURE.orgId, userId: ADMIN.id, role: "Org Admin" },
  ]);
  await db.insert(s.projectMemberships).values({
    id: "pm_dana",
    projectId: FIXTURE.projectId,
    userId: DANA.id,
    role: "Developer",
  });
  await db.insert(s.invitations).values([
    {
      id: "inv_accepted",
      orgId: FIXTURE.orgId,
      email: DANA.email,
      tokenHash: "hash_accepted",
      invitedBy: ADMIN.name,
      status: "accepted",
      expiresAt: new Date(Date.now() + 3_600_000),
    },
    // An invite to a DIFFERENT org she never joined: no membership ties it to
    // her, so a membership-shaped delete leaves the address behind.
    {
      id: "inv_pending_elsewhere",
      orgId: FIXTURE.rivalOrgId,
      email: DANA.email,
      tokenHash: "hash_pending",
      invitedBy: "Someone Else",
      status: "pending",
      expiresAt: new Date(Date.now() + 3_600_000),
    },
  ]);
  await db.insert(s.auditLog).values([
    {
      id: "aud_1",
      orgId: FIXTURE.orgId,
      actor: DANA.name,
      action: "Revealed secret",
      target: "DB_PASSWORD",
    },
    {
      id: "aud_2",
      orgId: FIXTURE.orgId,
      actor: ADMIN.name,
      action: "Changed role",
      target: `${DANA.name} → Developer`,
    },
  ]);
}

beforeEach(async () => {
  sessionUser = DANA;
  await seed();
});

describe("deleteUser", () => {
  it("leaves no row referencing the person's email anywhere in the schema", async () => {
    await deleteUser({ confirmEmail: DANA.email });
    expect(await columnsHolding(DANA.email)).toEqual([]);
  });

  it("takes the credential material with the account", async () => {
    await deleteUser({ confirmEmail: DANA.email });
    for (const [what, rows] of [
      ["session (IP address, user agent)", await db.select().from(auth.session)],
      ["account (password hash)", await db.select().from(auth.account)],
      ["two_factor (secret, backup codes)", await db.select().from(auth.twoFactor)],
      ["verification (email in identifier)", await db.select().from(auth.verification)],
    ] as const) {
      expect(rows, what).toEqual([]);
    }
  });

  it("redacts the person from an audit trail that outlives them, without deleting the record", async () => {
    await deleteUser({ confirmEmail: DANA.email });
    const rows = await db.select().from(s.auditLog).where(eq(s.auditLog.orgId, FIXTURE.orgId));
    // Both rows survive — they are the org's record of what happened, and one
    // of them is somebody else's action.
    expect(rows).toHaveLength(2);
    const text = rows.map((r) => `${r.actor}|${r.target}`).join("\n");
    expect(text).not.toContain(DANA.name);
    // The substring case: "Dana Reeve → Developer" is not an exact match on the
    // name, and an equality-based redaction would leave it intact.
    expect(text).toContain("Deleted user → Developer");
    expect(text).toContain(ADMIN.name);
  });

  it("refuses without the typed confirmation, and changes nothing", async () => {
    await expect(deleteUser({ confirmEmail: "someone@else.example" })).rejects.toThrow(
      /confirm/i
    );
    expect(await columnsHolding(DANA.email)).not.toEqual([]);
  });

  it("refuses to strand an org whose only admin is the person leaving", async () => {
    sessionUser = ADMIN;
    await expect(deleteUser({ confirmEmail: ADMIN.email })).rejects.toThrow(/only admin/i);
    const [org] = await db.select().from(s.orgs).where(eq(s.orgs.id, FIXTURE.orgId));
    expect(org, "the org must survive a refused erasure").toBeTruthy();
  });

  it("deletes an org the person was the sole member of", async () => {
    // Ada is the ONLY member of the rival org, and one of two admins of Acme —
    // so erasing her must take the rival org with her and leave Acme standing.
    await db
      .insert(s.memberships)
      .values({ id: "mem_solo", orgId: FIXTURE.rivalOrgId, userId: ADMIN.id, role: "Org Admin" });
    await db
      .update(s.memberships)
      .set({ role: "Org Admin" })
      .where(eq(s.memberships.userId, DANA.id));

    sessionUser = ADMIN;
    await deleteUser({ confirmEmail: ADMIN.email });
    const rival = await db.select().from(s.orgs).where(eq(s.orgs.id, FIXTURE.rivalOrgId));
    expect(rival, "a sole-member org goes with its last member").toEqual([]);
    const acme = await db.select().from(s.orgs).where(eq(s.orgs.id, FIXTURE.orgId));
    expect(acme, "an org with other members survives").toHaveLength(1);
  });
});

describe("deleteOrganization", () => {
  it("removes the org's rows including the audit log no cascade reaches", async () => {
    sessionUser = ADMIN;
    await deleteOrganization({ orgId: FIXTURE.orgId, confirmName: "Acme" });
    expect(await db.select().from(s.orgs).where(eq(s.orgs.id, FIXTURE.orgId))).toEqual([]);
    // audit_log.org_id has no foreign key, so the org cascade never touched it.
    expect(
      await db.select().from(s.auditLog).where(eq(s.auditLog.orgId, FIXTURE.orgId))
    ).toEqual([]);
    expect(
      await db.select().from(s.memberships).where(eq(s.memberships.orgId, FIXTURE.orgId))
    ).toEqual([]);
    expect(
      await db.select().from(s.servers).where(eq(s.servers.orgId, FIXTURE.orgId))
    ).toEqual([]);
    // The other tenant is untouched.
    expect(
      await db.select().from(s.orgs).where(eq(s.orgs.id, FIXTURE.rivalOrgId))
    ).toHaveLength(1);
  });

  it("refuses when the typed name does not match", async () => {
    sessionUser = ADMIN;
    await expect(
      deleteOrganization({ orgId: FIXTURE.orgId, confirmName: "acme" })
    ).rejects.toThrow(/name exactly/i);
    expect(await db.select().from(s.orgs).where(eq(s.orgs.id, FIXTURE.orgId))).toHaveLength(1);
  });
});

describe("export", () => {
  it("gives the person their own data and withholds credential material", async () => {
    const out = await exportUserData();
    expect((out.account as { email: string }).email).toBe(DANA.email);
    expect(out.sessions).toHaveLength(1);
    expect((out.sessions as { ipAddress: string }[])[0].ipAddress).toBe("203.0.113.7");
    expect(out.organizations).toHaveLength(1);
    expect(out.projectGrants).toHaveLength(1);
    // The export must never become an exfiltration path for the things that
    // authenticate the account.
    expect(JSON.stringify(out)).not.toContain("argon2id");
    expect(JSON.stringify(out)).not.toContain("JBSWY3DPEHPK3PXP");
    expect(out.excluded).toContain("password hash");
  });

  it("gives an org admin the whole org, audit log included and untruncated", async () => {
    sessionUser = ADMIN;
    const out = await exportOrganization({ orgId: FIXTURE.orgId });
    expect((out.members as unknown[]).map((m) => (m as { email: string }).email)).toContain(
      DANA.email
    );
    expect(out.invitations).toHaveLength(1); // the rival org's invite is not ours
    expect((out.auditLog as unknown[]).length).toBeGreaterThanOrEqual(2);
    expect(out.servers).not.toEqual([]);
  });
});
