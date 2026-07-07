"use server";

import { revalidatePath } from "next/cache";
import { and, eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { user } from "../db/auth-schema";
import { requireOrgAdmin } from "../active-org";
import { writeAudit } from "../audit";

function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}

const ROLES = new Set(["Org Admin", "Project Admin", "Developer"]);
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

function nameFromEmail(email: string) {
  return email
    .split("@")[0]
    .replace(/[._-]+/g, " ")
    .replace(/\b\w/g, (c) => c.toUpperCase())
    .trim();
}

/** Add a member by email. v1 is invite-and-join in one step (no email/accept
 *  flow yet): if no user has that email we create a display-only account. */
export async function inviteMember(input: {
  orgId: string;
  email: string;
  role: string;
}) {
  const actor = await requireOrgAdmin(input.orgId);
  const email = input.email.trim().toLowerCase();
  if (!EMAIL_RE.test(email)) throw new Error("Enter a valid email address.");
  const role = ROLES.has(input.role) ? input.role : "Developer";

  let [u] = await db.select().from(user).where(eq(user.email, email));
  if (!u) {
    const id = rid("usr");
    await db
      .insert(user)
      .values({ id, name: nameFromEmail(email), email, emailVerified: false });
    [u] = await db.select().from(user).where(eq(user.id, id));
  }

  const [existing] = await db
    .select()
    .from(s.memberships)
    .where(
      and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.userId, u.id))
    );
  if (existing) throw new Error("That person is already a member.");

  await db
    .insert(s.memberships)
    .values({ id: rid("mem"), orgId: input.orgId, userId: u.id, role });
  await writeAudit({
    orgId: input.orgId,
    actor: actor.name,
    action: "Invited member",
    target: `${email} · ${role}`,
  });
  revalidatePath("/dashboard", "layout");
}

export async function changeMemberRole(input: {
  orgId: string;
  userId: string;
  role: string;
}) {
  const actor = await requireOrgAdmin(input.orgId);
  const role = ROLES.has(input.role) ? input.role : "Developer";

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
