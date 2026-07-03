"use server";

import { cookies } from "next/headers";
import { revalidatePath } from "next/cache";
import { ORG_COOKIE, requireMembership } from "../active-org";

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
