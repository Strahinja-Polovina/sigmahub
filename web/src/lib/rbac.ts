// P2-7 per-project RBAC rules, kept pure for unit testing. The org role is
// always the CEILING (the CP's signed-actor model only lets a role narrow,
// never widen — these rules mirror that invariant web-side):
//
// - Org Admin: full access to every project; project rows never apply.
// - An UNSCOPED user (memberships.scoped = false, never granted) keeps their
//   org-wide role on every project (backward compatible: nobody loses access
//   when P2-7 ships).
// - A SCOPED user sees only granted projects, each at min(org role, granted
//   role). Scoping is EXPLICIT state, set when their first grant is issued —
//   never inferred from a live grant count. Inferring it meant revoking a
//   contractor's last grant (or deleting the only project they were granted)
//   silently re-widened them to every project in the org, while the toast and
//   audit trail described a narrowing (SIGMA-167). A scoped user with zero
//   grants now sees NOTHING: fail closed, matching the admin's intent.

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
 *  when none); `scoped` is the membership's explicit scoping flag (SIGMA-167 —
 *  NOT a live grant count). */
export function effectiveProjectRole(
  orgRole: string,
  grantedRole: string | undefined,
  scoped: boolean
): string | null {
  if (orgRole === "Org Admin") return "Org Admin";
  if (grantedRole !== undefined) return minRole(orgRole, grantedRole);
  if (scoped) return null; // scoped user, no grant here → invisible (even at zero grants)
  return orgRole; // unscoped: legacy org-wide access
}

/** Whether the user should see the project at all. */
export function canSeeProject(
  orgRole: string,
  grantedRole: string | undefined,
  scoped: boolean
): boolean {
  return effectiveProjectRole(orgRole, grantedRole, scoped) !== null;
}
