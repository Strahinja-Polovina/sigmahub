// What stops the invite flow from being a free mail cannon (SIGMA-365).
//
// Sign-up creates a personal org with the new user as Org Admin, so on a public
// launch "an org admin" is "anyone who registered". From there:
//
//   - resendInvite had NO limit of any kind. Holding the button mailed an
//     arbitrary address without bound.
//   - inviteMember is bounded to one PENDING invite per address, which bounds
//     nothing: an attacker uses a different address each time.
//
// The mail goes out over our SMTP, from the sending domain that every other
// tenant's password-reset and verification mail depends on. The recipient does
// not have to be a customer, and a blocklisted domain is not something a deploy
// fixes. This is the cheapest abuse in the product to run and the most expensive
// to undo, which is why the limits are asserted rather than assumed.
//
// The other half of the property is that neither limit is a DEAD END — the shape
// of defect this whole review series keeps finding. The invite dialog offers the
// link to copy, so a throttled admin can still onboard the person immediately,
// and both refusals have to say so.

import { beforeEach, describe, expect, it, vi } from "vitest";
import { eq } from "drizzle-orm";

import * as s from "@/server/db/schema";
import { user } from "@/server/db/auth-schema";
import {
  INVITE_SENDS_PER_WINDOW,
  INVITE_SEND_WINDOW_MS,
  INVITE_TTL_MS,
  hashInviteToken,
} from "@/lib/invite";
import { FIXTURE, seedDemoFixture, type DemoDb } from "@/server/testing/demo-db";
import { unwrap } from "@/lib/action-result";

const sent: string[] = [];

vi.mock("@/server/db", async () => {
  const { createDemoDb } = await import("@/server/testing/demo-db");
  return { db: await createDemoDb() };
});
vi.mock("next/cache", () => ({ revalidatePath: () => {} }));
vi.mock("@/server/audit", () => ({ writeAudit: async () => {} }));
vi.mock("@/server/email", () => ({
  sendInviteEmail: async (o: { to: string }) => {
    sent.push(o.to);
    return { delivered: true };
  },
}));
vi.mock("@/server/active-org", () => ({
  getSessionUser: async () => ({ id: "usr_admin", name: "Admin", email: "admin@acme.test" }),
  requireOrgAdmin: async () => ({ id: "usr_admin", name: "Admin", role: "Org Admin" }),
}));

const { db } = await import("@/server/db");
const { inviteMember, resendInvite } = await import("@/server/actions/members");
const d = () => db as unknown as DemoDb;

beforeEach(async () => {
  sent.length = 0;
  for (const t of [s.projectMemberships, s.invitations, s.memberships, s.orgMailBudget, s.orgs, user]) {
    await d().delete(t);
  }
  await seedDemoFixture(d());
});

describe("resendInvite", () => {
  const seedPending = async (lastSentAt: Date) => {
    await d()
      .insert(s.invitations)
      .values({
        id: "inv_1",
        orgId: FIXTURE.orgId,
        email: "target@example.com",
        role: "Developer",
        projectGrants: "[]",
        tokenHash: hashInviteToken("tok"),
        invitedBy: "Admin",
        status: "pending",
        expiresAt: new Date(Date.now() + INVITE_TTL_MS),
        lastSentAt,
      });
  };

  it("refuses a second send inside the cooldown, and sends no mail", async () => {
    await seedPending(new Date());

    const res = await resendInvite({ orgId: FIXTURE.orgId, invitationId: "inv_1" });
    expect(res.ok).toBe(false);
    expect(res.ok ? "" : res.error).toMatch(/resend in/i);
    // The mail is the thing being limited; a refusal that still sent it would be
    // a limit in name only.
    expect(sent).toEqual([]);
  });

  it("tells the admin when they can retry AND that copying the link works now", async () => {
    // A refusal with no exit is the exact defect class this review series keeps
    // finding. This one has two exits and must name both.
    await seedPending(new Date());
    const res = await resendInvite({ orgId: FIXTURE.orgId, invitationId: "inv_1" });
    expect(res.ok ? "" : res.error).toMatch(/copy the link/i);
  });

  it("allows the resend once the cooldown has elapsed, and re-arms", async () => {
    await seedPending(new Date(Date.now() - 5 * 60 * 1000));

    const out = unwrap(await resendInvite({ orgId: FIXTURE.orgId, invitationId: "inv_1" }));
    expect(out.delivered).toBe(true);
    expect(sent).toEqual(["target@example.com"]);

    // lastSentAt moved, so the next one is refused — otherwise the cooldown
    // would be a one-shot that an attacker clears by waiting once.
    const [row] = await d().select().from(s.invitations).where(eq(s.invitations.id, "inv_1"));
    expect(Date.now() - row.lastSentAt.getTime()).toBeLessThan(5000);
    const again = await resendInvite({ orgId: FIXTURE.orgId, invitationId: "inv_1" });
    expect(again.ok ? "" : again.error).toMatch(/resend in/i);
  });
});

