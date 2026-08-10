"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import {
  ORG_COOKIE,
  getSessionUser,
  requireMembership,
  requireOrgAdmin,
} from "../active-org";
import { writeAudit } from "../audit";

const ORG_COOKIE_OPTIONS = {
  path: "/",
  httpOnly: true,
  sameSite: "lax",
  maxAge: 60 * 60 * 24 * 365,
} as const;

/** Switch the active org (server-readable cookie) — only to an org you belong to. */
export async function setActiveOrg(orgId: string) {
  await requireMembership(orgId);
  (await cookies()).set(ORG_COOKIE, orgId, ORG_COOKIE_OPTIONS);
  revalidatePath("/dashboard", "layout");
}

function rid(prefix: string) {
  return `${prefix}_${crypto.randomUUID().replace(/-/g, "").slice(0, 12)}`;
}

/** URL/CLI-safe stem for an org name. Empty for a name with no ASCII
 *  alphanumerics at all (e.g. "株式会社"), which the caller replaces with a
 *  generic stem rather than minting an empty slug. */
function slugStem(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 32);
}

/**
 * SIGMA-306: create a second organization and switch into it.
 *
 * Until this existed, `orgs` had two writers — the seed script and
 * ensurePersonalOrg, which fires once at first login — and the switcher's
 * "New organization" item was a link to the CURRENT org's settings page. A
 * consultant who wanted a second client's fleet kept apart clicked it, found
 * an org name in an editable field, typed the new name and pressed Save, and
 * renamed the org they already had for every teammate at once.
 *
 * The row shape mirrors ensurePersonalOrg (org + Org Admin membership) because
 * that is what the rest of the product reads: an org with no membership for its
 * creator is invisible to the creator. Both inserts run in one transaction, so
 * a failure never leaves an orphan org nobody can reach.
 */
export async function createOrg(input: {
  name: string;
}): Promise<{ orgId: string; name: string }> {
  const user = await getSessionUser();
  const name = input.name.trim();
  if (!name) throw new Error("Organization name is required.");
  if (name.length > 64) {
    throw new Error("Organization name must be 64 characters or fewer.");
  }

  const orgId = rid("org");
  // `orgs.slug` is UNIQUE and two clients may well both be called "Beta
  // Client", so the stem gets a short unique suffix rather than a collision.
  const stem = slugStem(name) || "org";
  const slug = `${stem}-${orgId.slice(-6)}`;

  await db.transaction(async (tx) => {
    await tx.insert(s.orgs).values({ id: orgId, name, slug, plan: "free" });
    await tx.insert(s.memberships).values({
      id: rid("mem"),
      orgId,
      userId: user.id,
      role: "Org Admin",
      // Org-wide: there are no projects yet, and a scoped creator would see
      // nothing in the org they just made (SIGMA-167).
      scoped: false,
    });
  });

  // Switch into it, same cookie setActiveOrg writes — the membership above is
  // what makes that cookie survive getActiveOrgId's re-check.
  (await cookies()).set(ORG_COOKIE, orgId, ORG_COOKIE_OPTIONS);
  await writeAudit({
    orgId,
    actor: user.name,
    action: "Created organization",
    target: name,
  });
  revalidatePath("/dashboard", "layout");
  return { orgId, name };
}

/** Rename the organization (Org Admins only). */
export async function updateOrg(input: { orgId: string; name: string }) {
  const actor = await requireOrgAdmin(input.orgId);
  const name = input.name.trim();
  if (!name) throw new Error("Organization name is required.");
  await db.update(s.orgs).set({ name }).where(eq(s.orgs.id, input.orgId));
  await writeAudit({
    orgId: input.orgId,
    actor: actor.name,
    action: "Updated org name",
    target: name,
  });
  revalidatePath("/dashboard", "layout");
}
