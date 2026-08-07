"use server";

// P2-7 per-project role grants. Granting is Project Admin+ ON THAT PROJECT
// (org admins always qualify). The org role stays the ceiling — granting
// "Project Admin" to an org Developer yields an effective Developer there
// (see lib/rbac.ts); the grant's real power is scoping and narrowing.

import { revalidatePath } from "next/cache";
import { and, eq, inArray } from "drizzle-orm";
import { requireOrgAdmin, requireProjectRole } from "../active-org";
import { writeAudit } from "../audit";
import { db } from "../db";
import * as s from "../db/schema";

const PROJECT_ROLES = new Set(["Project Admin", "Developer"]);

function rid(prefix: string) {
  return `${prefix}_${Math.random().toString(36).slice(2, 10)}`;
}

/** Grant (or change) a member's role on one project. */
export async function setProjectRole(input: {
  orgId: string;
  projectId: string;
  userId: string;
  role: string;
}): Promise<void> {
  const { user } = await requireProjectRole(input.orgId, input.projectId, "Project Admin");
  if (!PROJECT_ROLES.has(input.role)) {
    throw new Error("Project roles are Project Admin or Developer.");
  }
  // The grantee must be an org member — a project grant is a narrowing of an
  // existing org membership, never a backdoor into the org.
  const [member] = await db
    .select({ id: s.memberships.id })
    .from(s.memberships)
    .where(and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.userId, input.userId)));
  if (!member) throw new Error("That user is not a member of this organization.");

  await db
    .insert(s.projectMemberships)
    .values({ id: rid("pm"), projectId: input.projectId, userId: input.userId, role: input.role })
    .onConflictDoUpdate({
      target: [s.projectMemberships.projectId, s.projectMemberships.userId],
      set: { role: input.role },
    });
  // The first grant scopes the member EXPLICITLY (SIGMA-167). The flag — not a
  // live grant count — is what the visibility rules read, so later revoking
  // this grant narrows them to nothing instead of silently re-widening them to
  // every project in the org.
  await db
    .update(s.memberships)
    .set({ scoped: true })
    .where(and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.userId, input.userId)));
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: `Project role set (${input.role})`,
    target: `${input.projectId} · ${input.userId}`,
  });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
}

/** Remove a member's grant. The member STAYS scoped (SIGMA-167): revoking the
 *  last grant leaves them with access to nothing, matching the admin's intent
 *  to narrow — it never silently restores org-wide access. Org-wide access is
 *  restored only by the explicit restoreOrgWideAccess action. */
export async function revokeProjectRole(input: {
  orgId: string;
  projectId: string;
  userId: string;
}): Promise<void> {
  const { user } = await requireProjectRole(input.orgId, input.projectId, "Project Admin");
  await db
    .delete(s.projectMemberships)
    .where(
      and(
        eq(s.projectMemberships.projectId, input.projectId),
        eq(s.projectMemberships.userId, input.userId)
      )
    );
  await writeAudit({
    orgId: input.orgId,
    actor: user.name,
    action: "Project role revoked",
    target: `${input.projectId} · ${input.userId}`,
  });
  revalidatePath(`/dashboard/projects/${input.projectId}`);
}

/** Explicitly clear a member's project scoping, restoring their org-wide role
 *  on every project (SIGMA-167 — the ONLY path that widens; Org Admin gate
 *  because the effect is org-wide, not per-project). Their remaining grants
 *  are removed too: org-wide access and per-project narrowing don't compose. */
export async function restoreOrgWideAccess(input: {
  orgId: string;
  userId: string;
}): Promise<void> {
  const admin = await requireOrgAdmin(input.orgId);
  await db
    .update(s.memberships)
    .set({ scoped: false })
    .where(and(eq(s.memberships.orgId, input.orgId), eq(s.memberships.userId, input.userId)));
  const orgProjects = db
    .select({ id: s.projects.id })
    .from(s.projects)
    .where(eq(s.projects.orgId, input.orgId));
  await db
    .delete(s.projectMemberships)
    .where(
      and(
        eq(s.projectMemberships.userId, input.userId),
        inArray(s.projectMemberships.projectId, orgProjects)
      )
    );
  await writeAudit({
    orgId: input.orgId,
    actor: admin.name,
    action: "Org-wide access restored",
    target: input.userId,
  });
  revalidatePath("/dashboard/settings");
}