describe("the per-org outbound-mail budget", () => {
  /** Spend `n` of the org's window, the way real sends do. Addresses are unique
   *  per call so the one-pending-per-email guard is never what refuses. */
  let seq = 0;
  const spend = async (n: number, orgId = FIXTURE.orgId) => {
    for (let i = 0; i < n; i++) {
      await inviteMember({ orgId, email: `seed${seq++}@example.com`, role: "Developer" });
    }
  };

  it("lets a real team onboard — the budget is above ordinary use", async () => {
    await spend(INVITE_SENDS_PER_WINDOW - 1);
    const out = unwrap(await inviteMember({
      orgId: FIXTURE.orgId,
      email: "newhire@example.com",
      role: "Developer",
    }));
    expect(out.delivered).toBe(true);
    expect(sent).toHaveLength(INVITE_SENDS_PER_WINDOW);
  });

  it("stops SENDING past the budget, without refusing the invite", async () => {
    // The budget bounds mail, not membership. Refusing the invite here was the
    // first version and it was a dead end: the refusal told the admin to copy
    // the invite link, and because it came before the insert there was no
    // invitation and no link to copy (SIGMA-365).
    await spend(INVITE_SENDS_PER_WINDOW);
    sent.length = 0;
    const out = unwrap(
      await inviteMember({ orgId: FIXTURE.orgId, email: "spam@example.com", role: "Developer" })
    );
    expect(sent).toEqual([]);
    expect(out.delivered).toBe(false);
    expect(out.throttled).toBe(true);
    // The exit the dialog offers has to actually exist.
    expect(out.inviteUrl).toMatch(/\/invite\//);
    const [row] = await d()
      .select()
      .from(s.invitations)
      .where(eq(s.invitations.email, "spam@example.com"));
    expect(row).toBeTruthy();
  });

  // THE ONE THAT MATTERS. The first version of this throttle was a resend
  // cooldown plus a cap on invitations CREATED per hour, and it looked
  // sufficient. It was not: rotating through pending invitations sends without
  // creating anything, so 25 invites resendable once a minute each is 1500
  // messages an hour — a mail cannon that passes both limits.
  it("counts RESENDS too, or the cooldown just paces an unbounded stream", async () => {
    const ids: string[] = [];
    for (let i = 0; i < 5; i++) {
      await inviteMember({ orgId: FIXTURE.orgId, email: `p${i}@example.com`, role: "Developer" });
    }
    const pending = await d().select().from(s.invitations);
    for (const row of pending) ids.push(row.id);
    // Age them past the per-invitation cooldown so only the budget can refuse.
    await d()
      .update(s.invitations)
      .set({ lastSentAt: new Date(Date.now() - 10 * 60 * 1000) });

    sent.length = 0;
    let refusedAfter = 0;
    // Rotate through the pending invites, resending. Under the old design this
    // loop ran forever; under the budget it stops.
    for (let round = 0; round < 20 && refusedAfter === 0; round++) {
      for (const id of ids) {
        const res = await resendInvite({ orgId: FIXTURE.orgId, invitationId: id });
        if (!res.ok) {
          refusedAfter = sent.length;
          break;
        }
        await d()
          .update(s.invitations)
          .set({ lastSentAt: new Date(Date.now() - 10 * 60 * 1000) })
          .where(eq(s.invitations.id, id));
      }
    }
    expect(refusedAfter).toBeGreaterThan(0);
    // The 5 creates already spent 5 of the window, so the resends get the rest.
    expect(sent.length).toBeLessThanOrEqual(INVITE_SENDS_PER_WINDOW);
  });

  it("rolls the window forward once it has fully elapsed", async () => {
    // A budget that counted forever would brick every long-lived organization,
    // which is a worse failure than the abuse it prevents.
    await spend(INVITE_SENDS_PER_WINDOW);
    await d()
      .update(s.orgMailBudget)
      .set({ windowStart: new Date(Date.now() - INVITE_SEND_WINDOW_MS - 60_000) });

    const out = unwrap(await inviteMember({
      orgId: FIXTURE.orgId,
      email: "tomorrow@example.com",
      role: "Developer",
    }));
    expect(out.delivered).toBe(true);
  });

  it("does not let a steady drip hold the window open forever", async () => {
    // window_start is when the window OPENED, not the last send. Advancing it on
    // every send would mean a caller who never pauses is never reset — and never
    // refused either, since the count would keep resetting with it.
    await spend(3);
    const [before] = await d().select().from(s.orgMailBudget);
    await spend(3);
    const [after] = await d().select().from(s.orgMailBudget);
    expect(after.windowStart.getTime()).toBe(before.windowStart.getTime());
    expect(after.sent).toBe(6);
  });

  it("is scoped per org — one abuser cannot spend another tenant's allowance", async () => {
    await spend(INVITE_SENDS_PER_WINDOW);
    const out = unwrap(await inviteMember({
      orgId: FIXTURE.rivalOrgId,
      email: "unaffected@example.com",
      role: "Developer",
    }));
    expect(out.delivered).toBe(true);
  });
});
