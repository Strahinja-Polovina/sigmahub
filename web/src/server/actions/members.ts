"use server";

import { revalidatePath } from "next/cache";
import { and, eq, inArray } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { user } from "../db/auth-schema";
import { requireOrgAdmin, getSessionUser } from "../active-org";
import { writeAudit } from "../audit";
import { sendInviteEmail } from "../email";
import { emailVerificationRequired } from "../../lib/email-verification";
import { chargeOrgMail } from "../mail-budget";
import {
  INVITE_TTL_MS,
  appBaseUrl,
  hashInviteToken,
  humanWait,
  resendWaitMs,
  inviteRejection,
  inviteRejectionMessage,
  inviteUrl,
  newInviteToken,
  normalizeOrgRole,
  parseProjectGrants,
  sameEmail,
  serializeProjectGrants,
  type InviteProjectGrant,
} from "../../lib/invite";

function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}

/** True for a Postgres unique-violation (SQLSTATE 23505), raised by both
 *  node-postgres and PGlite. Used to turn a raced insert against a unique
 *  constraint/index into a friendly domain error. */
function isUniqueViolation(e: unknown): boolean {
  return typeof e === "object" && e !== null && (e as { code?: string }).code === "23505";
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

/** The org's display name, for the invite email/link copy. */
async function orgName(orgId: string): Promise<string> {
  const [org] = await db
    .select({ name: s.orgs.name })
    .from(s.orgs)
    .where(eq(s.orgs.id, orgId));
  return org?.name ?? "an organization";
}

/** P2-7b: invite a member by email. Instead of the old instant-join (which
 *  minted a login-less display-only user row), this records a pending
 *  invitation with a hashed one-time token and sends the accept link. Delivery
 *  degrades honestly: with no mail transport the link is logged AND returned so
 *  an admin can copy it. Membership + grants materialize only on accept. */
export async function inviteMember(input: {
  orgId: string;
  email: string;
  role: string;
  projectGrants?: InviteProjectGrant[];
}): Promise<{ inviteUrl: string; delivered: boolean }> {
  const actor = await requireOrgAdmin(input.orgId);
  const email = input.email.trim().toLowerCase();
  if (!EMAIL_RE.test(email)) throw new Error("Enter a valid email address.");
  const role = normalizeOrgRole(input.role);

  // Already a member? (match by the account's email, case-insensitive).
  const [alreadyMember] = await db
    .select({ id: s.memberships.id })
    .from(s.memberships)
    .innerJoin(user, eq(s.memberships.userId, user.id))
    .where(and(eq(s.memberships.orgId, input.orgId), eq(user.email, email)));
  if (alreadyMember) throw new Error("That person is already a member.");

  // One outstanding invite per email — resend/revoke manage an existing one.
  const [pending] = await db
    .select({ id: s.invitations.id })
    .from(s.invitations)
    .where(
      and(
        eq(s.invitations.orgId, input.orgId),
        eq(s.invitations.email, email),
        eq(s.invitations.status, "pending")
      )
    );
  if (pending) {
    throw new Error("An invite is already pending for that email — resend or revoke it first.");
  }

  // Charge the org's outbound-mail budget BEFORE writing the row, so a refusal
  // does not leave a pending invitation nobody was told about (SIGMA-365).
  // Scoped per org, so one abuser cannot spend another tenant's allowance, and
  // never a dead end: the message names the reset and the copy-link route, which
  // works immediately.
  await chargeOrgMail(input.orgId);

  const { raw, hash } = newInviteToken();
  try {
    await db.insert(s.invitations).values({
      id: rid("inv"),
      orgId: input.orgId,
      email,
      role,
      projectGrants: serializeProjectGrants(input.projectGrants ?? []),
      tokenHash: hash,
      invitedBy: actor.name,
      status: "pending",
      expiresAt: new Date(Date.now() + INVITE_TTL_MS),
    });
  } catch (e) {
    // The partial unique index (SIGMA-115) rejects a second pending invite for
    // the same (org, email) that raced past the check above. Surface the same
    // friendly error and, crucially, do NOT send an email for a row that was
    // never inserted.
    if (isUniqueViolation(e)) {
      throw new Error("An invite is already pending for that email — resend or revoke it first.");
    }
    throw e;
  }

  const url = inviteUrl(appBaseUrl(), raw);
  const { delivered } = await sendInviteEmail({ to: email, orgName: await orgName(input.orgId), role, url });
  await writeAudit({
    orgId: input.orgId,
    actor: actor.name,
    action: "Invited member",
    target: `${email} · ${role}`,
  });
  revalidatePath("/dashboard", "layout");
  return { inviteUrl: url, delivered };
}

/** Rotate a pending invite's token (killing the old link) and re-send it. */
export async function resendInvite(input: {
  orgId: string;
  invitationId: string;
}): Promise<{ inviteUrl: string; delivered: boolean }> {
  const actor = await requireOrgAdmin(input.orgId);
  const [inv] = await db
    .select({
      email: s.invitations.email,
      role: s.invitations.role,
      status: s.invitations.status,
      lastSentAt: s.invitations.lastSentAt,
    })
    .from(s.invitations)
    .where(and(eq(s.invitations.id, input.invitationId), eq(s.invitations.orgId, input.orgId)));
  if (!inv || inv.status !== "pending") throw new Error("No pending invite to resend.");

  // Resend had NO limit (SIGMA-365): holding the button mailed an arbitrary
  // address without bound, from our sending domain. The recipient does not have
  // to be a customer, and a blocklisted domain is not fixed by deploying a patch.
  const wait = resendWaitMs(inv.lastSentAt, new Date());
  if (wait > 0) {
    throw new Error(
      `An invite was just sent to ${inv.email}. You can resend in ${humanWait(wait)} — ` +
        `or copy the link from the invite list and send it yourself now.`
    );
  }
  // The per-invitation cooldown above bounds how often ONE address is mailed; it
  // does not bound the org, which can hold many pending invitations and rotate
  // through them. The budget is what actually bounds volume, so a resend is
  // charged exactly like a new invite.
  await chargeOrgMail(input.orgId);

  const { raw, hash } = newInviteToken();
  await db
    .update(s.invitations)
    .set({
      tokenHash: hash,
      expiresAt: new Date(Date.now() + INVITE_TTL_MS),
      lastSentAt: new Date(),
    })
    .where(eq(s.invitations.id, input.invitationId));

  const url = inviteUrl(appBaseUrl(), raw);
  const { delivered } = await sendInviteEmail({ to: inv.email, orgName: await orgName(input.orgId), role: inv.role, url });
  await writeAudit({
    orgId: input.orgId,
    actor: actor.name,
    action: "Resent invite",
    target: `${inv.email} · ${inv.role}`,
  });
  revalidatePath("/dashboard", "layout");
  return { inviteUrl: url, delivered };
}

/** Revoke a pending invite — its link stops working; the email is free to
 *  re-invite (the pending-dup guard only sees pending rows). */
export async function revokeInvite(input: { orgId: string; invitationId: string }): Promise<void> {
  const actor = await requireOrgAdmin(input.orgId);
  const [inv] = await db
    .select({ email: s.invitations.email, status: s.invitations.status })
    .from(s.invitations)
    .where(and(eq(s.invitations.id, input.invitationId), eq(s.invitations.orgId, input.orgId)));
  if (!inv || inv.status !== "pending") throw new Error("No pending invite to revoke.");

  await db.update(s.invitations).set({ status: "revoked" }).where(eq(s.invitations.id, input.invitationId));
  await writeAudit({ orgId: input.orgId, actor: actor.name, action: "Revoked invite", target: inv.email });
  revalidatePath("/dashboard", "layout");
}

/** Accept an invite: the signed-in account (whose email must match the invite)
 *  gains the org membership + any project grants, and the token is
 *  one-time-invalidated. Runs in one locked transaction so a double-submit
 *  can't create two memberships or accept twice. */
export async function acceptInvite(input: { token: string }): Promise<{ orgId: string }> {
  const sessionUser = await getSessionUser();
  const hash = hashInviteToken(input.token);
  const now = new Date();

  const orgId = await db.transaction(async (tx) => {
    const [inv] = await tx
      .select()
      .from(s.invitations)
      .where(eq(s.invitations.tokenHash, hash))
      .for("update");
    const rejection = inviteRejection(inv ?? null, now);
    if (rejection) throw new Error(inviteRejectionMessage(rejection));
    if (!sameEmail(sessionUser.email, inv.email)) {
      throw new Error(
        `This invitation was sent to ${inv.email}. Sign in with that email to accept it.`
      );
    }
    // The email match above is the ONLY thing binding this invite to a person,
    // so the address has to have been proven (SIGMA-361). Anyone can register an
    // account claiming any address; without verification, someone holding a
    // leaked invite link registers the invited address and joins the org as
    // themselves.
    //
    // The gate is the deployment's verification policy, NOT "is mail
    // deliverable" (SIGMA-365). Those are the same by default and differ the
    // moment an operator states AUTH_REQUIRE_EMAIL_VERIFICATION — and reading
    // deliverability there refused every invite on a deployment that had
    // deliberately turned verification off, pointing each invitee at a link it
    // was configured never to send. See lib/email-verification.ts.
    if (emailVerificationRequired() && !sessionUser.emailVerified) {
      throw new Error(
        "Verify your email address before accepting this invitation. " +
          "Check your inbox for the verification link, or request a new one from the sign-in page."
      );
    }

    // Membership first (idempotent) so any project grant has an org membership
    // to narrow — a project grant is never a backdoor into the org. The
    // per-invitation FOR UPDATE lock above only serializes accepts of the SAME
    // invitation, so two DIFFERENT invitations for the same (org, user) could
    // both pass a check-then-insert and create duplicate memberships with
    // divergent roles (SIGMA-111). onConflictDoNothing on the (org_id, user_id)
    // unique constraint makes the DB the authority: the first accept wins, a
    // concurrent second is a no-op that keeps the already-materialized role.
    await tx
      .insert(s.memberships)
      .values({
        id: rid("mem"),
        orgId: inv.orgId,
        userId: sessionUser.id,
        role: normalizeOrgRole(inv.role),
      })
      .onConflictDoNothing({
        target: [s.memberships.orgId, s.memberships.userId],
      });

    // Materialize grants, skipping any project that has since been deleted or
    // moved orgs — a stale grant must never block joining the org.
    let granted = false;
    for (const g of parseProjectGrants(inv.projectGrants)) {
      const [proj] = await tx
        .select({ id: s.projects.id })
        .from(s.projects)
        .where(and(eq(s.projects.id, g.projectId), eq(s.projects.orgId, inv.orgId)));
      if (!proj) continue;
      await tx
        .insert(s.projectMemberships)
        .values({ id: rid("pm"), projectId: g.projectId, userId: sessionUser.id, role: g.role })
        .onConflictDoUpdate({
          target: [s.projectMemberships.projectId, s.projectMemberships.userId],
          set: { role: g.role },
        });
      granted = true;
    }
    // An invite carrying grants creates a SCOPED member (SIGMA-167): the flag
    // is explicit state, so a later revoke of their last grant narrows to
    // nothing instead of silently widening to every project.
    if (granted) {
      await tx
        .update(s.memberships)
        .set({ scoped: true })
        .where(
          and(eq(s.memberships.orgId, inv.orgId), eq(s.memberships.userId, sessionUser.id))
        );
    }

    await tx
      .update(s.invitations)
      .set({ status: "accepted", acceptedAt: now })
      .where(eq(s.invitations.id, inv.id));
    return inv.orgId;
  });

  await writeAudit({ orgId, actor: sessionUser.name, action: "Accepted invite", target: sessionUser.email });
  revalidatePath("/dashboard", "layout");
  return { orgId };
}

export async function changeMemberRole(input: {
  orgId: string;
  userId: string;
  role: string;
}) {
  const actor = await requireOrgAdmin(input.orgId);
  const role = normalizeOrgRole(input.role);

  // Guard + mutation share one transaction with the admin rows locked
  // (FOR UPDATE) so two concurrent demotions can't both pass the last-admin
  // check and leave the org with zero admins — a permanent lockout.
  const target = await db.transaction(async (tx) => {
    if (role !== "Org Admin") {
      const admins = await tx
        .select({ userId: s.memberships.userId })
        .from(s.memberships)
        .where(
          and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.role, "Org Admin"))
        )
        .for("update");
      if (admins.length <= 1 && admins.some((a) => a.userId === input.userId)) {
        throw new Error("An organization needs at least one admin.");
      }
    }
    const [t] = await tx
      .select({ name: user.name })
      .from(s.memberships)
      .innerJoin(user, eq(s.memberships.userId, user.id))
      .where(
        and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.userId, input.userId))
      );
    await tx
      .update(s.memberships)
      .set({ role })
      .where(
        and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.userId, input.userId))
      );
    return t;
  });
  await writeAudit({
    orgId: input.orgId,
    actor: actor.name,
    action: "Changed role",
    target: `${target?.name ?? input.userId} → ${role}`,
  });
  revalidatePath("/dashboard", "layout");
}

