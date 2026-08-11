import { redirect } from "next/navigation";
import {
  currentUserHasPassword,
  getActiveOrgId,
  getSessionUser,
  hasFullOrgVisibility,
} from "@/server/active-org";
import { getMembers, getOrg, getPendingInvites } from "@/server/queries";
import { getAuditLog } from "@/server/audit";
import { cpEnabled, cpListAudit, cpGetGitIntegration } from "@/server/cp";
import { getRegistry } from "@/server/actions/registry";
import { SettingsView } from "@/components/dashboard/settings/settings-view";

export default async function SettingsPage() {
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  // The org-wide audit stream carries raw resource identifiers (connection,
  // branch-map, domain, resource ids) in its `target` field, spanning every
  // project in the org. A user scoped to a subset of projects (P2-7) must not
  // read ids for projects they can't see, so the whole audit is gated on full
  // org visibility (org admins and zero-grant legacy users) — SIGMA-97.
  const canViewAudit = await hasFullOrgVisibility(orgId);

  const [
    org,
    members,
    pendingInvites,
    audit,
    sessionUser,
    cpAudit,
    gitIntegration,
    registry,
    hasPassword,
  ] = await Promise.all([
    getOrg(orgId),
    getMembers(orgId),
    getPendingInvites(orgId),
    canViewAudit ? getAuditLog(orgId) : Promise.resolve([]),
    getSessionUser(),
    // CP mode: merge the control plane's audit stream (register, tokens,
    // status flips, domain mutations) with the local web audit.
    canViewAudit && cpEnabled() ? cpListAudit(orgId).catch(() => []) : Promise.resolve([]),
      // Org-level GitHub integration. A CP failure degrades the tab to the
      // not-connected state rather than breaking the whole settings page.
      cpEnabled()
        ? cpGetGitIntegration(orgId).catch(() => null)
        : Promise.resolve(null),
      // The org's container registry — what makes an image built on one machine
      // runnable on another. Already degrades to "not configured" on failure.
      getRegistry({ orgId }),
      // Whether to offer the Password card at all — a social-only account has no
      // credential for changePassword to verify against (SIGMA-345).
      currentUserHasPassword(),
    ]);
  if (!org) redirect("/login");

  const currentUserId = sessionUser.id;
  const currentUserRole =
    members.find((m) => m.id === currentUserId)?.role ?? "Developer";

  const merged = [
    ...audit.map((a) => ({
      id: a.id,
      actor: a.actor,
      action: a.action,
      target: a.target,
      createdAt: a.createdAt,
    })),
    ...cpAudit.map((a) => ({
      id: `cp_${a.id}`,
      actor: a.actor,
      action: a.action,
      target: a.target,
      createdAt: new Date(a.createdAt),
    })),
  ]
    .sort((x, y) => y.createdAt.getTime() - x.createdAt.getTime())
    .slice(0, 50);

  return (
    <SettingsView
      org={{ id: org.id, name: org.name, slug: org.slug, plan: org.plan }}
      members={members}
      pendingInvites={pendingInvites.map((i) => ({
        id: i.id,
        email: i.email,
        role: i.role,
        invitedBy: i.invitedBy,
        expiresAt: i.expiresAt,
      }))}
      audit={merged}
      canViewAudit={canViewAudit}
      currentUserId={currentUserId}
      currentUserRole={currentUserRole}
      cpMode={cpEnabled()}
      orgCreatedAt={org.createdAt}
      twoFactorEnabled={Boolean(
        (sessionUser as { twoFactorEnabled?: boolean | null }).twoFactorEnabled
      )}
      hasPassword={hasPassword}
      gitIntegration={gitIntegration}
      registry={registry}
    />
  );
}
