import "server-only";
import { cookies, headers } from "next/headers";
import { redirect } from "next/navigation";
import { and, eq } from "drizzle-orm";
import { auth } from "../lib/auth";
import { effectiveProjectRole, roleAtLeast } from "../lib/rbac";
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

/** Assert the session user belongs to `orgId`; returns their role and explicit
 *  scoping flag (SIGMA-167). Throws otherwise. */
export async function requireMembership(orgId: string) {
  const user = await getSessionUser();
  const [m] = await db
    .select({ role: s.memberships.role, scoped: s.memberships.scoped })
    .from(s.memberships)
    .where(and(eq(s.memberships.userId, user.id), eq(s.memberships.orgId, orgId)));
  if (!m) throw new Error("You are not a member of this organization.");
  return { user, role: m.role, scoped: m.scoped };
}

/** Assert the session user is an Org Admin of `orgId`. Returns the user. */
export async function requireOrgAdmin(orgId: string) {
  const { user, role } = await requireMembership(orgId);
  if (role !== "Org Admin") {
    throw new Error("Only organization admins can perform this action.");
  }
  return user;
}

/** Assert the session user can mutate the domain model (create/delete
 *  projects, environments, resources; attach servers). Matches the CP's
 *  Project Admin+ gate so both modes agree; returns the user and role.
 *  ORG-scoped — for actions addressed to a specific project use
 *  requireProjectRole so per-project grants (P2-7) apply. */
export async function requireProjectAdmin(orgId: string) {
  const { user, role } = await requireMembership(orgId);
  if (role !== "Org Admin" && role !== "Project Admin") {
    throw new Error("Only project admins can perform this action.");
  }
  return { user, role };
}

// ── Per-project RBAC (P2-7) ─────────────────────────────────────────────────
// Grants live web-side (the CP has no user concept — it sees the acting user
// only as the signed {name, role} header, which can only NARROW the token's
// role). The effective per-project role computed here is what rides that
// header, so CP enforcement follows automatically with zero CP changes.

/** The user's project grants inside one org: projectId → granted role. */
export async function projectGrants(
  userId: string,
  orgId: string
): Promise<Map<string, string>> {
  const rows = await db
    .select({ projectId: s.projectMemberships.projectId, role: s.projectMemberships.role })
    .from(s.projectMemberships)
    .innerJoin(s.projects, eq(s.projectMemberships.projectId, s.projects.id))
    .where(and(eq(s.projectMemberships.userId, userId), eq(s.projects.orgId, orgId)));
  return new Map(rows.map((r) => [r.projectId, r.role]));
}

/** Projects the user may see in this org, or null for "all" (org admins and
 *  UNSCOPED users). Read paths filter lists with this.
 *
 *  Scoping is the membership's explicit flag, not a grant count (SIGMA-167):
 *  a scoped user whose last grant was revoked — or whose only granted project
 *  was deleted — sees an EMPTY set, not everything. */
export async function visibleProjects(
  userId: string,
  orgId: string,
  orgRole: string
): Promise<Set<string> | null> {
  if (orgRole === "Org Admin") return null;
  const [m] = await db
    .select({ scoped: s.memberships.scoped })
    .from(s.memberships)
    .where(and(eq(s.memberships.userId, userId), eq(s.memberships.orgId, orgId)));
  const grants = await projectGrants(userId, orgId);
  if (!m?.scoped && grants.size === 0) return null; // unscoped: org-wide
  return new Set(grants.keys());
}

/** Assert the session user holds at least `min` on THIS project (effective
 *  role = org ceiling narrowed by their grant; see lib/rbac.ts for the exact
 *  rules). Returns the user and the effective role — forward THAT role to the
 *  CP actor header, never the bare org role. */
