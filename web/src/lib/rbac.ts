// P2-7 per-project RBAC rules, kept pure for unit testing. The org role is
// always the CEILING (the CP's signed-actor model only lets a role narrow,
// never widen — these rules mirror that invariant web-side):
//
// - Org Admin: full access to every project; project rows never apply.
// - A user with ZERO project grants keeps their org-wide role on every
//   project (backward compatible: nobody loses access when P2-7 ships).
// - A user with ANY project grant becomes project-scoped: they see only
//   granted projects, each at min(org role, granted role).

export type OrgRole = "Org Admin" | "Project Admin" | "Developer";
export type ProjectRole = "Project Admin" | "Developer";

const RANK: Record<string, number> = {
  "Org Admin": 3,
  "Project Admin": 2,
  Developer: 1,
};

/** Unknown roles rank 0 — fail closed, mirroring the CP's roleRank. */
export function roleRank(role: string): number {
  return RANK[role] ?? 0;
}

export function roleAtLeast(role: string, min: string): boolean {
  return roleRank(role) >= roleRank(min);
}

function minRole(a: string, b: string): string {
  return roleRank(a) <= roleRank(b) ? a : b;
}

/** The role a user effectively holds on one project, or null for no access.
 *  `grantedRole` is their project_memberships row for THIS project (undefined
 *  when none); `hasAnyGrant` says whether they hold any project grant at all
 *  (the scoping switch). */
export function effectiveProjectRole(
  orgRole: string,
  grantedRole: string | undefined,
  hasAnyGrant: boolean
): string | null {
  if (orgRole === "Org Admin") return "Org Admin";
  if (grantedRole !== undefined) return minRole(orgRole, grantedRole);
  if (hasAnyGrant) return null; // scoped user, no grant here → invisible
  return orgRole; // legacy org-wide access
}

/** Whether the user should see the project at all. */
export function canSeeProject(
  orgRole: string,
  grantedRole: string | undefined,
  hasAnyGrant: boolean
): boolean {
  return effectiveProjectRole(orgRole, grantedRole, hasAnyGrant) !== null;
}
