"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { eq } from "drizzle-orm";
import { db } from "../db";
import * as s from "../db/schema";
import { ORG_COOKIE, requireMembership, requireOrgAdmin } from "../active-org";
import { writeAudit } from "../audit";

/** Switch the active org (server-readable cookie) — only to an org you belong to. */
export async function setActiveOrg(orgId: string) {
  await requireMembership(orgId);
  (await cookies()).set(ORG_COOKIE, orgId, {
    path: "/",
    httpOnly: true,
    sameSite: "lax",
    maxAge: 60 * 60 * 24 * 365,
  });
  revalidatePath("/dashboard", "layout");
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