export async function requireProjectRole(
  orgId: string,
  projectId: string,
  min: "Project Admin" | "Developer"
) {
  const { user, role: orgRole, scoped } = await requireMembership(orgId);
  // Bind the project to the org BEFORE trusting any role. Without this, an Org
  // Admin of org A (every user is Org Admin of their personal org) passes the
  // Org-Admin short-circuit in effectiveProjectRole for a projectId that lives
  // in org B — and the local grant tables (project_memberships) have no CP
  // backstop, so a cross-tenant grant mutation would go through (SIGMA-70).
  const [proj] = await db
    .select({ orgId: s.projects.orgId })
    .from(s.projects)
    .where(eq(s.projects.id, projectId));
  if (!proj || proj.orgId !== orgId) {
    throw new Error("You do not have access to this project.");
  }
  const grants = await projectGrants(user.id, orgId);
  // Explicit scoping wins; grants.size covers pre-backfill rows defensively.
  const effective = effectiveProjectRole(orgRole, grants.get(projectId), scoped || grants.size > 0);
  if (!effective) {
    throw new Error("You do not have access to this project.");
  }
  if (!roleAtLeast(effective, min)) {
    throw new Error("This action requires the Project Admin role for this project.");
  }
  return { user, role: effective };
}

/** Project Admin gate for actions addressed by resource id: the project is
 *  resolved through the local mirror. The lookup is org-scoped (join through
 *  projects, assert orgId) so a resource in another org is never resolved to a
 *  foreign project here — defense-in-depth on top of the CP's own org checks
 *  (SIGMA-76). A missing/foreign mirror row falls back to the org-level gate
 *  rather than breaking the action — the mirror self-heals via the SIGMA-56
 *  sync, and the CP still enforces its own org+role checks. */
export async function requireProjectAdminForResource(orgId: string, resourceId: string) {
  const [res] = await db
    .select({ projectId: s.resources.projectId })
    .from(s.resources)
    .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
    .where(and(eq(s.resources.id, resourceId), eq(s.projects.orgId, orgId)));
  if (res) {
    return requireProjectRole(orgId, res.projectId, "Project Admin");
  }
  // Mirror row missing (e.g. a CP-created resource before the SIGMA-56 sync).
  // Fail closed for PROJECT-SCOPED users — the bare org gate below would ignore
  // their per-project grants and let a Project-Admin-org-role user act on a
  // project they were never granted (SIGMA-89). Unscoped users (org admins /
  // zero-grant legacy users, who see everything anyway) still fall back to the
  // org gate; the CP re-enforces org+role regardless.
  const { user, role } = await requireMembership(orgId);
  if ((await visibleProjects(user.id, orgId, role)) !== null) {
    throw new Error("You do not have access to this resource.");
  }
  return requireProjectAdmin(orgId);
}

/** Assert the session user can SEE `projectId` under P2-7 (org admins and
 *  unscoped users see all). Throws otherwise. */
export async function assertProjectVisible(orgId: string, projectId: string) {
  const { user, role } = await requireMembership(orgId);
  const visible = await visibleProjects(user.id, orgId, role);
  if (visible && !visible.has(projectId)) {
    throw new Error("You do not have access to this project.");
  }
}

/** Read-visibility gate (P2-7) for a read action addressed by resource id: the
 *  caller must be able to see the resource's project. Fails closed when the
 *  resource can't be resolved in-org (SIGMA-84). The CP enforces only the org
 *  tenant on these reads, so per-project scoping is web-only — apply this on the
 *  info/log/list READ actions that forward a client resourceId to the CP. */
export async function requireResourceVisible(orgId: string, resourceId: string) {
  const [res] = await db
    .select({ projectId: s.resources.projectId })
    .from(s.resources)
    .innerJoin(s.projects, eq(s.resources.projectId, s.projects.id))
    .where(and(eq(s.resources.id, resourceId), eq(s.projects.orgId, orgId)));
  if (!res) throw new Error("You do not have access to this resource.");
  await assertProjectVisible(orgId, res.projectId);
}

/** Read-visibility gate (P2-7) addressed by environment id (SIGMA-84). */
export async function requireEnvironmentVisible(orgId: string, environmentId: string) {
  const [env] = await db
    .select({ projectId: s.environments.projectId })
    .from(s.environments)
    .innerJoin(s.projects, eq(s.environments.projectId, s.projects.id))
    .where(and(eq(s.environments.id, environmentId), eq(s.projects.orgId, orgId)));
  if (!env) throw new Error("You do not have access to this environment.");
  await assertProjectVisible(orgId, env.projectId);
}

/** True when the session user sees every project in the org (org admin or an
 *  unscoped member) — the only case an org-wide read (e.g. an unscoped log
 *  search) is allowed for (SIGMA-84). */
export async function hasFullOrgVisibility(orgId: string): Promise<boolean> {
  const { user, role } = await requireMembership(orgId);
  return (await visibleProjects(user.id, orgId, role)) === null;
}
