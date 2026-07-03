import "server-only";
import { cookies, headers } from "next/headers";
import { redirect } from "next/navigation";
import { and, eq } from "drizzle-orm";
import { auth } from "../lib/auth";
import { db } from "./db";
import * as s from "./db/schema";

export const ORG_COOKIE = "sh_org";

/** Current session user, or redirect to /login. */
export async function getSessionUser() {
  const session = await auth.api.getSession({ headers: await headers() });
  if (!session) redirect("/login");
  return session.user;
}

/** Orgs the current user is a member of (deterministic order → stable default). */
export async function getMyOrgs() {
  const user = await getSessionUser();
  return db
    .select({
      id: s.orgs.id,
      name: s.orgs.name,
      slug: s.orgs.slug,
      plan: s.orgs.plan,
      role: s.memberships.role,
    })
    .from(s.memberships)
    .innerJoin(s.orgs, eq(s.memberships.orgId, s.orgs.id))
    .where(eq(s.memberships.userId, user.id))
    .orderBy(s.orgs.name);
}

/** Active org = the `sh_org` cookie if the user still belongs to it, else their
 *  first org. Server components and the client OrgProvider resolve to the same
 *  value, so the two never drift. */
export async function getActiveOrgId(): Promise<string | null> {
  const myOrgs = await getMyOrgs();
  if (myOrgs.length === 0) return null;
  const picked = (await cookies()).get(ORG_COOKIE)?.value;
  if (picked && myOrgs.some((o) => o.id === picked)) return picked;
  return myOrgs[0].id;
}

/** Assert the session user belongs to `orgId`; returns their role. Throws otherwise. */
export async function requireMembership(orgId: string) {
  const user = await getSessionUser();
  const [m] = await db
    .select({ role: s.memberships.role })
    .from(s.memberships)
    .where(and(eq(s.memberships.userId, user.id), eq(s.memberships.orgId, orgId)));
  if (!m) throw new Error("You are not a member of this organization.");
  return { user, role: m.role };
}