export async function removeMember(input: { orgId: string; userId: string }) {
  const actor = await requireOrgAdmin(input.orgId);
  if (input.userId === actor.id) {
    throw new Error("You can’t remove yourself from an organization.");
  }
  // Same guarded transaction as changeMemberRole: lock the admin rows and
  // re-check inside so two admins removing each other concurrently can't both
  // slip past the last-admin guard.
  const target = await db.transaction(async (tx) => {
    const admins = await tx
      .select({ userId: s.memberships.userId })
      .from(s.memberships)
      .where(
        and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.role, "Org Admin"))
      )
      .for("update");
    if (admins.length <= 1 && admins.some((a) => a.userId === input.userId)) {
      throw new Error("An organization needs at least one admin.");
    }
    const [t] = await tx
      .select({ name: user.name })
      .from(s.memberships)
      .innerJoin(user, eq(s.memberships.userId, user.id))
      .where(
        and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.userId, input.userId))
      );
    await tx
      .delete(s.memberships)
      .where(
        and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.userId, input.userId))
      );
    // Also drop the user's project grants for this org. project_memberships has
    // no FK/cascade to memberships, so leaving them makes the grants inert-but-
    // resurrectable: a later re-invite recreates the membership and the stale
    // grants silently reactivate, restoring project access that was never
    // re-granted (SIGMA-148). Scope the delete to this org's projects.
    await tx.delete(s.projectMemberships).where(
      and(
        eq(s.projectMemberships.userId, input.userId),
        inArray(
          s.projectMemberships.projectId,
          tx
            .select({ id: s.projects.id })
            .from(s.projects)
            .where(eq(s.projects.orgId, input.orgId))
        )
      )
    );
    return t;
  });
  await writeAudit({
    orgId: input.orgId,
    actor: actor.name,
    action: "Removed member",
    target: target?.name ?? input.userId,
  });
  revalidatePath("/dashboard", "layout");
}
