// Who acceptInvite lets into an organization (SIGMA-325).
//
// lib/invite.test.ts covers the pure helpers — hashInviteToken, inviteRejection,
// sameEmail, parseProjectGrants — and nothing covered the action that USES
// them. The distance between the two is the whole security property: deleting
// the `sameEmail(sessionUser.email, inv.email)` check from acceptInvite leaves
// every web test green (verified: 745 passed with the guard removed), and turns
// an invite link into a bearer token for someone else's organization. A link
// forwarded, pasted into a ticket, or read out of a shared inbox is then a
// membership at the invited role — Org Admin included.
//
// The other properties here are the same shape: they are enforced by the
// action's transaction, not by any helper, so no helper test can see them.
// One-time use, the scoped flag an invite with grants sets (SIGMA-167), and the
// stale-grant skip that must not block joining the org itself.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { and, eq } from "drizzle-orm";

import * as s from "@/server/db/schema";
import { user } from "@/server/db/auth-schema";
import { hashInviteToken, INVITE_TTL_MS } from "@/lib/invite";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";
import { unwrap } from "@/lib/action-result";

/** Who is signed in when the invite link is opened. */
const session = vi.hoisted(() => ({
  id: "usr_invitee",
  name: "Invitee",
  email: "invitee@example.com",
}));

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/audit", () => ({ writeAudit: async () => {} }));
vi.mock("@/server/email", () => ({
  sendInviteEmail: async () => ({ delivered: false }),
}));
vi.mock("@/server/active-org", () => ({
  getSessionUser: async () => session,
  requireOrgAdmin: async () => ({ id: "usr_admin", name: "Admin", role: "Org Admin" }),
}));

const { db } = await import("@/server/db");
const { acceptInvite } = await import("@/server/actions/members");

const d = () => db as unknown as DemoDb;


/** An action's refusal message. Actions return `{ ok: false, error }` rather
 *  than throwing, because Next redacts a thrown server-action error in
 *  production and these sentences exist to be read (SIGMA-365). */
async function refusal(p: Promise<{ ok: boolean } | { ok: false; error: string }>): Promise<string> {
  const res = (await p) as { ok: boolean; error?: string };
  expect(res.ok, "expected the action to refuse, but it succeeded").toBe(false);
  return res.error ?? "";
}

const TOKEN = "raw-invite-token-for-the-test";

/** Write a pending invitation straight to the table — inviteMember's own path
 *  is not what is under test, and this keeps the token deterministic. */
async function seedInvite(over: Partial<typeof s.invitations.$inferInsert> = {}) {
  const row = {
    id: "inv_1",
    orgId: FIXTURE.orgId,
    email: "invitee@example.com",
    role: "Project Admin",
    projectGrants: "[]",
    tokenHash: hashInviteToken(TOKEN),
    invitedBy: "Admin",
    status: "pending",
    expiresAt: new Date(Date.now() + INVITE_TTL_MS),
    ...over,
  };
  await d().insert(s.invitations).values(row);
  return row;
}

const membership = async (userId = session.id) =>
  (
    await d()
      .select()
      .from(s.memberships)
      .where(and(eq(s.memberships.orgId, FIXTURE.orgId), eq(s.memberships.userId, userId)))
  )[0];

const inviteRow = async () =>
  (await d().select().from(s.invitations).where(eq(s.invitations.id, "inv_1")))[0];

beforeEach(async () => {
  for (const t of [
    s.projectMemberships,
    s.invitations,
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
  await d().insert(user).values({
    id: session.id,
    name: session.name,
    email: session.email,
    emailVerified: true,
  });
  session.email = "invitee@example.com";
});

describe("acceptInvite", () => {
  it("refuses an invite addressed to a different email", async () => {
    await seedInvite({ email: "someone.else@example.com" });

    expect(await refusal(acceptInvite({ token: TOKEN }))).toMatch(
      /sent to someone\.else@example\.com/
    );
    // Nothing partially applied: no membership, and the invite is still live
    // for the person it was actually sent to.
    expect(await membership()).toBeUndefined();
    expect((await inviteRow()).status).toBe("pending");
  });

  it("matches the invited email case-insensitively", async () => {
    await seedInvite({ email: "Invitee@Example.COM" });

    await expect(acceptInvite({ token: TOKEN })).resolves.toEqual({ ok: true, orgId: FIXTURE.orgId });
    expect((await membership()).role).toBe("Project Admin");
  });

  it("is one-time: the same link cannot be accepted twice", async () => {
    await seedInvite();
    unwrap(await acceptInvite({ token: TOKEN }));
    expect((await inviteRow()).status).toBe("accepted");
    expect((await inviteRow()).acceptedAt).not.toBeNull();

    expect(await refusal(acceptInvite({ token: TOKEN }))).toMatch(/already been accepted/);
  });

  it("refuses a revoked or expired invite", async () => {
    await seedInvite({ status: "revoked" });
    expect(await refusal(acceptInvite({ token: TOKEN }))).toMatch(/revoked/);
    expect(await membership()).toBeUndefined();

    await d().delete(s.invitations);
    await seedInvite({ expiresAt: new Date(Date.now() - 1000) });
    expect(await refusal(acceptInvite({ token: TOKEN }))).toMatch(/expired/);
    expect(await membership()).toBeUndefined();
  });

  it("refuses a token that matches no invitation", async () => {
    await seedInvite();
    expect(await refusal(acceptInvite({ token: "not-the-token" }))).toMatch(/invalid/);
    expect(await membership()).toBeUndefined();
  });

  it("materializes project grants and marks the member scoped", async () => {
    await seedInvite({
      role: "Developer",
      projectGrants: JSON.stringify([
        { projectId: FIXTURE.projectId, role: "Project Admin" },
        // A grant for a project that has since been deleted — it must be
        // skipped rather than block the org membership.
        { projectId: "proj_deleted", role: "Developer" },
      ]),
    });

    unwrap(await acceptInvite({ token: TOKEN }));

    const mem = await membership();
    expect(mem.role).toBe("Developer");
    // SIGMA-167: an invite carrying grants creates a SCOPED member, so revoking
    // their last grant narrows them to nothing instead of widening them to the
    // whole org.
    expect(mem.scoped).toBe(true);

    const grants = await d()
      .select()
      .from(s.projectMemberships)
      .where(eq(s.projectMemberships.userId, session.id));
    expect(grants).toHaveLength(1);
    expect(grants[0].projectId).toBe(FIXTURE.projectId);
    expect(grants[0].role).toBe("Project Admin");
  });

  it("leaves an org-wide invite unscoped", async () => {
    await seedInvite({ role: "Org Admin" });
    unwrap(await acceptInvite({ token: TOKEN }));
    const mem = await membership();
    expect(mem.role).toBe("Org Admin");
    expect(mem.scoped).toBe(false);
  });

  it("does not overwrite an existing membership's role", async () => {
    // SIGMA-111: two different invitations for the same (org, user) both pass
    // their own FOR UPDATE lock, so the unique constraint is the authority and
    // the first accept's role has to survive the second.
    await d().insert(s.memberships).values({
      id: "mem_existing",
      orgId: FIXTURE.orgId,
      userId: session.id,
      role: "Org Admin",
    });
    await seedInvite({ role: "Developer" });

    unwrap(await acceptInvite({ token: TOKEN }));

    expect((await membership()).role).toBe("Org Admin");
    expect(
      await d().select().from(s.memberships).where(eq(s.memberships.userId, session.id))
    ).toHaveLength(1);
  });
});
