"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { and, count, eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import {
  ORG_COOKIE,
  getSessionUser,
  requireMembership,
  requireOrgAdmin,
} from "../active-org";
import { writeAudit } from "../audit";
import { Refusal, attempt, type ActionResult } from "../../lib/action-result";
import { MAX_ORGS_PER_USER } from "../../lib/org-limits";

const ORG_COOKIE_OPTIONS = {
  path: "/",
  httpOnly: true,
  sameSite: "lax",
  maxAge: 60 * 60 * 24 * 365,
} as const;

/** Switch the active org (server-readable cookie) — only to an org you belong to. */
export async function setActiveOrg(orgId: string): Promise<ActionResult<object>> {
  return attempt(async () => {
    await requireMembership(orgId);
    (await cookies()).set(ORG_COOKIE, orgId, ORG_COOKIE_OPTIONS);
    revalidatePath("/dashboard", "layout");
  });
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
}): Promise<ActionResult<{ orgId: string; name: string }>> {
  return attempt(async () => {
    const user = await getSessionUser();
    const name = input.name.trim();
    if (!name) throw new Refusal("Organization name is required.");
    if (name.length > 64) {
      throw new Refusal("Organization name must be 64 characters or fewer.");
    }

    // A ceiling on organizations per account (SIGMA-365).
    //
    // Every per-org limit in the product is bypassed by making more orgs, and this
    // action needs nothing but a session. The free tier is per org, so a loop here
    // is unlimited free capacity — which is precisely what
    // assertFreeTierNotExhaustedTx was written to stop. The outbound-mail budget is
    // per org, so the same loop is an unlimited mail cannon. Both ceilings were
    // designed and tested against a fixed org, and neither noticed that the org
    // itself was free to mint.
    //
    // Counted over Org Admin memberships rather than a creator column, which is an
    // approximation in the safe direction — being ADDED to somebody else's org as
    // an admin also counts — so the ceiling is set well above the consultant this
    // action exists for (SIGMA-306: several client fleets kept apart). Anyone who
    // legitimately reaches it is a conversation, not a loop.
    const [{ owned } = { owned: 0 }] = await db
      .select({ owned: count() })
      .from(s.memberships)
      .where(and(eq(s.memberships.userId, user.id), eq(s.memberships.role, "Org Admin")));
    if (owned >= MAX_ORGS_PER_USER) {
      throw new Refusal(
        `You already administer ${MAX_ORGS_PER_USER} organizations, which is the limit for one ` +
          `account. Ask an administrator of the organization you want to join to invite you, or ` +
          `contact support if you genuinely need more.`
      );
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
  });
}

/** Rename the organization (Org Admins only). */
export async function updateOrg(input: { orgId: string; name: string }): Promise<ActionResult<object>> {
  return attempt(async () => {
    const actor = await requireOrgAdmin(input.orgId);
    const name = input.name.trim();
    if (!name) throw new Refusal("Organization name is required.");
    await db.update(s.orgs).set({ name }).where(eq(s.orgs.id, input.orgId));
    await writeAudit({
      orgId: input.orgId,
      actor: actor.name,
      action: "Updated org name",
      target: name,
    });
    revalidatePath("/dashboard", "layout");
  });
}
