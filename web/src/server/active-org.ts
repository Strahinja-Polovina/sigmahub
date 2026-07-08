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

/** v1: every user needs an org to use the dashboard, but signup doesn't create
 *  one. Called when a signed-in user has no memberships: creates their personal
 *  org with them as Org Admin. Deterministic ids + onConflictDoNothing make it
 *  idempotent under concurrent renders. */
export async function ensurePersonalOrg() {
  const user = await getSessionUser();
  const suffix = user.id.replace(/[^a-zA-Z0-9]/g, "").slice(0, 12).toLowerCase();
  const orgId = `org_p_${suffix}`;
  const first = user.name?.trim().split(/\s+/)[0] || "Personal";
  await db
    .insert(s.orgs)
    .values({ id: orgId, name: `${first}'s Org`, slug: `personal-${suffix}`, plan: "free" })
    .onConflictDoNothing();
  await db
    .insert(s.memberships)
    .values({ id: `mem_p_${suffix}`, orgId, userId: user.id, role: "Org Admin" })
    .onConflictDoNothing();
  return orgId;
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

/** Assert the session user is an Org Admin of `orgId`. Returns the user. */
export async function requireOrgAdmin(orgId: string) {
  const { user, role } = await requireMembership(orgId);
  if (role !== "Org Admin") {
    throw new Error("Only organization admins can perform this action.");
  }
  return user;
}
